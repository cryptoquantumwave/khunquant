package engine

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/attention"
	"github.com/cryptoquantumwave/khunquant/pkg/attention/attentiontest"
	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/perceptiontest"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// commitEval is an Evaluator that is also a Committer: it returns the candidates a
// test scripts for each event (by its number) and records what it is told was
// decided.
type commitEval struct {
	mu        sync.Mutex
	cands     map[int][]proactive.Candidate
	err       error
	panicOn   int
	committed [][]string
}

func (c *commitEval) Evaluate(_ context.Context, _ View, d Diff) ([]proactive.Candidate, error) {
	if c.err != nil {
		return nil, c.err
	}
	return c.cands[d.(int)], nil
}

func (c *commitEval) Committed(_ context.Context, ids []string) {
	c.mu.Lock()
	c.committed = append(c.committed, append([]string(nil), ids...))
	n := len(c.committed)
	c.mu.Unlock()
	if c.panicOn == n {
		panic("boom in Committed")
	}
}

func (c *commitEval) calls() [][]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([][]string(nil), c.committed...)
}

func withID(id, key string) proactive.Candidate {
	c := candidate(key)
	c.ID = id
	return c
}

func runCommit(t *testing.T, ev *commitEval, at Attention, n int) *started {
	t.Helper()
	s := start(t, Options{
		WorldModel: &recorder{}, Evaluator: ev, Attention: at, Clock: perceptiontest.NewClock(noon),
	})
	for i := 0; i < n; i++ {
		s.Emitter().Emit(tickEvent("BTC/USDT", i))
	}
	waitFor(t, "the events", func() bool { return s.Stats().Processed == uint64(n) })
	return s
}

// The committer is told exactly the candidates attention decided: one whose Decide
// returned an error, and one whose Decide panicked, are not among them.
func TestEngine_CommitterGetsOnlyDecidedCandidates(t *testing.T) {
	ev := &commitEval{cands: map[int][]proactive.Candidate{
		0: {withID("ok1", "k1"), withID("bad", "kbad"), withID("boom", "kboom"), withID("ok2", "k2")},
	}}
	at := &attentiontest.Fake{DecideFn: func(_ context.Context, c proactive.Candidate) (attention.Decision, error) {
		switch c.ID {
		case "bad":
			return attention.Decision{}, errors.New("database is locked")
		case "boom":
			panic("governor exploded")
		}
		return attention.Decision{ID: proactive.NewID(), Disposition: proactive.DispLog, Candidate: c}, nil
	}}
	s := runCommit(t, ev, at, 1)

	if got, want := ev.calls(), [][]string{{"ok1", "ok2"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("committed = %v, want %v", got, want)
	}
	if st := s.Stats(); st.StagePanics != 1 || st.StageErrors != 1 || st.Decisions != 2 {
		t.Fatalf("stats = %+v", st)
	}
}

// Committed is called for an event that produced no candidate, so that a state
// change that fired nothing can settle.
func TestEngine_CommitterIsCalledForEveryEvaluatedEvent(t *testing.T) {
	ev := &commitEval{cands: map[int][]proactive.Candidate{1: {withID("c", "k")}}}
	runCommit(t, ev, &attentiontest.Fake{}, 3)
	if got, want := ev.calls(), [][]string{nil, {"c"}, nil}; !reflect.DeepEqual(got, want) {
		t.Fatalf("committed = %v, want %v", got, want)
	}
}

// Without an attention stage nothing records a decision, so nothing is decided.
func TestEngine_CommitterWithoutAttentionDecidesNothing(t *testing.T) {
	ev := &commitEval{cands: map[int][]proactive.Candidate{0: {withID("c", "k")}}}
	s := runCommit(t, ev, nil, 1)
	if got, want := ev.calls(), [][]string{nil}; !reflect.DeepEqual(got, want) {
		t.Fatalf("committed = %v, want %v", got, want)
	}
	if st := s.Stats(); st.Candidates != 1 || st.Decisions != 0 {
		t.Fatalf("stats = %+v", st)
	}
}

// A failed Evaluate is not committed, and a panic in Committed costs the event, not
// the pipeline.
func TestEngine_CommitterNotCalledAfterFailedEvaluate(t *testing.T) {
	ev := &commitEval{err: errors.New("no")}
	s := runCommit(t, ev, &attentiontest.Fake{}, 2)
	if len(ev.calls()) != 0 {
		t.Fatalf("committed after a failed evaluation: %v", ev.calls())
	}
	if st := s.Stats(); st.StageErrors != 2 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestEngine_CommitterPanicIsRecovered(t *testing.T) {
	ev := &commitEval{cands: map[int][]proactive.Candidate{}, panicOn: 1}
	s := runCommit(t, ev, &attentiontest.Fake{}, 3)
	if len(ev.calls()) != 3 {
		t.Fatalf("calls = %v, want 3", ev.calls())
	}
	if st := s.Stats(); st.StagePanics != 1 || st.Processed != 3 {
		t.Fatalf("stats = %+v", st)
	}
}

// An evaluator that is not a Committer is used as before.
func TestEngine_EvaluatorWithoutCommitterIsUnchanged(t *testing.T) {
	rec := &recorder{}
	s := start(t, Options{
		WorldModel: rec, Evaluator: rec, Attention: &attentiontest.Fake{}, Dispatcher: rec,
		Clock: perceptiontest.NewClock(noon),
	})
	s.Emitter().Emit(events.Event{
		ID: "a", Kind: events.KindPerceptionMarketTick, Attrs: map[string]any{"n": 7},
		Trust: events.TrustPtr(proactive.TrustExchange),
	})
	waitFor(t, "the dispatch", func() bool { return len(rec.dispatches()) == 1 })
}
