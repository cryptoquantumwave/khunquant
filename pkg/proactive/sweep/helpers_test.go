package sweep

import (
	"bytes"
	"context"
	"errors"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/worldmodel"
)

var t0 = time.Date(2026, 3, 10, 5, 0, 0, 0, time.UTC)

// syncBuf is a log sink that is safe for concurrent writers.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

var ansi = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// String is the log without colour codes, so that fields read as key=value.
func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return ansi.ReplaceAllString(s.b.String(), "")
}

func captureLogs(t *testing.T) *syncBuf {
	t.Helper()
	sink := &syncBuf{}
	logger.SetAdditionalWriter(sink)
	t.Cleanup(func() { logger.SetAdditionalWriter(nil) })
	return sink
}

// fakeWorld is a world model with a settable diff and a mark.
type fakeWorld struct {
	mu       sync.Mutex
	cursor   int64
	diff     worldmodel.Diff
	hasMark  bool
	markSeq  int64
	markAt   time.Time
	sinceErr error
	sets     []int64 // positions the mark was moved to, in order
	ensured  int
}

func newWorld(diff worldmodel.Diff) *fakeWorld {
	return &fakeWorld{cursor: 10, diff: diff, hasMark: true, markSeq: 4, markAt: t0}
}

func (w *fakeWorld) Cursor() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.cursor
}

func (w *fakeWorld) SinceMark(context.Context, string) (worldmodel.Diff, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.sinceErr != nil {
		return worldmodel.Diff{}, w.sinceErr
	}
	if !w.hasMark {
		return worldmodel.Diff{}, worldmodel.ErrNoMark
	}
	return w.diff, nil
}

func (w *fakeWorld) MarkInfo(string) (int64, time.Time, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.markSeq, w.markAt, w.hasMark
}

func (w *fakeWorld) EnsureMark(context.Context, string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.ensured++
	w.hasMark = true
	return nil
}

func (w *fakeWorld) SetMarkAtTime(_ context.Context, _ string, seq int64, at time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.sets = append(w.sets, seq)
	w.markSeq, w.markAt = seq, at
	return nil
}

func (w *fakeWorld) setCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.sets)
}

// fakeTopics returns scripted topic lists, one per call (the last repeats).
type fakeTopics struct {
	mu    sync.Mutex
	lists [][]Topic
	err   error
	calls int
	since []time.Time
}

func (f *fakeTopics) TriggerTopics(_ context.Context, since time.Time) ([]Topic, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.since = append(f.since, since)
	if f.err != nil {
		return nil, f.err
	}
	if len(f.lists) == 0 {
		return nil, nil
	}
	i := min(f.calls-1, len(f.lists)-1)
	return f.lists[i], nil
}

// fakeTurn is a model turn that returns scripted outcomes and records its prompts.
type fakeTurn struct {
	mu      sync.Mutex
	prompts []string
	outs    []Outcome
	errs    []error
	hook    func()
}

func (f *fakeTurn) run(_ context.Context, prompt string) (Outcome, error) {
	f.mu.Lock()
	i := len(f.prompts)
	f.prompts = append(f.prompts, prompt)
	hook := f.hook
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	if i < len(f.errs) && f.errs[i] != nil {
		return Outcome{}, f.errs[i]
	}
	if len(f.outs) == 0 {
		return Outcome{Decisions: []proactive.Decision{{Kind: proactive.DecideSilent}}}, nil
	}
	return f.outs[min(i, len(f.outs)-1)], nil
}

func (f *fakeTurn) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.prompts)
}

func (f *fakeTurn) prompt(i int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.prompts[i]
}

// fakeDeliver records what was submitted.
type fakeDeliver struct {
	mu   sync.Mutex
	got  [][]proactive.Decision
	hook func()
}

func (f *fakeDeliver) deliver(_ context.Context, kept []proactive.Decision, _ Outcome) string {
	f.mu.Lock()
	f.got = append(f.got, kept)
	hook := f.hook
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	return "delivered"
}

func (f *fakeDeliver) all() []proactive.Decision {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []proactive.Decision
	for _, g := range f.got {
		out = append(out, g...)
	}
	return out
}

// fakeRecon returns a scripted report.
type fakeRecon struct {
	rep   perception.ReconcileReport
	calls int
	hook  func(ctx context.Context)
}

func (f *fakeRecon) ReconcileReport(ctx context.Context) perception.ReconcileReport {
	f.calls++
	if f.hook != nil {
		f.hook(ctx)
	}
	return f.rep
}

type fakeSettle struct {
	err   error
	calls int
}

func (f *fakeSettle) Settle(context.Context) error { f.calls++; return f.err }

// rig is a Runner over fakes.
type rig struct {
	world   *fakeWorld
	topics  *fakeTopics
	turn    *fakeTurn
	deliver *fakeDeliver
	recon   *fakeRecon
	settle  *fakeSettle
	state   *State
	run     *Runner
}

func newRig(diff worldmodel.Diff) *rig {
	r := &rig{
		world: newWorld(diff), topics: &fakeTopics{}, turn: &fakeTurn{}, deliver: &fakeDeliver{},
		recon: &fakeRecon{}, settle: &fakeSettle{}, state: &State{},
	}
	r.build()
	return r
}

func (r *rig) build() {
	r.run = New(Config{
		Reconciler: r.recon, Settler: r.settle, World: r.world, Topics: r.topics,
		Turn: r.turn.run, Deliver: r.deliver.deliver, Now: func() time.Time { return t0.Add(time.Hour) },
		State: r.state,
	})
}

// bigMove is a diff that is material: BTC's hourly close moved 8%.
func bigMove() worldmodel.Diff {
	return worldmodel.Diff{Changes: []worldmodel.Change{{
		Subject: "price:binance:BTC/USDT", Key: "close_1h", Old: 100.0, New: 92.0, Trust: proactive.TrustExchange,
	}}}
}

// noiseOnly is a diff of moves below the floor.
func noiseOnly() worldmodel.Diff {
	return worldmodel.Diff{Changes: []worldmodel.Change{
		{Subject: "portfolio", Key: "total_usd", Old: 10000.0, New: 10020.0, Trust: proactive.TrustSystem},
		{Subject: "price:binance:BTC/USDT", Key: "close_1h", Old: 100.0, New: 100.4, Trust: proactive.TrustExchange},
	}}
}

var errBoom = errors.New("boom")

// nullEmitter discards what sources emit.
type nullEmitter struct{}

func (nullEmitter) Emit(events.Event)                 {}
func (nullEmitter) EmitLatest(events.Event)           {}
func (nullEmitter) EmitNonBlocking(events.Event) bool { return true }

// stuckSource is a source whose Reconcile waits on a lock that ignores contexts
// (when it has one) and returns at once otherwise.
type stuckSource struct {
	name string
	lock *sync.Mutex
}

func (s *stuckSource) Name() string                                    { return s.name }
func (s *stuckSource) Start(context.Context, perception.Emitter) error { return nil }
func (s *stuckSource) Health() perception.SourceHealth                 { return perception.SourceHealth{} }
func (s *stuckSource) Reconcile(context.Context, perception.Emitter) error {
	if s.lock != nil {
		s.lock.Lock()
		defer s.lock.Unlock()
	}
	return nil
}

func notify(key, text string) proactive.Decision {
	return proactive.Decision{Kind: proactive.DecideNotify, Severity: proactive.SeverityWarning, DedupKey: key, Text: text}
}
