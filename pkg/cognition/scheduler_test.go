package cognition

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex/sqlitextest"
)

const waitFor = 5 * time.Second

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock(t time.Time) *fakeClock { return &fakeClock{t: t} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

var t0 = time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

// events records, in order, what the jobs of a test did.
type events struct {
	mu  sync.Mutex
	log []string
}

func (e *events) add(s string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.log = append(e.log, s)
}

func (e *events) all() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.log)
}

func (e *events) has(s string) bool { return slices.Contains(e.all(), s) }

// start builds a Scheduler on a fresh database, runs it, and stops it at cleanup.
func start(t *testing.T, cfg config.CognitionConfig, clock Clock, zone func() *time.Location) *Scheduler {
	t.Helper()
	return startOn(t, sqlitextest.Open(t), cfg, clock, zone)
}

func startOn(t *testing.T, db *sql.DB, cfg config.CognitionConfig, clock Clock, zone func() *time.Location) *Scheduler {
	t.Helper()
	s, err := New(cfg, db, clock, zone)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	stopSched(t, s)
	return s
}

func stopSched(t *testing.T, s *Scheduler) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("Run returned %v", err)
				}
			case <-time.After(waitFor):
				t.Errorf("Run did not return")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

// cfgOf has budgets too large to matter; a test narrows the one it exercises.
func cfgOf(workers int) config.CognitionConfig {
	return config.CognitionConfig{
		Enabled: true, MaxConcurrentLLM: workers, WakesPerHour: 1000, TokensPerDay: 10_000_000,
	}
}

func submit(t *testing.T, s *Scheduler, j Job) <-chan Result {
	t.Helper()
	ch, err := s.Submit(context.Background(), j)
	if err != nil {
		t.Fatalf("Submit(%s): %v", j.ID, err)
	}
	return ch
}

func result(t *testing.T, ch <-chan Result) Result {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(waitFor):
		t.Fatal("no Result")
		return Result{}
	}
}

func recv(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(waitFor):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// gate is a job that records start/end, signals started, and finishes when
// released (or cancelled).
type gate struct {
	started chan struct{}
	release chan struct{}
}

func newGate() *gate { return &gate{started: make(chan struct{}), release: make(chan struct{})} }

func (g *gate) job(ev *events, id string, pri Priority, session string) Job {
	return Job{
		ID: id, Kind: proactive.TurnProactive, Priority: pri, SessionKey: session,
		Run: func(ctx context.Context) (Usage, error) {
			ev.add("start " + id)
			close(g.started)
			select {
			case <-g.release:
			case <-ctx.Done():
				ev.add("cancelled " + id)
				return Usage{}, ctx.Err()
			}
			ev.add("end " + id)
			return Usage{}, nil
		},
	}
}

func quick(ev *events, id string, pri Priority, session string) Job {
	return Job{
		ID: id, Kind: proactive.TurnProactive, Priority: pri, SessionKey: session,
		Run: func(context.Context) (Usage, error) {
			ev.add("run " + id)
			return Usage{}, nil
		},
	}
}

func TestScheduler_UserBeforeProactive(t *testing.T) {
	s := start(t, cfgOf(1), nil, nil)
	var ev events
	g := newGate()
	first := submit(t, s, g.job(&ev, "blocker", PriProactive, "s0"))
	recv(t, g.started, "blocker")

	// Queue in the "wrong" order, one session each so only priority orders them.
	chans := []<-chan Result{
		submit(t, s, quick(&ev, "legacy", PriLegacy, "s1")),
		submit(t, s, quick(&ev, "reflection", PriReflection, "s2")),
		submit(t, s, quick(&ev, "proactive1", PriProactive, "s3")),
		submit(t, s, quick(&ev, "proactive2", PriProactive, "s4")),
		submit(t, s, quick(&ev, "followup", PriApprovalFollowUp, "s5")),
		submit(t, s, quick(&ev, "protective", PriProtective, "s6")),
		submit(t, s, quick(&ev, "user", PriUser, "s7")),
	}
	st := s.Stats()
	if st.Running != 1 || st.Queued["user"] != 1 || st.Queued["proactive"] != 2 || st.Queued["legacy"] != 1 {
		t.Fatalf("Stats while blocked = %+v", st)
	}
	close(g.release)
	result(t, first)
	for _, ch := range chans {
		result(t, ch)
	}
	want := []string{
		"start blocker", "end blocker",
		"run user", "run protective", "run followup", "run proactive1", "run proactive2",
		"run reflection", "run legacy",
	}
	if got := ev.all(); !slices.Equal(got, want) {
		t.Fatalf("order = %v\nwant    %v", got, want)
	}
}

func TestScheduler_SessionFIFO(t *testing.T) {
	s := start(t, cfgOf(2), nil, nil) // a free worker must not break the session rule
	var ev events
	g := newGate()
	a1 := submit(t, s, g.job(&ev, "a1", PriProactive, "A"))
	recv(t, g.started, "a1")
	a2 := submit(t, s, quick(&ev, "a2", PriProactive, "A"))
	// B was submitted after A2 and runs on the free worker while A2 waits.
	b1 := submit(t, s, quick(&ev, "b1", PriProactive, "B"))
	result(t, b1)
	if ev.has("run a2") {
		t.Fatalf("a2 started while a1 was still running: %v", ev.all())
	}
	close(g.release)
	result(t, a1)
	result(t, a2)
	got := ev.all()
	if i, j := slices.Index(got, "end a1"), slices.Index(got, "run a2"); i < 0 || j < i {
		t.Fatalf("a2 did not start after a1 ended: %v", got)
	}
}

func TestScheduler_FIFOWithinPriority(t *testing.T) {
	s := start(t, cfgOf(1), nil, nil)
	var ev events
	g := newGate()
	first := submit(t, s, g.job(&ev, "blocker", PriUser, ""))
	recv(t, g.started, "blocker")
	var chans []<-chan Result
	for _, id := range []string{"1", "2", "3", "4"} {
		chans = append(chans, submit(t, s, quick(&ev, id, PriProactive, "same")))
	}
	close(g.release)
	result(t, first)
	for _, ch := range chans {
		result(t, ch)
	}
	if got, want := ev.all()[2:], []string{"run 1", "run 2", "run 3", "run 4"}; !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestScheduler_DifferentSessionsParallelWhenWorkers2(t *testing.T) {
	s := start(t, cfgOf(2), nil, nil)
	var ev events
	ga, gb := newGate(), newGate()
	a := submit(t, s, ga.job(&ev, "a", PriUser, "A"))
	b := submit(t, s, gb.job(&ev, "b", PriUser, "B"))
	recv(t, ga.started, "a") // both running at once: neither has been released
	recv(t, gb.started, "b")
	if st := s.Stats(); st.Running != 2 {
		t.Fatalf("Running = %d, want 2", st.Running)
	}
	close(ga.release)
	close(gb.release)
	result(t, a)
	result(t, b)
}

func TestScheduler_OneWorkerRunsOneJob(t *testing.T) {
	s := start(t, config.CognitionConfig{}, nil, nil) // MaxConcurrentLLM 0 = 1
	var ev events
	ga := newGate()
	a := submit(t, s, ga.job(&ev, "a", PriUser, "A"))
	recv(t, ga.started, "a")
	b := submit(t, s, quick(&ev, "b", PriUser, "B"))
	if st := s.Stats(); st.Workers != 1 || st.Running != 1 || st.Queued["user"] != 1 {
		t.Fatalf("Stats = %+v", st)
	}
	close(ga.release)
	result(t, a)
	result(t, b)
	if got, want := ev.all(), []string{"start a", "end a", "run b"}; !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

// A job whose session is busy waits without holding back a later job of another
// session, even though it outranks it.
func TestScheduler_BusySessionPassedOverForOtherSession(t *testing.T) {
	s := start(t, cfgOf(2), nil, nil)
	var ev events
	g := newGate()
	a1 := submit(t, s, g.job(&ev, "a1", PriUser, "A"))
	recv(t, g.started, "a1")
	// Higher priority than b, but its session is busy: b must still run.
	a2 := submit(t, s, quick(&ev, "a2", PriUser, "A"))
	b := submit(t, s, quick(&ev, "b", PriLegacy, "B"))
	result(t, b)
	if ev.has("run a2") {
		t.Fatal("a2 ran while a1 held the session")
	}
	close(g.release)
	result(t, a1)
	result(t, a2)
}

func budgeted(ev *events, id string, session string, in, out int) Job {
	return Job{
		ID: id, Kind: proactive.TurnProactive, Priority: PriProactive, SessionKey: session, Budgeted: true,
		Run: func(context.Context) (Usage, error) {
			ev.add("run " + id)
			return Usage{InputTokens: in, OutputTokens: out, Model: "m"}, nil
		},
		Fallback: func(_ context.Context, reason string) error {
			ev.add("fallback " + id + " " + reason)
			return nil
		},
	}
}

func TestScheduler_BudgetFallback(t *testing.T) {
	cfg := cfgOf(1)
	cfg.TokensPerDay = 100
	s := start(t, cfg, newClock(t0), nil)
	var ev events

	r1 := result(t, submit(t, s, budgeted(&ev, "j1", "s", 60, 40)))
	if r1.Fallback || r1.Err != nil || r1.Usage.InputTokens != 60 {
		t.Fatalf("j1 = %+v", r1)
	}
	r2 := result(t, submit(t, s, budgeted(&ev, "j2", "s", 1, 1)))
	if !r2.Fallback || r2.Err != nil {
		t.Fatalf("j2 = %+v, want a successful fallback", r2)
	}
	// An unbudgeted job is not held to the budget.
	u := quick(&ev, "user", PriUser, "s")
	u.Kind = proactive.TurnUser
	if r := result(t, submit(t, s, u)); r.Err != nil || r.Fallback {
		t.Fatalf("user = %+v", r)
	}
	// Over budget with no Fallback: the sentinel error.
	nf := budgeted(&ev, "j3", "s", 1, 1)
	nf.Fallback = nil
	if r := result(t, submit(t, s, nf)); !errors.Is(r.Err, ErrBudgetExhausted) || r.Fallback {
		t.Fatalf("j3 = %+v", r)
	}
	want := []string{"run j1", "fallback j2 budget_exhausted", "run user"}
	if got := ev.all(); !slices.Equal(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	if st := s.Stats(); st.Budget["proactive"] != (BudgetUse{Wakes: 1, InputTokens: 60, OutputTokens: 40}) {
		t.Fatalf("Stats.Budget = %+v", st.Budget)
	}
}

// A limit of 0 allows no budgeted work (it is not "unlimited"); jobs that are not
// Budgeted are unaffected.
func TestScheduler_ZeroBudgetBlocksBudgeted(t *testing.T) {
	for name, mutate := range map[string]func(*config.CognitionConfig){
		"tokens_per_day": func(c *config.CognitionConfig) { c.TokensPerDay = 0 },
		"wakes_per_hour": func(c *config.CognitionConfig) { c.WakesPerHour = 0 },
		"negative":       func(c *config.CognitionConfig) { c.TokensPerDay = -5 },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := cfgOf(1)
			mutate(&cfg)
			s := start(t, cfg, newClock(t0), nil)
			var ev events
			if r := result(t, submit(t, s, budgeted(&ev, "bg", "s", 1, 1))); !r.Fallback || r.Err != nil {
				t.Fatalf("budgeted job = %+v, want a fallback", r)
			}
			u := quick(&ev, "user", PriUser, "s")
			u.Kind = proactive.TurnUser
			if r := result(t, submit(t, s, u)); r.Fallback || r.Err != nil {
				t.Fatalf("non-budgeted job = %+v, want it to run", r)
			}
			want := []string{"fallback bg budget_exhausted", "run user"}
			if got := ev.all(); !slices.Equal(got, want) {
				t.Fatalf("events = %v, want %v", got, want)
			}
		})
	}
}

func TestScheduler_WakesPerHourRolls(t *testing.T) {
	cfg := cfgOf(1)
	cfg.WakesPerHour = 2
	clock := newClock(t0)
	s := start(t, cfg, clock, nil)
	var ev events
	for _, id := range []string{"w1", "w2"} {
		if r := result(t, submit(t, s, budgeted(&ev, id, "s", 1, 1))); r.Fallback {
			t.Fatalf("%s fell back", id)
		}
		clock.Advance(10 * time.Minute)
	}
	if r := result(t, submit(t, s, budgeted(&ev, "w3", "s", 1, 1))); !r.Fallback {
		t.Fatal("w3 should be over the hourly wake budget")
	}
	clock.Advance(45 * time.Minute) // w1 is now 65 min old, w2 55
	if r := result(t, submit(t, s, budgeted(&ev, "w4", "s", 1, 1))); r.Fallback {
		t.Fatal("w4 should run: one wake left the rolling hour")
	}
	if r := result(t, submit(t, s, budgeted(&ev, "w5", "s", 1, 1))); !r.Fallback {
		t.Fatal("w5 should be over the hourly wake budget again")
	}
}

func TestScheduler_BudgetDayBoundaryInLocalZone(t *testing.T) {
	ict := time.FixedZone("ICT", 7*3600)
	cfg := cfgOf(1)
	cfg.TokensPerDay = 100
	// 16:30 UTC is 23:30 on 18 Oct in ICT; 17:30 UTC is 00:30 on 19 Oct.
	clock := newClock(time.Date(2026, 10, 18, 16, 30, 0, 0, time.UTC))
	s := start(t, cfg, clock, func() *time.Location { return ict })
	var ev events

	if r := result(t, submit(t, s, budgeted(&ev, "d1", "s", 100, 0))); r.Fallback {
		t.Fatal("d1 fell back")
	}
	if r := result(t, submit(t, s, budgeted(&ev, "d2", "s", 1, 0))); !r.Fallback {
		t.Fatal("d2 should be over budget on the same ICT day")
	}
	if st := s.Stats(); st.Day != "2026-10-18" {
		t.Fatalf("Day = %s, want 2026-10-18", st.Day)
	}
	// One hour on, it is still 18 Oct in UTC but 19 Oct in ICT: a new budget.
	clock.Advance(time.Hour)
	if r := result(t, submit(t, s, budgeted(&ev, "d3", "s", 1, 0))); r.Fallback {
		t.Fatal("d3 should run: the ICT day rolled over at 17:00 UTC")
	}
	if st := s.Stats(); st.Day != "2026-10-19" || st.Budget["proactive"].InputTokens != 1 {
		t.Fatalf("Stats = %+v", st)
	}
}

func TestScheduler_BudgetUTCWhenZoneNil(t *testing.T) {
	cfg := cfgOf(1)
	cfg.TokensPerDay = 10
	clock := newClock(time.Date(2026, 10, 18, 23, 30, 0, 0, time.UTC))
	s := start(t, cfg, clock, func() *time.Location { return nil })
	var ev events
	result(t, submit(t, s, budgeted(&ev, "u1", "s", 10, 0)))
	clock.Advance(time.Hour)
	if r := result(t, submit(t, s, budgeted(&ev, "u2", "s", 1, 0))); r.Fallback {
		t.Fatal("u2 should run on the next UTC day")
	}
}

// Usage is added after a job, so the day total can pass TokensPerDay by what the
// last admitted job spent, and by no more.
func TestScheduler_OvershootBoundedByOneJob(t *testing.T) {
	cfg := cfgOf(1)
	cfg.TokensPerDay = 100
	s := start(t, cfg, newClock(t0), nil)
	var ev events
	var ran, fell int
	for i := 0; i < 6; i++ {
		r := result(t, submit(t, s, budgeted(&ev, "o", "s", 80, 0)))
		if r.Fallback {
			fell++
		} else {
			ran++
		}
	}
	if ran != 2 || fell != 4 {
		t.Fatalf("ran=%d fell=%d, want 2 and 4: 80 < 100 admits a second job, 160 does not admit a third", ran, fell)
	}
	used := s.Stats().Budget["proactive"].InputTokens
	if used > cfg.TokensPerDay+80 {
		t.Fatalf("used %d tokens, more than TokensPerDay plus one job (%d)", used, cfg.TokensPerDay+80)
	}
}

// With two workers, budgeted jobs still run one at a time, so a second job's
// budget check always sees the first one's usage.
func TestScheduler_BudgetedJobsDoNotOverlap(t *testing.T) {
	cfg := cfgOf(2)
	cfg.TokensPerDay = 100
	s := start(t, cfg, newClock(t0), nil)
	var ev events
	ga := newGate()
	ja := ga.job(&ev, "a", PriProactive, "A")
	ja.Budgeted = true
	a := submit(t, s, ja)
	recv(t, ga.started, "a")
	b := submit(t, s, budgeted(&ev, "b", "B", 1, 1))
	other := submit(t, s, quick(&ev, "plain", PriLegacy, "C"))
	result(t, other) // the idle worker takes the unbudgeted job...
	if ev.has("run b") {
		t.Fatal("budgeted b started while budgeted a was running")
	}
	close(ga.release)
	result(t, a)
	result(t, b)
}

func TestScheduler_DeadlineFallback(t *testing.T) {
	s := start(t, cfgOf(1), nil, nil)
	var ev events
	var fbCtxErr error
	var fbReason string
	r := result(t, submit(t, s, Job{
		ID: "slow", Kind: proactive.TurnProactive, Priority: PriProactive, Deadline: 30 * time.Millisecond,
		Run: func(ctx context.Context) (Usage, error) {
			<-ctx.Done()
			ev.add("run cancelled")
			return Usage{InputTokens: 7}, ctx.Err()
		},
		Fallback: func(ctx context.Context, reason string) error {
			fbReason, fbCtxErr = reason, ctx.Err() // a fresh context, not the spent one
			ev.add("fallback")
			return nil
		},
	}))
	if !r.Fallback || r.Err != nil || r.Usage.InputTokens != 7 {
		t.Fatalf("Result = %+v", r)
	}
	if fbReason != "deadline" || fbCtxErr != nil {
		t.Fatalf("fallback reason=%q ctx.Err=%v", fbReason, fbCtxErr)
	}
	if got := ev.all(); !slices.Equal(got, []string{"run cancelled", "fallback"}) {
		t.Fatalf("events = %v", got)
	}
}

func TestScheduler_DeadlineWithoutFallback(t *testing.T) {
	s := start(t, cfgOf(1), nil, nil)
	r := result(t, submit(t, s, Job{
		ID: "slow", Kind: proactive.TurnProactive, Priority: PriProactive, Deadline: 20 * time.Millisecond,
		Run: func(ctx context.Context) (Usage, error) { <-ctx.Done(); return Usage{}, ctx.Err() },
	}))
	if !errors.Is(r.Err, ErrDeadline) || r.Fallback {
		t.Fatalf("Result = %+v", r)
	}
}

func TestScheduler_DefaultDeadlines(t *testing.T) {
	s := &Scheduler{}
	for kind, want := range map[proactive.TurnKind]time.Duration{
		proactive.TurnUser: 10 * time.Minute, proactive.TurnProactive: time.Minute,
		proactive.TurnReflection: 3 * time.Minute, proactive.TurnCronLegacy: time.Minute,
		proactive.TurnApprovalFollowUp: time.Minute,
	} {
		if got := s.deadline(Job{Kind: kind}); got != want {
			t.Errorf("deadline(%s) = %s, want %s", kind, got, want)
		}
	}
	if got := s.deadline(Job{Kind: proactive.TurnUser, Deadline: time.Second}); got != time.Second {
		t.Errorf("explicit deadline = %s", got)
	}
}

func TestScheduler_UsagePersisted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.db")
	open := func() *sql.DB {
		db, err := sqlitex.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		return db
	}
	cfg := cfgOf(1)
	cfg.TokensPerDay = 100
	clock := newClock(t0)
	var ev events

	db1 := open()
	s1, err := New(cfg, db1, clock, nil)
	if err != nil {
		t.Fatal(err)
	}
	stop := stopSched(t, s1)
	result(t, submit(t, s1, budgeted(&ev, "before", "s", 70, 40)))
	stop()
	if err := db1.Close(); err != nil {
		t.Fatal(err)
	}

	db2 := open()
	t.Cleanup(func() { _ = db2.Close() })
	s2 := startOn(t, db2, cfg, clock, nil)
	if got, want := s2.Stats().Budget["proactive"], (BudgetUse{Wakes: 1, InputTokens: 70, OutputTokens: 40}); got != want {
		t.Fatalf("after reopen Budget = %+v, want %+v", got, want)
	}
	if r := result(t, submit(t, s2, budgeted(&ev, "after", "s", 1, 1))); !r.Fallback {
		t.Fatal("the reopened scheduler forgot the day's usage: job ran instead of falling back")
	}
	if got, want := ev.all(), []string{"run before", "fallback after budget_exhausted"}; !slices.Equal(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func TestScheduler_MigrationIsIdempotent(t *testing.T) {
	db := sqlitextest.Open(t)
	for i := 0; i < 2; i++ {
		if _, err := New(cfgOf(1), db, nil, nil); err != nil {
			t.Fatalf("New #%d: %v", i, err)
		}
	}
	if _, err := New(cfgOf(1), nil, nil, nil); err == nil {
		t.Fatal("New with a nil database succeeded")
	}
}

func TestScheduler_CancelAllDropsQueued(t *testing.T) {
	s := start(t, cfgOf(1), nil, nil)
	var ev events
	g := newGate()
	running := submit(t, s, g.job(&ev, "running", PriUser, "A"))
	recv(t, g.started, "running")

	withFallback := func(id string, pri Priority, session string) Job {
		j := quick(&ev, id, pri, session)
		j.Budgeted = true
		j.Fallback = func(context.Context, string) error { ev.add("FALLBACK " + id); return nil }
		return j
	}
	q1 := submit(t, s, withFallback("q1", PriProactive, "B"))
	q2 := submit(t, s, withFallback("q2", PriUser, "A")) // behind the running job of its session

	s.CancelAll("stop")

	for name, ch := range map[string]<-chan Result{"running": running, "q1": q1, "q2": q2} {
		r := result(t, ch)
		if !errors.Is(r.Err, ErrCancelled) || r.Fallback {
			t.Fatalf("%s Result = %+v, want ErrCancelled and no fallback", name, r)
		}
	}
	for _, e := range ev.all() {
		if e == "FALLBACK q1" || e == "FALLBACK q2" || e == "run q1" || e == "run q2" {
			t.Fatalf("a dropped job did work: %v", ev.all())
		}
	}
	if st := s.Stats(); st.Queued["proactive"] != 0 || st.Queued["user"] != 0 {
		t.Fatalf("queue not empty after CancelAll: %+v", st)
	}

	// Later submissions are unaffected.
	if r := result(t, submit(t, s, quick(&ev, "after", PriUser, "A"))); r.Err != nil {
		t.Fatalf("job after CancelAll: %+v", r)
	}
}

func TestScheduler_CancelAllDoesNotRunDeadlineFallback(t *testing.T) {
	s := start(t, cfgOf(1), nil, nil)
	started := make(chan struct{})
	var fell bool
	ch := submit(t, s, Job{
		ID: "j", Kind: proactive.TurnProactive, Priority: PriProactive, Deadline: time.Hour,
		Run: func(ctx context.Context) (Usage, error) {
			close(started)
			<-ctx.Done()
			return Usage{}, ctx.Err()
		},
		Fallback: func(context.Context, string) error { fell = true; return nil },
	})
	recv(t, started, "job")
	s.CancelAll("stop")
	r := result(t, ch)
	if !errors.Is(r.Err, ErrCancelled) || !errors.Is(r.Err, context.Canceled) || r.Fallback || fell {
		t.Fatalf("Result = %+v fell=%v", r, fell)
	}
}

func TestScheduler_PanicRecovered(t *testing.T) {
	s := start(t, cfgOf(1), nil, nil)
	var ev events
	r := result(t, submit(t, s, Job{
		ID: "boom", Kind: proactive.TurnUser, Priority: PriUser,
		Run: func(context.Context) (Usage, error) { panic("kaboom") },
	}))
	if r.Err == nil {
		t.Fatal("panicking job reported no error")
	}
	// The (single) worker survived and the session is not stuck.
	if r := result(t, submit(t, s, quick(&ev, "next", PriUser, ""))); r.Err != nil {
		t.Fatalf("job after a panic: %+v", r)
	}
	// A panicking Fallback is contained too.
	cfg := cfgOf(1)
	cfg.TokensPerDay = 1
	s2 := start(t, cfg, newClock(t0), nil)
	result(t, submit(t, s2, budgeted(&ev, "spend", "s", 5, 5)))
	j := budgeted(&ev, "fb", "s", 1, 1)
	j.Fallback = func(context.Context, string) error { panic("fallback boom") }
	if r := result(t, submit(t, s2, j)); r.Err == nil || !r.Fallback {
		t.Fatalf("panicking fallback Result = %+v", r)
	}
}

func TestScheduler_SubmitterContextCancelsQueuedJob(t *testing.T) {
	s := start(t, cfgOf(1), nil, nil)
	var ev events
	g := newGate()
	first := submit(t, s, g.job(&ev, "first", PriUser, ""))
	recv(t, g.started, "first")
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := s.Submit(ctx, quick(&ev, "abandoned", PriUser, ""))
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	close(g.release)
	result(t, first)
	if r := result(t, ch); !errors.Is(r.Err, context.Canceled) {
		t.Fatalf("Result = %+v, want context.Canceled", r)
	}
	if ev.has("run abandoned") {
		t.Fatal("a job whose submitter gave up still ran")
	}
}

func TestScheduler_JobContextCarriesSubmitValues(t *testing.T) {
	type key struct{}
	s := start(t, cfgOf(1), nil, nil)
	ctx := context.WithValue(context.Background(), key{}, "origin")
	var got any
	ch, err := s.Submit(ctx, Job{Kind: proactive.TurnUser, Run: func(c context.Context) (Usage, error) {
		got = c.Value(key{})
		return Usage{}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	result(t, ch)
	if got != "origin" {
		t.Fatalf("job ctx value = %v", got)
	}
}

func TestScheduler_StopCancelsRunningAndDropsQueued(t *testing.T) {
	db := sqlitextest.Open(t)
	s, err := New(cfgOf(1), db, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var ev events
	g := newGate()
	// Queued before Run starts is fine.
	running := submit(t, s, g.job(&ev, "running", PriUser, ""))
	queued := submit(t, s, quick(&ev, "queued", PriUser, ""))
	stop := stopSched(t, s)
	recv(t, g.started, "running")
	stop()

	if r := result(t, running); !errors.Is(r.Err, ErrStopped) {
		t.Fatalf("running Result = %+v, want ErrStopped", r)
	}
	if r := result(t, queued); !errors.Is(r.Err, ErrStopped) {
		t.Fatalf("queued Result = %+v, want ErrStopped", r)
	}
	if _, err := s.Submit(context.Background(), quick(&ev, "late", PriUser, "")); !errors.Is(err, ErrStopped) {
		t.Fatalf("Submit after stop = %v, want ErrStopped", err)
	}
	if err := s.Run(context.Background()); err == nil {
		t.Fatal("second Run succeeded")
	}
}

func TestScheduler_SubmitValidation(t *testing.T) {
	s := start(t, cfgOf(1), nil, nil)
	if _, err := s.Submit(context.Background(), Job{Priority: PriUser}); err == nil {
		t.Error("job without Run accepted")
	}
	run := func(context.Context) (Usage, error) { return Usage{}, nil }
	if _, err := s.Submit(context.Background(), Job{Priority: Priority(99), Run: run}); err == nil {
		t.Error("invalid priority accepted")
	}
	if _, err := s.Submit(context.Background(), Job{Priority: Priority(-1), Run: run}); err == nil {
		t.Error("negative priority accepted")
	}
}

func TestScheduler_QueueBound(t *testing.T) {
	s, err := New(cfgOf(1), sqlitextest.Open(t), nil, nil) // never started: nothing drains
	if err != nil {
		t.Fatal(err)
	}
	run := func(context.Context) (Usage, error) { return Usage{}, nil }
	for i := 0; i < maxQueued; i++ {
		if _, err := s.Submit(context.Background(), Job{Priority: PriProactive, Run: run}); err != nil {
			t.Fatalf("Submit #%d: %v", i, err)
		}
	}
	if _, err := s.Submit(context.Background(), Job{Priority: PriProactive, Run: run}); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("Submit over the bound = %v, want ErrQueueFull", err)
	}
	if _, err := s.Submit(context.Background(), Job{Priority: PriUser, Run: run}); err != nil {
		t.Fatalf("a user job was refused: %v", err)
	}
}
