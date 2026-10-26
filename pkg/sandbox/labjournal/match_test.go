package labjournal

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func add(j *Journal, kind, subject, payload string) {
	j.Append(Entry{Kind: kind, Subject: subject, Payload: []byte(payload)})
}

func TestClampWait(t *testing.T) {
	for in, want := range map[time.Duration]time.Duration{
		-time.Second: 0, 0: 0, time.Second: time.Second, 25 * time.Second: MaxWait, 120 * time.Second: MaxWait,
	} {
		if got := clampWait(in); got != want {
			t.Errorf("clampWait(%v) = %v, want %v", in, got, want)
		}
	}
	if MaxWait != 25*time.Second {
		t.Fatalf("MaxWait = %v", MaxWait)
	}
}

// A request for 120 s never holds the caller past 25 s: it is clamped before the
// wait starts, and a context that expires first ends the wait with what was seen
// instead of an error (lab.Call's own deadline is the same 25 s).
func TestWaitForBounded(t *testing.T) {
	j := openTest(t, Options{})

	// A hit returns at once, long before any timeout.
	add(j, "event.agent.error", "x", `{"event":{"n":1}}`)
	start := time.Now()
	res, err := j.WaitFor(context.Background(), Match{Kind: "event.agent.error"}, 0, 120*time.Second)
	if err != nil || !res.Matched || res.Count != 1 || time.Since(start) > 2*time.Second {
		t.Fatalf("hit: %+v %v after %v", res, err, time.Since(start))
	}

	// No hit: the wait ends at the deadline with matched=false and the cursor kept.
	start = time.Now()
	res, err = j.WaitFor(context.Background(), Match{Kind: "never"}, res.NextSeq, 300*time.Millisecond)
	if err != nil || res.Matched || res.Count != 0 || res.NextSeq != 1 {
		t.Fatalf("miss: %+v %v", res, err)
	}
	if d := time.Since(start); d < 250*time.Millisecond || d > 2*time.Second {
		t.Fatalf("miss took %v, want about 300ms", d)
	}

	// The context's deadline caps a wait that asked for far more.
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	start = time.Now()
	res, err = j.WaitFor(ctx, Match{Kind: "never"}, 0, 120*time.Second)
	if err != nil || res.Matched || time.Since(start) > 2*time.Second {
		t.Fatalf("context deadline: %+v %v after %v", res, err, time.Since(start))
	}

	// A cancelled context is an error.
	ctx, cancel = context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	if _, err = j.WaitFor(ctx, Match{Kind: "never"}, 0, 10*time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
}

// The wait wakes on the writer's commit, not on the poll: with the poll set far out, an
// entry appended mid-wait still ends it at once.
func TestWaitForWakesOnCommit(t *testing.T) {
	prev := pollEvery
	pollEvery = time.Hour
	t.Cleanup(func() { pollEvery = prev })
	j := openTest(t, Options{})

	time.AfterFunc(100*time.Millisecond, func() { add(j, "chat.out", "c1", `{"text":"hi"}`) })
	start := time.Now()
	res, err := j.WaitFor(context.Background(), Match{Kind: "chat.*"}, 0, 10*time.Second)
	if err != nil || !res.Matched || len(res.Entries) != 1 || time.Since(start) > 3*time.Second {
		t.Fatalf("%+v %v after %v", res, err, time.Since(start))
	}
}

func TestMatchFieldsAndSubject(t *testing.T) {
	j := openTest(t, Options{})
	add(j, "event.trigger.candidate", "price:BTC", `{"event":{"payload":{"symbol":"BTC/USDT","px":101.5,"tags":["a","b"]}}}`)
	add(j, "event.trigger.candidate", "price:ETH", `{"event":{"payload":{"symbol":"ETH/USDT","px":9}}}`)
	for name, tc := range map[string]struct {
		m    Match
		want int
	}{
		"substring":   {Match{Kind: "event.trigger.candidate", Fields: map[string]any{"event.payload.symbol": "BTC"}}, 1},
		"number":      {Match{Fields: map[string]any{"event.payload.px": 101.5}}, 1},
		"int literal": {Match{Fields: map[string]any{"event.payload.px": 9}}, 1},
		"array index": {Match{Fields: map[string]any{"event.payload.tags.1": "b"}}, 1},
		"absent path": {Match{Fields: map[string]any{"event.payload.nope": "x"}, Count: "0"}, 0},
		"subject":     {Match{Subject: "price:ETH"}, 1},
		"all":         {Match{Kind: "event.*", Count: ">= 2"}, 2},
	} {
		res, err := j.WaitFor(context.Background(), tc.m, 0, 100*time.Millisecond)
		if err != nil || !res.Matched || res.Count != tc.want {
			t.Errorf("%s: %+v %v, want matched with count %d", name, res, err, tc.want)
		}
	}
}

func TestMatchCountSemantics(t *testing.T) {
	j := openTest(t, Options{})
	for i := 0; i < 3; i++ {
		add(j, "k", "s", fmt.Sprintf(`{"i":%d}`, i))
	}
	wait := func(count string) (WaitResult, time.Duration) {
		start := time.Now()
		res, err := j.WaitFor(context.Background(), Match{Kind: "k", Count: count}, 0, 400*time.Millisecond)
		if err != nil {
			t.Fatalf("count %q: %v", count, err)
		}
		return res, time.Since(start)
	}
	for _, tc := range []struct {
		count   string
		matched bool
		quick   bool
	}{
		{"", true, true}, {">= 3", true, true}, {"> 2", true, true}, {"3", true, true}, {"==3", true, true},
		{">= 4", false, false}, {"4", false, false}, // may still arrive: wait out the window
		{"<= 3", true, false}, {"< 4", true, false}, // absence-style: hold only when the window passes
		{"0", false, true}, {"<= 2", false, true}, // already exceeded: end at once
	} {
		res, took := wait(tc.count)
		if res.Matched != tc.matched || (tc.quick && took > 300*time.Millisecond) || (!tc.quick && took < 300*time.Millisecond) {
			t.Errorf("count %q: matched=%v after %v, want matched=%v quick=%v", tc.count, res.Matched, took, tc.matched, tc.quick)
		}
	}
	if res, _ := wait("0"); res.Count != 3 {
		t.Errorf("count of an exceeded absence check = %d, want 3", res.Count)
	}
	// A fresh window with nothing after the cursor: "0" holds at the end.
	res, err := j.WaitFor(context.Background(), Match{Kind: "k", Count: "0"}, 3, 150*time.Millisecond)
	if err != nil || !res.Matched || res.NextSeq != 3 {
		t.Errorf("absence after the cursor: %+v %v", res, err)
	}
	for _, bad := range []string{"x", "-1", "< 0", ">= x", "1.5"} {
		if _, err := j.WaitFor(context.Background(), Match{Count: bad}, 0, 0); !errors.Is(err, ErrBadMatch) {
			t.Errorf("count %q: err = %v, want ErrBadMatch", bad, err)
		}
	}
}

// More matches than one query page: the count is exact, the entries are capped, and
// the cursor ends at the last row.
func TestWaitForPagesPastTheQueryLimit(t *testing.T) {
	j := openTest(t, Options{})
	const n = MaxLimit + 30
	for i := 0; i < n; i++ {
		add(j, "k", "s", `{}`)
	}
	res, err := j.WaitFor(context.Background(), Match{Kind: "k", Count: fmt.Sprint(n)}, 0, time.Second)
	if err != nil || !res.Matched || res.Count != n || len(res.Entries) != maxWaitEntries || res.NextSeq != n {
		t.Fatalf("count %d entries %d next %d matched %v err %v", res.Count, len(res.Entries), res.NextSeq, res.Matched, err)
	}
}

func TestQueryDescReturnsTheNewestFirst(t *testing.T) {
	j := openTest(t, Options{})
	for i := 0; i < 5; i++ {
		add(j, "k", "s", fmt.Sprintf(`{"i":%d}`, i))
	}
	got := query(t, j, Query{Desc: true, Limit: 2})
	if len(got) != 2 || got[0].Seq != 5 || got[1].Seq != 4 {
		t.Fatalf("Desc limit 2 = %+v, want seq 5, 4", got)
	}
	if got := query(t, j, Query{Limit: 2}); got[0].Seq != 1 {
		t.Fatalf("without Desc = %+v, want the oldest first", got)
	}
}
