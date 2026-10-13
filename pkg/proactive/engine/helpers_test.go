package engine

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/attention"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/perceptiontest"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex/sqlitextest"
)

var bkk = func() *time.Location {
	l, err := time.LoadLocation("Asia/Bangkok")
	if err != nil {
		panic(err)
	}
	return l
}()

// noon is 12:00 in Bangkok: inside the default active hours.
var noon = time.Date(2026, 3, 10, 12, 0, 0, 0, bkk)

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// tickEvent is a market tick for subject, numbered n, from an exchange.
func tickEvent(subject string, n int) events.Event {
	return events.Event{
		ID:     fmt.Sprintf("%s#%d", subject, n),
		Kind:   events.KindPerceptionMarketTick,
		Source: events.Source{Component: "perception", Name: "test"},
		Attrs:  map[string]any{"subject": subject, "n": n},
		Trust:  events.TrustPtr(proactive.TrustExchange),
	}
}

func nOf(ev events.Event) int { return ev.Attrs["n"].(int) }

// started is an engine running on its own goroutine.
type started struct {
	*Engine
	cancel context.CancelFunc
	done   chan error
}

func start(t *testing.T, o Options) *started {
	t.Helper()
	e := New(o)
	ctx, cancel := context.WithCancel(context.Background())
	s := &started{Engine: e, cancel: cancel, done: make(chan error, 1)}
	go func() { s.done <- e.Run(ctx) }()
	t.Cleanup(func() { s.stop(t) })
	return s
}

func (s *started) stop(t *testing.T) {
	t.Helper()
	s.cancel()
	select {
	case err := <-s.done:
		s.done <- err
		if err != nil {
			t.Errorf("Run: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Error("Run did not return")
	}
}

// recorder collects the calls the stages receive.
type recorder struct {
	mu      sync.Mutex
	applied []int
	evals   []int
	disp    [][]attention.Decision
	cands   []proactive.Candidate
	house   []time.Time
}

func (r *recorder) Apply(_ context.Context, ev events.Event) (Diff, error) {
	r.mu.Lock()
	r.applied = append(r.applied, nOf(ev))
	r.mu.Unlock()
	return nOf(ev), nil
}

func (r *recorder) View() View { return "view" }

func (r *recorder) Evaluate(_ context.Context, v View, d Diff) ([]proactive.Candidate, error) {
	n := d.(int)
	r.mu.Lock()
	r.evals = append(r.evals, n)
	r.mu.Unlock()
	return []proactive.Candidate{candidate(fmt.Sprintf("k%d", n))}, nil
}

func (r *recorder) Dispatch(_ context.Context, ds []attention.Decision) error {
	r.mu.Lock()
	r.disp = append(r.disp, ds)
	for _, d := range ds {
		r.cands = append(r.cands, d.Candidate)
	}
	r.mu.Unlock()
	return nil
}

func (r *recorder) Housekeep(_ context.Context, now time.Time) {
	r.mu.Lock()
	r.house = append(r.house, now)
	r.mu.Unlock()
}

func (r *recorder) appliedCopy() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int(nil), r.applied...)
}

func (r *recorder) dispatches() [][]attention.Decision {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]attention.Decision(nil), r.disp...)
}

func candidate(key string) proactive.Candidate {
	return proactive.Candidate{
		TriggerID: "trig:" + key, DedupKey: key, Severity: proactive.SeverityNotice,
		Subject: "BTC/USDT", Title: "title " + key, Trust: proactive.TrustSystem,
	}
}

// staticProfile is an attention.ProfileSource with the default profile.
type staticProfile struct{}

func (staticProfile) Profile(context.Context) (intent.Profile, error) {
	return intent.DefaultProfile(), nil
}

// govEnv is a real governor over a fresh database, on a fake clock.
type govEnv struct {
	db    *sql.DB
	gov   *attention.Governor
	clock *perceptiontest.Clock
}

func newGov(t *testing.T, mode string, cfg config.AttentionConfig) *govEnv {
	t.Helper()
	db := sqlitextest.Open(t)
	clock := perceptiontest.NewClock(noon)
	g, err := attention.New(db, staticProfile{}, cfg, mode, clock.Now)
	if err != nil {
		t.Fatalf("attention.New: %v", err)
	}
	return &govEnv{db: db, gov: g, clock: clock}
}

func liveCfg() config.AttentionConfig {
	return config.AttentionConfig{Mode: config.ProactiveModeLive, DailyBudget: 3, DedupWindowHours: 24}
}

func (g *govEnv) count(t *testing.T, q string, args ...any) int {
	t.Helper()
	var n int
	if err := g.db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", q, err)
	}
	return n
}

// runOutbox runs the outbox's lanes with a fast tick and stops it with the test.
func runOutbox(t *testing.T, ob *attention.Outbox) {
	t.Helper()
	ob.SetTickInterval(2 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		ob.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("outbox Run did not return")
		}
	})
}

// fakeDeliverer is an attention.Deliverer that records and may block.
type fakeDeliverer struct {
	mu   sync.Mutex
	msgs []attention.Message
	fn   func(ctx context.Context, m attention.Message) (bool, error)
}

func (f *fakeDeliverer) Deliver(ctx context.Context, m attention.Message) (bool, error) {
	f.mu.Lock()
	f.msgs = append(f.msgs, m)
	fn := f.fn
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, m)
	}
	return true, nil
}

func (f *fakeDeliverer) messages() []attention.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]attention.Message(nil), f.msgs...)
}
