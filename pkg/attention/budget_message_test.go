package attention

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

func TestBudget_GroupOfFiveCostsOne(t *testing.T) {
	e := newObEnv(t) // budget 3, 60 s window
	t0 := e.clock.Now()
	for i := 0; i < 5; i++ {
		e.clock.Set(t0.Add(time.Duration(i) * 5 * time.Second))
		expect(t, e.submit(cand("g1-"+strconv.Itoa(i), notice)), proactive.DispNotify, ReasonTemplate)
	}
	e.tickAt(t0.Add(time.Minute))
	if n := len(e.del.messages()); n != 1 {
		t.Fatalf("%d messages", n)
	}
	// one message used: two more are allowed, the third is not.
	for i, wantDisp := range []proactive.Disposition{proactive.DispNotify, proactive.DispNotify, proactive.DispDigest} {
		e.clock.Add(2 * time.Minute)
		d := e.submit(cand("later-"+strconv.Itoa(i), notice))
		if d.Disposition != wantDisp {
			t.Fatalf("later %d = %s/%s, want %s", i, d.Disposition, d.Reason, wantDisp)
		}
		e.tickAt(e.clock.Now().Add(time.Minute))
	}
	if n := len(e.del.messages()); n != 3 {
		t.Fatalf("%d messages, want 3", n)
	}
}

func TestBudget_TwoGroupsCostTwo(t *testing.T) {
	e := newObEnv(t)
	t0 := e.clock.Now()
	for g := 0; g < 2; g++ {
		e.clock.Set(t0.Add(time.Duration(g) * 5 * time.Minute))
		for i := 0; i < 4; i++ {
			expect(t, e.submit(cand("g"+strconv.Itoa(g)+"-"+strconv.Itoa(i), notice)), proactive.DispNotify, ReasonTemplate)
		}
		e.tickAt(e.clock.Now().Add(time.Minute))
	}
	if n := len(e.del.messages()); n != 2 {
		t.Fatalf("%d messages, want 2", n)
	}
	e.clock.Set(t0.Add(15 * time.Minute))
	expect(t, e.submit(cand("third", notice)), proactive.DispNotify, ReasonTemplate) // message 3
	e.tickAt(e.clock.Now().Add(time.Minute))
	e.clock.Add(5 * time.Minute)
	expect(t, e.submit(cand("fourth", notice)), proactive.DispDigest, ReasonBudget)

	got, err := sendsTodayFor(t, e.env)
	if err != nil || got != 3 {
		t.Fatalf("sends today = %d, %v; want 3 messages (not 9 decisions)", got, err)
	}
}

// sendsTodayFor evaluates the budget count the way Decide does.
func sendsTodayFor(t *testing.T, e *env) (int, error) {
	t.Helper()
	now := e.clock.Now()
	pf := e.g.loadProfile(context.Background(), now)
	tx, err := e.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	n, _, err := sendsToday(context.Background(), tx, pf.dayStartMs, pf.dayEndMs,
		now.Add(-pendingLease).UnixMilli(), e.g.groupWindow().Milliseconds(), now.UnixMilli())
	return n, err
}

func TestBudget_InFlightGroupIsOnePendingMessage(t *testing.T) {
	e := newObEnv(t)
	t0 := e.clock.Now()
	// five decisions inside one window reserve one slot between them, so the
	// budget leaves room for two more messages.
	for i := 0; i < 5; i++ {
		e.clock.Set(t0.Add(time.Duration(i) * time.Second))
		expect(t, e.submit(cand("a"+strconv.Itoa(i), notice)), proactive.DispNotify, ReasonTemplate)
	}
	if n, err := sendsTodayFor(t, e.env); err != nil || n != 1 {
		t.Fatalf("pending messages = %d, %v; want 1", n, err)
	}
	// a decision after the window has closed is another message.
	e.clock.Set(t0.Add(61 * time.Second))
	expect(t, e.submit(cand("b", notice)), proactive.DispNotify, ReasonTemplate)
	if n, _ := sendsTodayFor(t, e.env); n != 2 {
		t.Fatalf("pending messages = %d, want 2", n)
	}
}

func TestBudget_JoiningAnOpenMessageIsNotHeldToTheBudget(t *testing.T) {
	e := newObEnv(t) // budget 3
	e.prof.set(func(p *intent.Profile) { p.NotificationBudgetPerDay = 1 })
	t0 := e.clock.Now()
	expect(t, e.submit(cand("first", notice)), proactive.DispNotify, ReasonTemplate) // takes the only slot
	e.clock.Set(t0.Add(30 * time.Second))
	expect(t, e.submit(cand("second", notice)), proactive.DispNotify, ReasonTemplate) // joins it
	e.clock.Set(t0.Add(61 * time.Second))
	e.tick()
	if m := e.del.messages(); len(m) != 1 || len(m[0].Decisions) != 2 {
		t.Fatalf("messages = %+v", m)
	}
	// the window is closed and the message delivered: the budget is spent.
	expect(t, e.submit(cand("third", notice)), proactive.DispDigest, ReasonBudget)
}

func TestBudget_NoWindowEveryDecisionIsAMessage(t *testing.T) {
	cfg := liveCfg() // no group window
	e := newEnvMode(t, "live", cfg)
	for i := 0; i < 3; i++ {
		expect(t, e.decide(cand("k"+strconv.Itoa(i), notice)), proactive.DispNotify, ReasonTemplate)
	}
	expect(t, e.decide(cand("k3", notice)), proactive.DispDigest, ReasonBudget)
}

func TestBudget_ConcurrentDecidesCannotBothTakeTheLastSlot(t *testing.T) {
	e := newEnv(t) // budget 3, no window
	e.send(cand("one", notice))
	e.send(cand("two", notice)) // one slot left
	const n = 20
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		pass int
	)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			d, err := e.g.Decide(context.Background(), cand("c"+strconv.Itoa(i), notice))
			if err != nil {
				t.Errorf("Decide: %v", err)
				return
			}
			if d.Disposition == proactive.DispNotify {
				mu.Lock()
				pass++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()
	if pass != 1 {
		t.Fatalf("%d concurrent decisions passed with one slot left, want 1", pass)
	}
}

func TestMarkDelivered_StoresInterventionID(t *testing.T) {
	e := newEnv(t)
	a, b, c := e.decide(cand("a", notice)), e.decide(cand("b", notice)), e.decide(cand("c", notice))
	ctx := context.Background()
	shared := "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	for _, d := range []Decision{a, b} {
		if err := e.g.MarkDelivered(ctx, d, Delivery{Delivered: true, InterventionID: shared}); err != nil {
			t.Fatal(err)
		}
	}
	e.mark(c, true) // no ID: the decision is a message of its own
	if r := e.rowOf(a.ID); r.intervention != shared || r.delivered != 1 {
		t.Fatalf("a = %+v", r)
	}
	if r := e.rowOf(b.ID); r.intervention != shared {
		t.Fatalf("b = %+v", r)
	}
	if r := e.rowOf(c.ID); r.intervention != c.ID {
		t.Fatalf("c = %+v, want its own id", r)
	}
	// repeating changes nothing, even with another ID.
	if err := e.g.MarkDelivered(ctx, a, Delivery{Delivered: true, InterventionID: "OTHER"}); err != nil {
		t.Fatal(err)
	}
	if r := e.rowOf(a.ID); r.intervention != shared {
		t.Fatalf("a after repeat = %+v", r)
	}
	// a failure stores no intervention.
	d := e.decide(cand("d", notice))
	e.mark(d, false)
	if r := e.rowOf(d.ID); r.intervention != "" || r.delivered != 0 {
		t.Fatalf("failed row = %+v", r)
	}
	if n, _ := sendsTodayFor(t, e); n != 2 {
		t.Fatalf("sends today = %d, want 2 (shared + own)", n)
	}
}

func TestPendingMessages(t *testing.T) {
	cases := []struct {
		name   string
		at     []int64
		window int64
		n      int
		last   int64
	}{
		{"none", nil, 60000, 0, 0},
		{"one", []int64{5}, 60000, 1, 5},
		{"same window", []int64{0, 10000, 59999}, 60000, 1, 0},
		{"window is not extended", []int64{0, 59000, 61000}, 60000, 2, 61000},
		{"exactly a window apart opens a new one", []int64{0, 60000}, 60000, 2, 60000},
		{"no window", []int64{0, 1, 2}, 0, 3, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n, last := pendingMessages(c.at, c.window)
			if n != c.n || last != c.last {
				t.Fatalf("pendingMessages = %d, %d; want %d, %d", n, last, c.n, c.last)
			}
		})
	}
}
