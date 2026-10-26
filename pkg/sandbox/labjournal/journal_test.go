package labjournal

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func openTest(t *testing.T, o Options) *Journal {
	t.Helper()
	if o.Path == "" {
		o.Path = filepath.Join(t.TempDir(), "lab", "journal.db")
	}
	j, err := Open(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = j.Close() })
	return j
}

func query(t *testing.T, j *Journal, q Query) []Entry {
	t.Helper()
	got, err := j.Query(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// stall holds the database's write lock so the writer cannot commit, and returns the
// release function.
func stall(t *testing.T, j *Journal) func() {
	t.Helper()
	conn, err := j.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	return func() {
		_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		_ = conn.Close()
	}
}

func TestAppendNeverBlocks(t *testing.T) {
	j := openTest(t, Options{Queue: 64})
	release := stall(t, j)
	defer release()

	const n = 100_000
	start := time.Now()
	for i := 0; i < n; i++ {
		j.Append(Entry{Kind: "event.test"})
	}
	// The plan's bound is 50 ms on an idle machine; the property is that Append does
	// not wait for the stalled writer (which would take seconds), so the bound here
	// has headroom for a loaded one.
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("%d appends took %s with a stalled writer", n, d)
	}
	// At most the queue (64) plus one batch (256) in the writer's hands are not dropped.
	if got := j.Drops(); got < n-64-batchMax-1 || got > n {
		t.Fatalf("Drops() = %d, want about %d", got, n)
	}
}

func TestQueryReadsOwnWrites(t *testing.T) {
	j := openTest(t, Options{})
	for i := 0; i < 50; i++ {
		j.Append(Entry{Kind: "event.x", Subject: "s", Payload: json.RawMessage(`{"i":1}`)})
		if got := query(t, j, Query{Kinds: []string{"event.x"}, Limit: 1000}); len(got) != i+1 {
			t.Fatalf("after %d appends Query returned %d entries", i+1, len(got))
		}
	}
}

func TestQueryKindPrefixAndAfterSeq(t *testing.T) {
	j := openTest(t, Options{})
	for _, k := range []string{"chat.in", "chat.out", "chatter.x", "event.chat.in", "chat_out", "tool.call"} {
		j.Append(Entry{Kind: k, Subject: k})
	}
	kinds := func(q Query) []string {
		var out []string
		for _, e := range query(t, j, q) {
			out = append(out, e.Kind)
		}
		return out
	}
	if got := kinds(Query{Kinds: []string{"chat.*"}}); len(got) != 2 || got[0] != "chat.in" || got[1] != "chat.out" {
		t.Errorf("chat.* = %v", got)
	}
	if got := kinds(Query{Kinds: []string{"chat.in", "tool.call"}}); len(got) != 2 {
		t.Errorf("exact kinds = %v", got)
	}
	all := query(t, j, Query{})
	if len(all) != 6 {
		t.Fatalf("all = %d entries", len(all))
	}
	if got := query(t, j, Query{AfterSeq: all[2].Seq}); len(got) != 3 || got[0].Seq != all[3].Seq {
		t.Errorf("AfterSeq cursor returned %d entries", len(got))
	}
	if got := query(t, j, Query{Limit: 2}); len(got) != 2 || got[0].Seq != all[0].Seq || got[1].Seq != all[1].Seq {
		t.Errorf("limit 2 = %+v, want the oldest two in Seq order", got)
	}
	if got := query(t, j, Query{Subject: "tool.call"}); len(got) != 1 {
		t.Errorf("subject filter = %d entries", len(got))
	}
}

func TestSimTimeAndPayloadStamping(t *testing.T) {
	sim := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	j := openTest(t, Options{Now: func() time.Time { return sim }})
	j.Append(Entry{Kind: "a", Payload: json.RawMessage(`not json`)})
	j.Append(Entry{Kind: "b"})
	got := query(t, j, Query{})
	if len(got) != 2 || !got[0].SimTime.Equal(sim) || got[0].WallTime.IsZero() {
		t.Fatalf("entries = %+v", got)
	}
	if !json.Valid(got[0].Payload) || string(got[1].Payload) != "{}" {
		t.Errorf("payloads = %s, %s", got[0].Payload, got[1].Payload)
	}
	if got := query(t, j, Query{Since: sim.Add(time.Second)}); len(got) != 0 {
		t.Errorf("Since after the sim time still matched %d entries", len(got))
	}
	if got := query(t, j, Query{Until: sim.Add(time.Second)}); len(got) != 2 {
		t.Errorf("Until after the sim time matched %d entries, want 2", len(got))
	}
}

func TestReopenContinuesSeqAndCloseIsSafe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lab", "journal.db")
	j, err := Open(Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	j.Append(Entry{Kind: "a"})
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	j.Append(Entry{Kind: "late"}) // after Close: lost, but no panic and no wait
	if j.Close() != nil || j.Flush(context.Background()) != nil {
		t.Error("Close/Flush on a closed journal failed")
	}
	j2 := openTest(t, Options{Path: path})
	j2.Append(Entry{Kind: "b"})
	got := query(t, j2, Query{})
	if len(got) != 2 || got[1].Seq <= got[0].Seq {
		t.Fatalf("after reopen: %+v", got)
	}
}

func TestDropSourcesAddToDrops(t *testing.T) {
	j := openTest(t, Options{})
	j.AddDropSource(func() uint64 { return 7 })
	if j.Drops() != 7 {
		t.Errorf("Drops() = %d, want 7", j.Drops())
	}
}
