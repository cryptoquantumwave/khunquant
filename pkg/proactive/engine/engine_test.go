package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/attention"
	"github.com/cryptoquantumwave/khunquant/pkg/attention/attentiontest"
	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/perceptiontest"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

func TestEngine_ProcessesInOrder(t *testing.T) {
	rec := &recorder{}
	bus := events.NewBus()
	t.Cleanup(func() { _ = bus.Close() })
	_, ch, err := bus.Channel().SubscribeChan(context.Background(), events.SubscribeOptions{Buffer: 100000})
	if err != nil {
		t.Fatal(err)
	}
	clock := perceptiontest.NewClock(noon)
	e := New(Options{
		WorldModel: rec, Evaluator: rec, Attention: &attentiontest.Fake{}, Dispatcher: rec,
		Bus: bus, Clock: clock,
	})
	em := e.Emitter()
	const n = 100
	for i := 0; i < n; i++ {
		em.Emit(tickEvent("BTC/USDT", i)) // before Run: the inbox holds them
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()
	waitFor(t, "all events", func() bool { return e.Stats().Processed == n })
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	got := rec.appliedCopy()
	for i := 0; i < n; i++ {
		if got[i] != i {
			t.Fatalf("event %d was applied at position %d", got[i], i)
		}
	}
	ds := rec.dispatches()
	if len(ds) != n {
		t.Fatalf("%d dispatches, want %d", len(ds), n)
	}
	for i, d := range ds {
		if len(d) != 1 || d[0].Candidate.DedupKey != fmt.Sprintf("k%d", i) {
			t.Fatalf("dispatch %d = %+v", i, d)
		}
	}

	// On the bus, per event: the event itself, its diff, its candidate, its decision.
	want := []events.Kind{
		events.KindPerceptionMarketTick, events.KindWorldModelDiff, events.KindTriggerCandidate, events.KindAttentionDecision,
	}
	var kinds []events.Kind
	for len(ch) > 0 {
		kinds = append(kinds, (<-ch).Kind)
	}
	if len(kinds) != n*len(want) {
		t.Fatalf("%d bus events, want %d", len(kinds), n*len(want))
	}
	for i, k := range kinds {
		if k != want[i%len(want)] {
			t.Fatalf("bus event %d is %s, want %s", i, k, want[i%len(want)])
		}
	}
}

func TestEngine_CoalescesTicks(t *testing.T) {
	rec := &recorder{}
	e := New(Options{WorldModel: rec, Clock: perceptiontest.NewClock(noon)})
	em := e.Emitter()
	subjects := []string{"A", "B", "C"}
	const ticks = 3000
	for i := 0; i < ticks; i++ {
		em.EmitLatest(tickEvent(subjects[i%3], i))
	}
	if d := e.Stats().QueueDepth; d != 3 {
		t.Fatalf("queue depth = %d, want one waiting tick per subject", d)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()
	waitFor(t, "the ticks", func() bool { return e.Stats().Processed == 3 })
	cancel()
	<-done

	got := rec.appliedCopy()
	if len(got) != 3 || got[0] != ticks-3 || got[1] != ticks-2 || got[2] != ticks-1 {
		t.Fatalf("applied %v, want only the latest tick of each subject", got)
	}
	if st := e.Stats(); st.Coalesced != ticks-3 {
		t.Fatalf("coalesced = %d, want %d", st.Coalesced, ticks-3)
	}
}

func TestEngine_InterleavesTicksAndBlockingEvents(t *testing.T) {
	rec := &recorder{}
	e := New(Options{WorldModel: rec, Clock: perceptiontest.NewClock(noon)})
	em := e.Emitter()
	for i := 0; i < 6; i++ {
		em.Emit(tickEvent("fill", 100+i))
		em.EmitLatest(tickEvent(fmt.Sprintf("S%d", i), 200+i))
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()
	waitFor(t, "all", func() bool { return e.Stats().Processed == 12 })
	cancel()
	<-done
	got := rec.appliedCopy()
	for i, n := range got {
		blocking := n < 200
		if blocking != (i%2 == 0) {
			t.Fatalf("order %v: not alternating at %d", got, i)
		}
	}
}

func TestEngine_NilStagesNoop(t *testing.T) {
	bus := events.NewBus()
	t.Cleanup(func() { _ = bus.Close() })
	_, ch, _ := bus.Channel().SubscribeChan(context.Background(), events.SubscribeOptions{Buffer: 100})
	s := start(t, Options{Bus: bus, Clock: perceptiontest.NewClock(noon)})
	em := s.Emitter()
	em.Emit(tickEvent("X", 1))
	em.EmitLatest(tickEvent("Y", 2))
	em.EmitNonBlocking(tickEvent("Z", 3))
	waitFor(t, "three events", func() bool { return s.Stats().Processed == 3 })
	if st := s.Stats(); st.Candidates != 0 || st.Decisions != 0 || st.StagePanics != 0 || st.StageErrors != 0 {
		t.Fatalf("stats = %+v", st)
	}
	if len(ch) != 3 {
		t.Fatalf("%d bus events, want the three perception events only", len(ch))
	}

	// each stage alone does nothing without its predecessor's output.
	rec := &recorder{}
	s2 := start(t, Options{Dispatcher: rec, Clock: perceptiontest.NewClock(noon)})
	s2.Emitter().Emit(tickEvent("X", 1))
	waitFor(t, "the event", func() bool { return s2.Stats().Processed == 1 })
	if len(rec.dispatches()) != 0 {
		t.Fatal("a dispatcher with no decisions was called")
	}
}

func TestEngine_TrustOnlyGoesDown(t *testing.T) {
	cases := []struct {
		name  string
		trust *proactive.Trust
		cand  proactive.Trust
		want  proactive.Trust
	}{
		{"unset trust is untrusted", nil, proactive.TrustSystem, proactive.TrustUntrusted},
		{"external keeps low trust", events.TrustPtr(proactive.TrustUntrusted), proactive.TrustUserAuthored, proactive.TrustUntrusted},
		{"exchange caps a system candidate", events.TrustPtr(proactive.TrustExchange), proactive.TrustSystem, proactive.TrustExchange},
		{"a lower candidate stays lower", events.TrustPtr(proactive.TrustUserAuthored), proactive.TrustExchange, proactive.TrustExchange},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := &recorder{}
			ev := &trustEval{trust: c.cand}
			s := start(t, Options{
				WorldModel: rec, Evaluator: ev, Attention: &attentiontest.Fake{}, Dispatcher: rec,
				Clock: perceptiontest.NewClock(noon),
			})
			in := tickEvent("X", 1)
			in.Trust = c.trust
			s.Emitter().Emit(in)
			waitFor(t, "the decision", func() bool { return len(rec.dispatches()) == 1 })
			if got := rec.dispatches()[0][0].Candidate.Trust; got != c.want {
				t.Fatalf("candidate trust = %s, want %s", got, c.want)
			}
		})
	}
}

type trustEval struct{ trust proactive.Trust }

func (e *trustEval) Evaluate(context.Context, View, Diff) ([]proactive.Candidate, error) {
	c := candidate("trust")
	c.Trust = e.trust
	return []proactive.Candidate{c}, nil
}

func TestEngine_PanickingStageDropsOnlyThatEvent(t *testing.T) {
	for _, stage := range []string{"worldmodel", "evaluator", "attention", "dispatcher"} {
		t.Run(stage, func(t *testing.T) {
			rec := &recorder{}
			p := &panicky{rec: rec, stage: stage, on: 2}
			s := start(t, Options{
				WorldModel: p, Evaluator: p, Attention: p, Dispatcher: p, Clock: perceptiontest.NewClock(noon),
			})
			for i := 1; i <= 4; i++ {
				s.Emitter().Emit(tickEvent("X", i))
			}
			waitFor(t, "four events", func() bool { return s.Stats().Processed == 4 })
			if st := s.Stats(); st.StagePanics != 1 {
				t.Fatalf("stats = %+v, want one recovered panic", st)
			}
			// the other three events went all the way to the dispatcher (or as far
			// as the panicking stage allowed).
			if stage != "dispatcher" {
				if n := len(rec.dispatches()); n != 3 {
					t.Fatalf("%d dispatches, want 3", n)
				}
			}
		})
	}
}

// panicky wraps the recorder and panics in one stage on the on-th event.
type panicky struct {
	rec   *recorder
	stage string
	on    int
	seen  map[string]int
}

func (p *panicky) hit(stage string, n int) {
	if p.stage == stage && n == p.on {
		panic("boom in " + stage)
	}
}

func (p *panicky) Apply(ctx context.Context, ev events.Event) (Diff, error) {
	p.hit("worldmodel", nOf(ev))
	return p.rec.Apply(ctx, ev)
}
func (p *panicky) View() View { return "v" }
func (p *panicky) Evaluate(ctx context.Context, v View, d Diff) ([]proactive.Candidate, error) {
	p.hit("evaluator", d.(int))
	return p.rec.Evaluate(ctx, v, d)
}
func (p *panicky) Decide(_ context.Context, c proactive.Candidate) (attention.Decision, error) {
	var n int
	_, _ = fmt.Sscanf(c.DedupKey, "k%d", &n)
	p.hit("attention", n)
	return attention.Decision{ID: proactive.NewID(), Disposition: proactive.DispNotify, Candidate: c}, nil
}
func (p *panicky) Dispatch(ctx context.Context, ds []attention.Decision) error {
	var n int
	_, _ = fmt.Sscanf(ds[0].Candidate.DedupKey, "k%d", &n)
	p.hit("dispatcher", n)
	return p.rec.Dispatch(ctx, ds)
}

func TestEngine_StageErrorsAreCountedNotFatal(t *testing.T) {
	rec := &recorder{}
	s := start(t, Options{
		WorldModel: errWM{}, Evaluator: rec, Clock: perceptiontest.NewClock(noon),
	})
	for i := 0; i < 5; i++ {
		s.Emitter().Emit(tickEvent("X", i))
	}
	waitFor(t, "five events", func() bool { return s.Stats().Processed == 5 })
	if st := s.Stats(); st.StageErrors != 5 || st.Candidates != 0 {
		t.Fatalf("stats = %+v", st)
	}
}

type errWM struct{}

func (errWM) Apply(context.Context, events.Event) (Diff, error) { return nil, errors.New("bad event") }
func (errWM) View() View                                        { return nil }

// A subscriber that never returns, on a bus that blocks its publishers, must not
// slow the pipeline: the engine only ever publishes without waiting.
func TestEngine_NeverWaitsOnAStuckBusSubscriber(t *testing.T) {
	bus := events.NewBus()
	stuck := make(chan struct{})
	t.Cleanup(func() { close(stuck); _ = bus.Close() })
	_, err := bus.Channel().Subscribe(context.Background(), events.SubscribeOptions{
		Name: "stuck", Buffer: 1, Backpressure: events.Block, Concurrency: events.Locked,
	}, func(ctx context.Context, _ events.Event) error {
		<-stuck
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	s := start(t, Options{
		WorldModel: rec, Evaluator: rec, Attention: &attentiontest.Fake{}, Dispatcher: rec,
		Bus: bus, Clock: perceptiontest.NewClock(noon),
	})
	const n = 200
	for i := 0; i < n; i++ {
		s.Emitter().Emit(tickEvent("X", i))
	}
	waitFor(t, "all events despite the stuck subscriber", func() bool { return s.Stats().Processed == n })
	if st := s.Stats(); st.BusDropped == 0 {
		t.Fatalf("stats = %+v: the full subscriber should have refused events, and the engine counted it", st)
	}
}

func TestEngine_HousekeepingRunsEverySecondEvenUnderLoad(t *testing.T) {
	clock := perceptiontest.NewClock(noon)
	rec := &recorder{}
	start(t, Options{Dispatcher: rec, Clock: clock})
	if !clock.WaitForTimers(1, 5*time.Second) {
		t.Fatal("no housekeeping timer")
	}
	clock.Advance(time.Second)
	waitFor(t, "a housekeeping call", func() bool { rec.mu.Lock(); defer rec.mu.Unlock(); return len(rec.house) == 1 })
	rec.mu.Lock()
	got := rec.house[0]
	rec.mu.Unlock()
	if !got.Equal(noon.Add(time.Second)) {
		t.Fatalf("Housekeep(%v)", got)
	}

	// under load: the stage advances the clock while events are queued, and the
	// next housekeeping happens between events, not after the queue empties.
	clock2 := perceptiontest.NewClock(noon)
	rec2 := &recorder{}
	adv := &advancer{clock: clock2, after: 1}
	e2 := New(Options{Evaluator: adv, Dispatcher: rec2, Clock: clock2})
	for i := 0; i < 5; i++ {
		e2.Emitter().Emit(tickEvent("X", i)) // all queued before the pipeline starts
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e2.Run(ctx) }()
	waitFor(t, "five events", func() bool { return e2.Stats().Processed == 5 })
	cancel()
	<-done
	rec2.mu.Lock()
	n := len(rec2.house)
	rec2.mu.Unlock()
	if n != 1 {
		t.Fatalf("housekeeping calls = %d, want 1: the clock moved during the flood and the pipeline noticed between events", n)
	}
}

type advancer struct {
	clock *perceptiontest.Clock
	after int
	n     int
}

func (a *advancer) Evaluate(context.Context, View, Diff) ([]proactive.Candidate, error) {
	a.n++
	if a.n == a.after {
		a.clock.Advance(time.Second)
	}
	return nil, nil
}

func TestEngine_FloodIsBounded(t *testing.T) {
	const subjects = 100
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	stall := &stallEval{gate: gate, entered: entered}
	s := start(t, Options{Evaluator: stall, Clock: perceptiontest.NewClock(noon)})
	em := s.Emitter()

	// the pipeline is stuck on its first event while everything below arrives.
	em.Emit(tickEvent("first", 0))
	<-entered

	var sent atomic.Int64
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { // 30,000 coalescing ticks over 100 subjects
		defer wg.Done()
		for i := 0; i < 30000; i++ {
			em.EmitLatest(tickEvent(fmt.Sprintf("S%d", i%subjects), i))
			sent.Add(1)
		}
	}()
	go func() { // 30,000 non-blocking events
		defer wg.Done()
		for i := 0; i < 30000; i++ {
			em.EmitNonBlocking(tickEvent("nb", i))
			sent.Add(1)
		}
	}()
	// and a few sources that block, as Emit allows.
	var blockers sync.WaitGroup
	for i := 0; i < 4; i++ {
		blockers.Add(1)
		go func() {
			defer blockers.Done()
			for j := 0; j < 500; j++ {
				em.Emit(tickEvent("snap", j))
				sent.Add(1)
			}
		}()
	}
	wg.Wait()

	st := s.Stats()
	if limit := DefaultInboxSize + subjects; st.QueueHighWater > limit {
		t.Fatalf("queue high-water = %d, want at most %d (inbox + subjects)", st.QueueHighWater, limit)
	}
	if st.DroppedNonBlocking == 0 || st.Coalesced == 0 {
		t.Fatalf("stats = %+v: a flood into a stalled pipeline must drop and coalesce", st)
	}

	close(gate) // let it drain
	blockers.Wait()
	total := int64(1 + 30000 + 30000 + 4*500)
	waitFor(t, "everything accounted for", func() bool {
		st := s.Stats()
		return int64(st.Processed+st.Coalesced+st.DroppedNonBlocking+st.DroppedLatestFull) == total && st.QueueDepth == 0
	})
	if sent.Load() != total-1 {
		t.Fatalf("sent %d", sent.Load())
	}
}

type stallEval struct {
	gate    chan struct{}
	entered chan struct{}
	once    sync.Once
}

func (s *stallEval) Evaluate(context.Context, View, Diff) ([]proactive.Candidate, error) {
	s.once.Do(func() {
		s.entered <- struct{}{}
		<-s.gate
	})
	return nil, nil
}

func TestEngine_KeyTableIsBounded(t *testing.T) {
	e := New(Options{MaxSubjects: 10, Clock: perceptiontest.NewClock(noon)})
	for i := 0; i < 50; i++ {
		e.Emitter().EmitLatest(tickEvent(fmt.Sprintf("S%d", i), i))
	}
	st := e.Stats()
	if st.QueueDepth != 10 || st.DroppedLatestFull != 40 {
		t.Fatalf("stats = %+v, want 10 held and 40 refused", st)
	}
	// a key already waiting is still updated when the table is full.
	e.Emitter().EmitLatest(tickEvent("S0", 99))
	if st := e.Stats(); st.Coalesced != 1 || st.QueueDepth != 10 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestEngine_StopReleasesBlockedEmitsAndDrains(t *testing.T) {
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	rec := &recorder{}
	e := New(Options{
		Evaluator: &stallEval{gate: gate, entered: entered}, WorldModel: rec,
		Clock: perceptiontest.NewClock(noon), InboxSize: 4,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()
	em := e.Emitter()
	em.Emit(tickEvent("X", 0))
	<-entered
	for i := 1; i <= 4; i++ { // fills the inbox
		em.Emit(tickEvent("X", i))
	}
	blocked := make(chan struct{})
	go func() {
		em.Emit(tickEvent("X", 5)) // blocks: the inbox is full
		close(blocked)
	}()
	time.Sleep(20 * time.Millisecond)
	select {
	case <-blocked:
		t.Fatal("Emit did not block on a full inbox")
	default:
	}

	cancel()
	select {
	case <-blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("a blocked Emit was not released by the stop")
	}
	close(gate)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// everything accepted before the stop was processed; the late emit was refused.
	if got := rec.appliedCopy(); len(got) != 5 {
		t.Fatalf("applied %v, want the five accepted events", got)
	}
	if st := e.Stats(); st.DroppedClosed != 1 {
		t.Fatalf("stats = %+v", st)
	}
	em.EmitLatest(tickEvent("X", 6))
	if e.Emitter().EmitNonBlocking(tickEvent("X", 7)) {
		t.Fatal("a stopped engine accepted an event")
	}
	if err := e.Run(context.Background()); err == nil {
		t.Fatal("Run twice did not fail")
	}
}

func TestEngine_EmitContextGivesUp(t *testing.T) {
	e := New(Options{InboxSize: 1, Clock: perceptiontest.NewClock(noon)})
	ce := e.Emitter().(interface {
		EmitContext(ctx context.Context, ev events.Event) bool
	})
	if !ce.EmitContext(context.Background(), tickEvent("X", 0)) {
		t.Fatal("first emit refused")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if ce.EmitContext(ctx, tickEvent("X", 1)) {
		t.Fatal("emit into a full inbox succeeded")
	}
	if e.Stats().DroppedCanceled != 1 {
		t.Fatalf("stats = %+v", e.Stats())
	}
}

var _ = strings.Contains
