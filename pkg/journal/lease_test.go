package journal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func leaseUntil(t *testing.T, n *node, id string) (runner string, until time.Time) {
	t.Helper()
	var ms int64
	if err := n.db.QueryRow(`SELECT runner_id, lease_until_ms FROM workflows WHERE id = ?`, id).Scan(&runner, &ms); err != nil {
		t.Fatal(err)
	}
	return runner, fromMS(ms)
}

// A runner whose renewals fail must see its step's context cancelled, and the
// cancellation must come before another runner may start the step.
func TestLease_FailedRenewalCancelsTheStepBeforeAnotherRunnerStartsIt(t *testing.T) {
	stepStarted := make(chan struct{})
	aCtxDone := make(chan struct{})
	var bSawACancelled atomic.Bool
	var bAttempt atomic.Int32

	defA := Definition{Kind: "lease", Run: func(c *Ctx, _ json.RawMessage) error {
		return c.Step("order", nil, func(ctx context.Context, _ int) (any, error) {
			close(stepStarted)
			<-ctx.Done()
			close(aCtxDone)
			return nil, ctx.Err()
		})
	}}
	defB := Definition{Kind: "lease", Run: func(c *Ctx, _ json.RawMessage) error {
		return c.Step("order", nil, func(_ context.Context, attempt int) (any, error) {
			select {
			case <-aCtxDone:
				bSawACancelled.Store(true)
			default:
			}
			bAttempt.Store(int32(attempt))
			return "ok", nil
		})
	}}
	path := dbPath(t)
	a := newNode(t, path, t0, []Definition{defA})
	var failRenew atomic.Bool
	a.st.fault = func(op string) error {
		if op == "renew" && failRenew.Load() {
			return errors.New("disk I/O error")
		}
		return nil
	}
	id := mustStart(t, a.Engine, "lease", nil)
	aDone := make(chan struct{})
	go func() { defer close(aDone); a.execute(context.Background(), id) }()
	waitClosed(t, stepStarted, "A's step to start")

	failRenew.Store(true)
	if !a.clock.WaitForTimers(1, 5*time.Second) {
		t.Fatal("the renewer never armed its timer")
	}
	a.clock.Advance(testLease / 3) // the first renewal is due, and fails
	waitClosed(t, aCtxDone, "A's step context to be cancelled")
	waitClosed(t, aDone, "A's execute to return")

	if got := a.Stats().LeaseLost; got != 1 {
		t.Errorf("LeaseLost = %d, want 1", got)
	}
	recs := rawSteps(t, a.db, id)
	if len(recs) != 1 || !recs[0].FinishedAt.IsZero() || recs[0].Attempts != 1 {
		t.Fatalf("journal after the loss = %+v: the loser must write nothing further", recs)
	}
	if w := status(t, a, id); w.Status != StatusRunning {
		t.Fatalf("workflow = %+v: a runner that lost its lease must not finish or release it", w)
	}

	// Only now may the lease run out and another engine take over.
	b := newNode(t, path, t0.Add(testLease+testGrace), []Definition{defB})
	b.exec(id)
	if !bSawACancelled.Load() {
		t.Error("B started the step before A's step context was cancelled")
	}
	if bAttempt.Load() != 2 {
		t.Errorf("B ran the step as attempt %d, want 2", bAttempt.Load())
	}
	if w := status(t, b, id); w.Status != StatusSucceeded {
		t.Fatalf("workflow = %+v", w)
	}
}

// A step that outlives the lease keeps it alive through the renewer, which does
// not depend on the step returning.
func TestLease_LongStepKeepsRenewing(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var ctxErr atomic.Value
	def := Definition{Kind: "long", Run: func(c *Ctx, _ json.RawMessage) error {
		return c.Step("slow", nil, func(ctx context.Context, _ int) (any, error) {
			close(started)
			<-release
			ctxErr.Store(fmt.Sprint(ctx.Err()))
			return 1, nil
		})
	}}
	path := dbPath(t)
	a := newNode(t, path, t0, []Definition{def})
	id := mustStart(t, a.Engine, "long", nil)
	done := make(chan struct{})
	go func() { defer close(done); a.execute(context.Background(), id) }()
	waitClosed(t, started, "the step to start")

	for i := 1; i <= 9; i++ { // three lease lengths of renewals
		if !a.clock.WaitForTimers(1, 5*time.Second) {
			t.Fatal("renewer timer missing")
		}
		a.clock.Advance(testLease / 3)
		want := t0.Add(time.Duration(i)*testLease/3 + testLease)
		eventually(t, "the lease to be extended", func() bool {
			_, until := leaseUntil(t, a, id)
			return until.Equal(want)
		})
	}
	// Three lease lengths of fake time have passed, and nobody can take it over.
	b := newNode(t, path, a.clock.Now(), []Definition{def})
	if b.claimable(t, id) {
		t.Fatal("a workflow with a live lease is claimable")
	}
	close(release)
	waitClosed(t, done, "execute to return")
	if w := status(t, a, id); w.Status != StatusSucceeded {
		t.Fatalf("workflow = %+v", w)
	}
	if got := ctxErr.Load(); got != "<nil>" {
		t.Errorf("the step's context was %v while it ran", got)
	}
	if a.Stats().LeaseLost != 0 {
		t.Errorf("LeaseLost = %d", a.Stats().LeaseLost)
	}
}

// An abandoned engine: its clock never moves, so its renewer never fires and it
// never learns it lost the workflow. Its late write must be fenced out.
func TestLease_AbandonedRunnerIsFencedOut(t *testing.T) {
	aStarted := make(chan struct{})
	proceed := make(chan struct{})
	defA := Definition{Kind: "aband", Run: func(c *Ctx, _ json.RawMessage) error {
		var out string
		if err := c.Step("order", &out, func(context.Context, int) (any, error) {
			close(aStarted)
			<-proceed // ignores its context, as a careless step would
			return "from-A", nil
		}); err != nil {
			return err
		}
		return c.Step("after", nil, func(context.Context, int) (any, error) {
			t.Error("A ran a step after losing the workflow")
			return nil, nil
		})
	}}
	defB := Definition{Kind: "aband", Run: func(c *Ctx, _ json.RawMessage) error {
		var out string
		if err := c.Step("order", &out, func(_ context.Context, attempt int) (any, error) {
			if attempt != 2 {
				return nil, fmt.Errorf("B ran the step as attempt %d", attempt)
			}
			return "from-B", nil
		}); err != nil {
			return err
		}
		return c.Step("after", nil, func(context.Context, int) (any, error) { return nil, nil })
	}}
	path := dbPath(t)
	a := newNode(t, path, t0, []Definition{defA})
	id := mustStart(t, a.Engine, "aband", nil)
	aDone := make(chan struct{})
	go func() { defer close(aDone); a.execute(context.Background(), id) }()
	waitClosed(t, aStarted, "A's step to start")

	b := newNode(t, path, t0.Add(testLease+testGrace), []Definition{defB})
	b.exec(id)
	if w := status(t, b, id); w.Status != StatusSucceeded {
		t.Fatalf("B's run: %+v", w)
	}
	before := rawSteps(t, b.db, id)

	close(proceed) // A's step finally returns and tries to record it
	waitClosed(t, aDone, "A's execute to return")
	after := rawSteps(t, b.db, id)
	if fmt.Sprint(before) != fmt.Sprint(after) {
		t.Errorf("A changed the journal after losing the workflow:\nbefore %+v\nafter  %+v", before, after)
	}
	if string(after[0].Output) != `"from-B"` || after[0].Attempts != 2 {
		t.Errorf("step 0 = %+v, want B's output on attempt 2", after[0])
	}
	if a.Stats().LeaseLost != 1 {
		t.Errorf("A.LeaseLost = %d, want 1", a.Stats().LeaseLost)
	}
	if w := status(t, b, id); w.Status != StatusSucceeded {
		t.Errorf("A's late write disturbed the workflow: %+v", w)
	}
}

// Only one of many runners racing for one workflow wins the claim.
func TestLease_ClaimIsExclusive(t *testing.T) {
	def := Definition{Kind: "claim", Run: func(c *Ctx, _ json.RawMessage) error { return nil }}
	n := newNode(t, dbPath(t), t0, []Definition{def})
	id := mustStart(t, n.Engine, "claim", nil)
	var wg sync.WaitGroup
	var won atomic.Int32
	epochs := make(chan int64, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			epoch, ok, err := n.st.claim(context.Background(), id, fmt.Sprintf("runner-%d", i), n.kinds, t0, testLease, testGrace)
			if err != nil {
				t.Errorf("claim: %v", err)
				return
			}
			if ok {
				won.Add(1)
				epochs <- epoch
			}
		}()
	}
	wg.Wait()
	if won.Load() != 1 {
		t.Fatalf("%d runners won the claim, want 1", won.Load())
	}
	if e := <-epochs; e != 1 {
		t.Errorf("first epoch = %d, want 1", e)
	}
}

// Two engines on one database file, running at the same time: every step's side
// effect happens exactly once.
func TestLease_TwoEnginesRaceOverWorkflows(t *testing.T) {
	l := newLedger()
	def := Definition{Kind: "race", Run: func(c *Ctx, in json.RawMessage) error {
		var tag string
		_ = json.Unmarshal(in, &tag)
		for _, name := range []string{"one", "two", "three"} {
			if err := c.Step(name, nil, func(_ context.Context, attempt int) (any, error) {
				l.saw(tag+"/"+name, attempt)
				l.did(tag + "/" + name)
				return nil, nil
			}); err != nil {
				return err
			}
		}
		return nil
	}}
	path := dbPath(t)
	mk := func() *Engine {
		e, err := New(openAt(t, path), []Definition{def}, nil, WithTick(2*time.Millisecond), WithWaitPoll(2*time.Millisecond),
			WithShutdownWait(5*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	a, b := mk(), mk()
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for _, e := range []*Engine{a, b} {
		wg.Add(1)
		go func() { defer wg.Done(); _ = e.Run(ctx) }()
	}
	const n = 12
	ids := make([]string, n)
	for i := range ids {
		eng := a
		if i%2 == 1 {
			eng = b
		}
		ids[i] = mustStart(t, eng, "race", fmt.Sprintf("wf%d", i))
	}
	wctx, wcancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer wcancel()
	for _, id := range ids {
		w, err := a.Wait(wctx, id) // each Wait may be served by the other engine's run
		if err != nil || w.Status != StatusSucceeded {
			t.Fatalf("workflow %s: %+v (%v)", id, w, err)
		}
	}
	cancel()
	wg.Wait()
	for i := 0; i < n; i++ {
		for _, name := range []string{"one", "two", "three"} {
			key := fmt.Sprintf("wf%d/%s", i, name)
			if got := l.effect(key); got != 1 {
				t.Errorf("%s ran %d times, want exactly once", key, got)
			}
			if got := l.seen(key); len(got) != 1 || got[0] != 1 {
				t.Errorf("%s attempts = %v", key, got)
			}
		}
	}
	if got := a.Stats().Succeeded + b.Stats().Succeeded; got != n {
		t.Errorf("succeeded counters add up to %d, want %d", got, n)
	}
	if a.Stats().LeaseLost+b.Stats().LeaseLost != 0 {
		t.Error("a lease was lost although nothing failed")
	}
}

// An engine claims only kinds it has a definition for: a process that knows fewer
// kinds must not fail another's workflow as "unknown".
func TestLease_EngineClaimsOnlyItsOwnKinds(t *testing.T) {
	noop := func(*Ctx, json.RawMessage) error { return nil }
	path := dbPath(t)
	a := newNode(t, path, t0, []Definition{{Kind: "alpha", Run: noop}})
	b := newNode(t, path, t0, []Definition{{Kind: "beta", Run: noop}})
	id := mustStart(t, a.Engine, "alpha", nil)
	if b.claimable(t, id) {
		t.Error("B lists A's workflow as due")
	}
	b.exec(id)
	if w := status(t, b, id); w.Status != StatusPending {
		t.Errorf("B touched A's workflow: %+v", w)
	}
	a.exec(id)
	if w := status(t, a, id); w.Status != StatusSucceeded {
		t.Errorf("workflow = %+v", w)
	}
}

// Run's shutdown does not hang on a step that ignores its context, and the
// workflow stays claimed until its lease runs out.
func TestRun_ShutdownWithStepThatIgnoresItsContext(t *testing.T) {
	started := make(chan struct{})
	block := make(chan struct{})
	def := Definition{Kind: "stuck", Run: func(c *Ctx, _ json.RawMessage) error {
		return c.Step("stuck", nil, func(context.Context, int) (any, error) {
			close(started)
			<-block
			return 1, nil
		})
	}}
	path := dbPath(t)
	a := newNode(t, path, t0, []Definition{def}, WithShutdownWait(40*time.Millisecond))
	id := mustStart(t, a.Engine, "stuck", nil)
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() { defer close(runDone); _ = a.Run(ctx) }()
	waitClosed(t, started, "the step to start")
	began := time.Now()
	cancel()
	waitClosed(t, runDone, "Run to return")
	if d := time.Since(began); d > 3*time.Second {
		t.Errorf("Run took %s to return", d)
	}

	runner, until := leaseUntil(t, a, id)
	if w := status(t, a, id); w.Status != StatusRunning || runner != a.RunnerID() {
		t.Fatalf("workflow = %+v runner %q: it must stay claimed", w, runner)
	}
	// The renewer is stopped with Run, so the lease runs out and can be taken over.
	a.clock.Advance(testLease / 2)
	time.Sleep(20 * time.Millisecond)
	if _, now := leaseUntil(t, a, id); !now.Equal(until) {
		t.Errorf("the lease of an abandoned step was renewed after Run returned (%v -> %v)", until, now)
	}
	b := newNode(t, path, t0.Add(testLease), []Definition{def})
	if b.claimable(t, id) {
		t.Error("claimable before the grace period")
	}
	b.clock.Advance(testGrace)
	if !b.claimable(t, id) {
		t.Error("not claimable after lease and grace")
	}
	close(block) // let the zombie finish
	eventually(t, "the zombie step to wind down", func() bool { return a.Stats().Running == 0 })
}

// A step that honours its context and is still running when the shutdown wait is
// over is cancelled; the error it returns for that is the engine's doing and must
// not be journaled as a failure (or the workflow would fail on every restart). The
// workflow is released, and a new engine resumes the step as attempt 2 at once.
func TestRun_ShutdownCancelsACooperativeStepWithoutFailingIt(t *testing.T) {
	started := make(chan struct{})
	l := newLedger()
	var run atomic.Int32
	def := Definition{Kind: "coop", Run: func(c *Ctx, _ json.RawMessage) error {
		return c.Step("slow", nil, func(ctx context.Context, attempt int) (any, error) {
			l.saw("slow", attempt)
			if run.Add(1) == 1 {
				close(started)
				<-ctx.Done()
				return nil, fmt.Errorf("order interrupted: %w", ctx.Err())
			}
			return "done", nil
		})
	}}
	path := dbPath(t)
	a := newNode(t, path, t0, []Definition{def}, WithShutdownWait(30*time.Millisecond))
	id := mustStart(t, a.Engine, "coop", nil)
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() { defer close(runDone); _ = a.Run(ctx) }()
	waitClosed(t, started, "the step to start")
	cancel()
	waitClosed(t, runDone, "Run to return")
	eventually(t, "the interrupted worker to release the workflow", func() bool {
		return status(t, a, id).Status == StatusPending
	})

	runner, _ := leaseUntil(t, a, id)
	recs := rawSteps(t, a.db, id)
	if runner != "" || len(recs) != 1 || !recs[0].FinishedAt.IsZero() || recs[0].Attempts != 1 || recs[0].Error != "" {
		t.Fatalf("runner %q journal %+v: the step must stay started (no failure journaled) and the workflow unowned", runner, recs)
	}
	if a.Stats().Failed != 0 {
		t.Errorf("stats = %+v", a.Stats())
	}

	b := newNode(t, path, t0, []Definition{def}) // same instant: no lease to wait for
	b.exec(id)
	if w := status(t, b, id); w.Status != StatusSucceeded {
		t.Fatalf("after the restart: %+v", w)
	}
	if got := l.seen("slow"); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Errorf("attempts = %v, want [1 2]", got)
	}
}

func TestRun_ResumesDueWorkflowsOnTick_AndWaitFromAnotherEngine(t *testing.T) {
	l := newLedger()
	def := Definition{Kind: "tick", Run: func(c *Ctx, _ json.RawMessage) error {
		if err := c.Step("a", nil, func(context.Context, int) (any, error) { l.did("a"); return nil, nil }); err != nil {
			return err
		}
		if err := c.SleepUntil(t0.Add(time.Hour)); err != nil {
			return err
		}
		return c.Step("b", nil, func(context.Context, int) (any, error) { l.did("b"); return nil, nil })
	}}
	path := dbPath(t)
	runner := newNode(t, path, t0, []Definition{def})
	watcher := newNode(t, path, t0, []Definition{def}) // never runs anything
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() { defer close(runDone); _ = runner.Run(ctx) }()

	eventually(t, "Run to start", func() bool {
		runner.mu.Lock()
		defer runner.mu.Unlock()
		return runner.running
	})
	if err := runner.Run(ctx); !errors.Is(err, ErrAlreadyRunning) {
		t.Errorf("a second Run: %v, want ErrAlreadyRunning", err)
	}
	id := mustStart(t, watcher.Engine, "tick", nil) // started in a process that does not run it
	// The runner learns of a workflow another process started at its next scan, which
	// its fake clock only reaches when ticked (a second at a time, far below the lease).
	eventually(t, "the workflow to start waiting", func() bool {
		runner.clock.Advance(time.Second)
		return status(t, runner, id).Status == StatusWaiting
	})

	wctx, wcancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	if _, err := watcher.Wait(wctx, id); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Wait on a sleeping workflow: %v, want the context's error", err)
	}
	wcancel()

	waitCtx, waitCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer waitCancel()
	res := make(chan Workflow, 1)
	go func() {
		w, err := watcher.Wait(waitCtx, id)
		if err != nil {
			t.Errorf("Wait: %v", err)
		}
		res <- w
	}()
	// The runner's clock moves to the wake time; the next tick resumes the workflow.
	// (One jump to the wake time, then ticks: a clock that jumps by more than the
	// lease while a step runs makes the runner give up its lease, by design.)
	runner.clock.Advance(time.Hour)
	deadline := time.Now().Add(10 * time.Second)
	for status(t, runner, id).Status != StatusSucceeded && time.Now().Before(deadline) {
		runner.clock.Advance(time.Second)
		time.Sleep(time.Millisecond)
	}
	select {
	case w := <-res:
		if w.Status != StatusSucceeded {
			t.Errorf("Wait returned %+v", w)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Wait from the other engine never returned")
	}
	cancel()
	waitClosed(t, runDone, "Run to return")
	if l.effect("a") != 1 || l.effect("b") != 1 {
		t.Errorf("effects a=%d b=%d", l.effect("a"), l.effect("b"))
	}
}

// A renewal that finds the lease gone because this runner itself just suspended or
// released the workflow is not a loss; one that finds another owner is.
func TestLease_RenewalTellsOwnReleaseFromTakeover(t *testing.T) {
	for _, tc := range []struct {
		name     string
		sql      string
		wantLost uint64
	}{
		{"own release", `UPDATE workflows SET status='waiting', wake_at_ms=1, runner_id='', lease_until_ms=0 WHERE id=?`, 0},
		{"takeover", `UPDATE workflows SET runner_id='someone-else', epoch=epoch+1 WHERE id=?`, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			started := make(chan struct{})
			release := make(chan struct{})
			cancelled := make(chan struct{})
			def := Definition{Kind: "rel", Run: func(c *Ctx, _ json.RawMessage) error {
				return c.Step("s", nil, func(ctx context.Context, _ int) (any, error) {
					close(started)
					select {
					case <-ctx.Done():
						close(cancelled)
					case <-release:
					}
					return nil, ctx.Err()
				})
			}}
			a := newNode(t, dbPath(t), t0, []Definition{def})
			id := mustStart(t, a.Engine, "rel", nil)
			done := make(chan struct{})
			go func() { defer close(done); a.execute(context.Background(), id) }()
			waitClosed(t, started, "the step to start")
			exec(t, a.db, tc.sql, id)
			if !a.clock.WaitForTimers(1, 5*time.Second) {
				t.Fatal("renewer timer missing")
			}
			a.clock.Advance(testLease / 3)
			if tc.wantLost == 1 {
				waitClosed(t, cancelled, "the step context to be cancelled after a takeover")
			} else {
				time.Sleep(30 * time.Millisecond) // let the renewer take its decision
				select {
				case <-cancelled:
					t.Fatal("the step was cancelled by the owner's own release")
				default:
				}
			}
			// Read before the step returns: a step that returns after its workflow was
			// released from under it is fenced out on its own, which is a different count.
			if got := a.Stats().LeaseLost; got != tc.wantLost {
				t.Errorf("LeaseLost = %d, want %d", got, tc.wantLost)
			}
			close(release)
			waitClosed(t, done, "execute to return")
		})
	}
}
