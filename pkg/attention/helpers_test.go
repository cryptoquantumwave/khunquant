package attention

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex/sqlitextest"
)

// fakeClock is a settable clock safe for concurrent use.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

func (c *fakeClock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// fakeProfile is a scriptable ProfileSource.
type fakeProfile struct {
	mu    sync.Mutex
	p     intent.Profile
	err   error
	calls int
}

func (f *fakeProfile) Profile(context.Context) (intent.Profile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return intent.Profile{}, f.err
	}
	return f.p, nil
}

func (f *fakeProfile) set(mut func(*intent.Profile)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	mut(&f.p)
}

func (f *fakeProfile) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// profileFunc adapts a function to ProfileSource.
type profileFunc func(context.Context) (intent.Profile, error)

func (f profileFunc) Profile(ctx context.Context) (intent.Profile, error) { return f(ctx) }

var bkk = mustLoc("Asia/Bangkok")

func mustLoc(name string) *time.Location {
	l, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return l
}

// bkkAt is h:m on 2026-03-10+day in Bangkok. 2026-03-10 is a Tuesday.
func bkkAt(day, h, m int) time.Time {
	return time.Date(2026, 3, 10+day, h, m, 0, 0, bkk)
}

// env is a governor over a fresh database with a fake clock and profile. The
// clock starts at 12:00 Bangkok, inside the default active hours.
type env struct {
	t     *testing.T
	db    *sql.DB
	g     *Governor
	clock *fakeClock
	prof  *fakeProfile
}

func liveCfg() config.AttentionConfig {
	return config.AttentionConfig{
		Mode:             config.ProactiveModeLive,
		DailyBudget:      3,
		DedupWindowHours: 24,
	}
}

func newEnv(t *testing.T) *env { return newEnvMode(t, config.ProactiveModeLive, liveCfg()) }

func newEnvMode(t *testing.T, mode string, cfg config.AttentionConfig) *env {
	t.Helper()
	db := sqlitextest.Open(t)
	clock := &fakeClock{t: bkkAt(0, 12, 0)}
	prof := &fakeProfile{p: intent.DefaultProfile()}
	g, err := New(db, prof, cfg, mode, clock.Now)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return &env{t: t, db: db, g: g, clock: clock, prof: prof}
}

// cand builds a Notice candidate; opts adjust it.
func cand(key string, sev proactive.Severity, opts ...func(*proactive.Candidate)) proactive.Candidate {
	c := proactive.Candidate{
		TriggerID: "trig:" + key,
		DedupKey:  key,
		Severity:  sev,
		Subject:   "BTC/USDT",
		Title:     "title " + key,
	}
	for _, o := range opts {
		o(&c)
	}
	return c
}

func protective(c *proactive.Candidate) { c.Protective = true }
func composeLLM(c *proactive.Candidate) { c.Compose = proactive.ComposeLLM }
func withTrigger(id string) func(*proactive.Candidate) {
	return func(c *proactive.Candidate) { c.TriggerID = id }
}

func (e *env) decide(c proactive.Candidate) Decision {
	e.t.Helper()
	d, err := e.g.Decide(context.Background(), c)
	if err != nil {
		e.t.Fatalf("Decide(%s): %v", c.DedupKey, err)
	}
	return d
}

func (e *env) mark(d Decision, ok bool) {
	e.t.Helper()
	if err := e.g.MarkDelivered(context.Background(), d, Delivery{Delivered: ok}); err != nil {
		e.t.Fatalf("MarkDelivered(%s): %v", d.ID, err)
	}
}

// send decides c and, when it is a notify or escalate, reports it delivered.
func (e *env) send(c proactive.Candidate) Decision {
	e.t.Helper()
	d := e.decide(c)
	if d.Disposition == proactive.DispNotify || d.Disposition == proactive.DispEscalate {
		e.mark(d, true)
	}
	return d
}

func (e *env) count(q string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.db.QueryRow(q, args...).Scan(&n); err != nil {
		e.t.Fatalf("count %q: %v", q, err)
	}
	return n
}

func expect(t *testing.T, d Decision, disp proactive.Disposition, reason string) {
	t.Helper()
	if d.Disposition != disp || d.Reason != reason {
		t.Fatalf("decision = %s/%s, want %s/%s (candidate %s sev %s)",
			d.Disposition, d.Reason, disp, reason, d.Candidate.DedupKey, d.Candidate.Severity)
	}
}
