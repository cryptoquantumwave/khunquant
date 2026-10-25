package attention

import (
	"strconv"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// decideAll decides every candidate, as the pipeline does for one event, and
// returns the decisions in order.
func (e *obEnv) decideAll(cs ...proactive.Candidate) []Decision {
	e.t.Helper()
	ds := make([]Decision, len(cs))
	for i, c := range cs {
		ds[i] = e.decideOnly(c)
	}
	return ds
}

// One event, two protective Criticals (two breach codes of one plan): AcceptAll
// takes both under one hold of the lock, so the protective worker, which is awake
// and waiting, can only ever see both or neither. The owner gets one message with
// both decisions in it, each time, and every member is marked delivered.
//
// The property is by construction (the worker takes its group under the same mutex
// AcceptAll holds for the whole batch), so this cannot fail on correct code. On a
// hand-over one decision at a time (the loop of Accept calls this replaced) it fails
// only when the worker wins the race, which takes CPU contention: it was seen to
// split under -race -cpu 1,2,8, not on every run. TestAcceptAll_ProtectiveBatchIsOneGroup
// and the engine's dispatcher test are the deterministic halves of this contract.
func TestAcceptAll_OneEventOneMessage(t *testing.T) {
	e := newObEnv(t)
	e.startLanes()

	const events = 300
	var all []Decision
	for i := 0; i < events; i++ {
		k := strconv.Itoa(i)
		ds := e.decideAll(
			cand("margin:"+k, critical, protective),
			cand("liq:"+k, critical, protective),
		)
		all = append(all, ds...)
		for j, h := range e.ob.AcceptAll(ds) {
			if h != HandoffQueued {
				t.Fatalf("event %d decision %d: AcceptAll = %s, want queued", i, j, h)
			}
		}
		// The worker is idle and awake again before the next event, which is the
		// moment a one-at-a-time hand-over loses the race.
		waitUntil(t, "the event's message", func() bool { return len(e.del.messages()) == i+1 })
	}
	e.waitIdle()

	for i, m := range e.del.messages() {
		if len(m.Decisions) != 2 {
			t.Fatalf("message %d carries %d decisions, want the 2 of one event: %+v", i, len(m.Decisions), m)
		}
		a, b := m.Decisions[0].Candidate.DedupKey, m.Decisions[1].Candidate.DedupKey
		if a[len("margin:"):] != b[len("liq:"):] {
			t.Fatalf("message %d mixes events: %s and %s", i, a, b)
		}
	}
	for _, d := range all {
		if r := e.rowOf(d.ID); r.delivered != 1 {
			t.Fatalf("decision %s: row %+v, want delivered", d.ID, r)
		}
	}
	if st := e.ob.Stats(); st.ProtectiveHandOvers != 2*events || st.Superseded != 0 {
		t.Fatalf("stats = %+v", st)
	}
}

// With no worker at all the batch is still one group: both decisions wait on the
// protective lane together, and one take returns both.
func TestAcceptAll_ProtectiveBatchIsOneGroup(t *testing.T) {
	e := newObEnv(t)
	ds := e.decideAll(cand("a", critical, protective), cand("b", critical, protective))
	hs := e.ob.AcceptAll(ds)
	if len(hs) != 2 || hs[0] != HandoffQueued || hs[1] != HandoffQueued {
		t.Fatalf("AcceptAll = %v", hs)
	}
	if st := e.ob.Stats(); st.ProtectivePending != 2 {
		t.Fatalf("stats = %+v, want both pending", st)
	}
	g := e.ob.takeProtective()
	if len(g) != 2 || g[0].dec.ID != ds[0].ID || g[1].dec.ID != ds[1].ID {
		t.Fatalf("one take returned %d members, want the 2 of the batch in order", len(g))
	}
}

// AcceptAll returns for each decision what Accept returns, and the ordinary rules
// are Accept's: a decision that is not a message is ignored, a Notice waits for its
// window, a protective Critical takes the buffered Notice along with it, and a Notice
// after it opens a fresh window.
func TestAcceptAll_EachDecisionGetsAcceptsAnswer(t *testing.T) {
	e := newObEnv(t)
	e.tick() // prime the digest
	ds := e.decideAll(
		cand("i", info),
		cand("n", notice),
		cand("liq", critical, protective),
		cand("n2", notice),
	)
	if ds[0].Disposition != proactive.DispDigest {
		t.Fatalf("an info decision is %s, want digest", ds[0].Disposition)
	}
	want := []Handoff{HandoffIgnored, HandoffBuffered, HandoffQueued, HandoffBuffered}
	got := e.ob.AcceptAll(ds)
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("AcceptAll = %v, want %v", got, want)
		}
	}
	if st := e.ob.Stats(); st.ProtectivePending != 2 {
		t.Fatalf("stats = %+v, want the Notice and the Critical on the protective lane", st)
	}
	e.ob.mu.Lock()
	held := len(e.ob.buf)
	e.ob.mu.Unlock()
	if held != 1 {
		t.Fatalf("%d decisions still buffered, want the one Notice that came after", held)
	}
	if got := e.ob.AcceptAll(nil); len(got) != 0 {
		t.Fatalf("AcceptAll(nil) = %v", got)
	}
}

// After the stop AcceptAll does nothing, like Accept, and counts what it refused.
func TestAcceptAll_AfterStopRejectsTheMessages(t *testing.T) {
	e := newObEnv(t)
	stop := e.startLanes()
	e.tick() // prime the digest
	stop()

	ds := e.decideAll(cand("i", info), cand("late", critical, protective), cand("late2", notice))
	got := e.ob.AcceptAll(ds)
	want := []Handoff{HandoffIgnored, HandoffClosed, HandoffClosed}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("AcceptAll after stop = %v, want %v", got, want)
		}
	}
	if n := len(e.del.messages()); n != 0 {
		t.Fatalf("a late AcceptAll delivered %d messages", n)
	}
	if st := e.ob.Stats(); st.RejectedClosed != 2 {
		t.Fatalf("stats = %+v", st)
	}
}
