package journal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

type stepOut struct {
	N     int64   `json:"n"`
	Ratio float64 `json:"ratio"`
	Tag   string  `json:"tag"`
}

func TestStep_ReplaySkipsCompleted(t *testing.T) {
	l := newLedger()
	var firstRun, replay []stepOut
	phase := &firstRun
	def := Definition{Kind: "replay", Run: func(c *Ctx, _ json.RawMessage) error {
		for _, name := range []string{"a", "b", "c"} {
			var out stepOut
			err := c.Step(name, &out, func(_ context.Context, attempt int) (any, error) {
				l.saw(name, attempt)
				l.did(name)
				return stepOut{N: 1 << 40, Ratio: 0.5, Tag: name}, nil
			})
			if err != nil {
				return err
			}
			*phase = append(*phase, out)
		}
		return nil
	}}
	path := dbPath(t)
	n := newNode(t, path, t0, []Definition{def})
	id := mustStart(t, n.Engine, "replay", nil)
	n.exec(id)
	if w := status(t, n, id); w.Status != StatusSucceeded {
		t.Fatalf("first run: %+v", w)
	}

	// Force a replay of the finished journal: put the workflow back as pending.
	exec(t, n.db, `UPDATE workflows SET status='pending', finished_at_ms=NULL WHERE id=?`, id)
	phase = &replay
	n.exec(id)
	if w := status(t, n, id); w.Status != StatusSucceeded {
		t.Fatalf("replay: %+v", w)
	}
	for _, name := range []string{"a", "b", "c"} {
		if got := l.effect(name); got != 1 {
			t.Errorf("step %s ran %d times, want exactly once", name, got)
		}
	}
	if len(firstRun) != 3 || len(replay) != 3 {
		t.Fatalf("outputs: first %v replay %v", firstRun, replay)
	}
	for i := range firstRun {
		if firstRun[i] != replay[i] {
			t.Errorf("step %d: first run saw %+v, replay saw %+v", i, firstRun[i], replay[i])
		}
	}
	if got := n.Stats().StepsReplayed; got != 3 {
		t.Errorf("StepsReplayed = %d, want 3", got)
	}
}

func TestStep_CrashMidStep_RetriesWithAttempt2(t *testing.T) {
	l := newLedger()
	x := newExchange()
	var firstStarted, secondStarted time.Time
	var keys []string
	def := Definition{Kind: "mid", Run: func(c *Ctx, _ json.RawMessage) error {
		if err := orderStep(c, l, x, "before"); err != nil {
			return err
		}
		key := c.IdempotencyKey("order")
		return c.Step("order", nil, func(_ context.Context, attempt int) (any, error) {
			l.saw("order", attempt)
			keys = append(keys, c.IdempotencyKey("order"))
			switch attempt {
			case 1:
				firstStarted = c.StepStartedAt()
			default:
				secondStarted = c.StepStartedAt()
			}
			if attempt > 1 && x.has(key) {
				return "found", nil
			}
			l.did("order")
			x.place(key)
			return "placed", nil
		})
	}}
	path := dbPath(t)
	start := t0.Add(1234567 * time.Nanosecond) // not a whole millisecond: the real clock never is
	a := newNode(t, path, start, []Definition{def})
	id := mustStart(t, a.Engine, "mid", nil)
	// The process dies after the order was placed and before its output was
	// journaled: the worst place.
	a.armKill(func(point, step string, _ int) bool { return point == pointAfterFn && step == "order" })
	a.exec(id)
	if !a.kill.dead.Load() {
		t.Fatal("the crash point was never reached")
	}

	b := newNode(t, path, t0.Add(time.Hour), []Definition{def}) // long after the lease and grace
	b.exec(id)
	if w := status(t, b, id); w.Status != StatusSucceeded {
		t.Fatalf("after the restart: %+v", w)
	}
	if got := l.seen("order"); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("attempts seen by the cut step = %v, want [1 2]", got)
	}
	if got := l.effect("order"); got != 1 {
		t.Errorf("the order was placed %d times, want once", got)
	}
	if want := fromMS(ms(start)); !firstStarted.Equal(want) || !secondStarted.Equal(firstStarted) {
		t.Errorf("StepStartedAt: attempt 1 saw %v, attempt 2 saw %v, want the first start %v on both", firstStarted, secondStarted, want)
	}
	if len(keys) != 2 || keys[0] != keys[1] {
		t.Errorf("idempotency keys differ between attempts: %v", keys)
	}
	recs := rawSteps(t, b.db, id)
	if len(recs) != 2 || recs[1].Attempts != 2 || recs[1].FinishedAt.IsZero() {
		t.Errorf("journal = %+v", recs)
	}
}

func TestNow_StableAcrossReplay(t *testing.T) {
	var seen []time.Time
	def := Definition{Kind: "now", Run: func(c *Ctx, _ json.RawMessage) error {
		seen = append(seen, c.Now())
		return c.Step("work", nil, func(context.Context, int) (any, error) { return nil, nil })
	}}
	path := dbPath(t)
	a := newNode(t, path, t0, []Definition{def})
	id := mustStart(t, a.Engine, "now", nil)
	a.armKill(func(point, step string, _ int) bool { return point == pointAfterStarted && step == "work" })
	a.exec(id)

	b := newNode(t, path, t0.Add(2*time.Hour), []Definition{def})
	b.exec(id)
	if len(seen) != 2 {
		t.Fatalf("function ran %d times, want 2", len(seen))
	}
	if !seen[0].Equal(t0) || !seen[1].Equal(seen[0]) {
		t.Errorf("Now() = %v then %v after a replay two hours later, want %v both times", seen[0], seen[1], t0)
	}
}

func TestOutputSizeLimit(t *testing.T) {
	big := strings.Repeat("x", MaxOutputBytes+1)
	l := newLedger()
	def := Definition{Kind: "big", Run: func(c *Ctx, _ json.RawMessage) error {
		if err := c.Step("huge", nil, func(context.Context, int) (any, error) {
			l.did("huge")
			return big, nil
		}); err != nil {
			return err
		}
		return c.Step("after", nil, func(context.Context, int) (any, error) { l.did("after"); return 1, nil })
	}}
	n := newNode(t, dbPath(t), t0, []Definition{def})
	id := mustStart(t, n.Engine, "big", nil)
	n.exec(id)
	w := status(t, n, id)
	if w.Status != StatusFailed || !strings.Contains(w.Error, `"huge"`) || !strings.Contains(w.Error, "too large") {
		t.Fatalf("workflow = %+v, want failed with an error naming the step", w)
	}
	if l.effect("after") != 0 {
		t.Error("a step ran after the oversize one")
	}
	recs := rawSteps(t, n.db, id)
	if len(recs) != 1 || recs[0].FinishedAt.IsZero() || len(recs[0].Output) != 0 {
		t.Errorf("journal = %+v: the step must be finished, with an error and no output", recs)
	}
	// A restart must not run the step again.
	exec(t, n.db, `UPDATE workflows SET status='pending', finished_at_ms=NULL WHERE id=?`, id)
	n.exec(id)
	if got := l.effect("huge"); got != 1 {
		t.Errorf("the oversize step ran %d times, want once", got)
	}

	// An output that is not JSON fails the same way.
	def2 := Definition{Kind: "chan", Run: func(c *Ctx, _ json.RawMessage) error {
		return c.Step("bad", nil, func(context.Context, int) (any, error) { return make(chan int), nil })
	}}
	m := newNode(t, dbPath(t), t0, []Definition{def2})
	id2 := mustStart(t, m.Engine, "chan", nil)
	m.exec(id2)
	if w := status(t, m, id2); w.Status != StatusFailed || !strings.Contains(w.Error, "JSON") {
		t.Errorf("unserialisable output: %+v", w)
	}

	// The input limit.
	if _, err := n.Start(context.Background(), "big", strings.Repeat("y", MaxInputBytes)); !errors.Is(err, ErrInputTooLarge) {
		t.Errorf("Start with a 64 KB+ input: %v, want ErrInputTooLarge", err)
	}
}

func TestSleepUntil_ResumesAfterRestart(t *testing.T) {
	l := newLedger()
	var woke []time.Time
	def := Definition{Kind: "sleep", Run: func(c *Ctx, _ json.RawMessage) error {
		if err := c.Step("a", nil, func(context.Context, int) (any, error) { l.did("a"); return nil, nil }); err != nil {
			return err
		}
		if err := c.SleepUntil(c.Now().Add(time.Hour)); err != nil {
			return err
		}
		woke = append(woke, c.Now())
		return c.Step("b", nil, func(context.Context, int) (any, error) { l.did("b"); return nil, nil })
	}}
	path := dbPath(t)
	a := newNode(t, path, t0, []Definition{def})
	id := mustStart(t, a.Engine, "sleep", nil)
	a.exec(id)
	w := status(t, a, id)
	if w.Status != StatusWaiting || w.WakeAt == nil || !w.WakeAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("after the first run: %+v, want waiting until %v", w, t0.Add(time.Hour))
	}
	if w.Status.Terminal() || a.claimable(t, id) {
		t.Error("a sleeping workflow must not be due")
	}

	// A restart half way through the sleep: not due, and the wake time is unchanged.
	b := newNode(t, path, t0.Add(30*time.Minute), []Definition{def})
	if b.claimable(t, id) {
		t.Fatal("due after 30 minutes of a 60 minute sleep")
	}
	b.exec(id) // even if something forced a run, the claim refuses
	if l.effect("b") != 0 || status(t, b, id).Status != StatusWaiting {
		t.Fatal("the workflow ran before its wake time")
	}
	b.clock.Advance(30*time.Minute - time.Millisecond)
	if b.claimable(t, id) {
		t.Fatal("due 1ms before the wake time")
	}
	b.clock.Advance(time.Millisecond)
	if !b.claimable(t, id) {
		t.Fatal("not due at the wake time")
	}
	b.exec(id)
	if w := status(t, b, id); w.Status != StatusSucceeded {
		t.Fatalf("after waking: %+v", w)
	}
	if l.effect("a") != 1 || l.effect("b") != 1 {
		t.Errorf("effects a=%d b=%d, want 1 each", l.effect("a"), l.effect("b"))
	}
	if len(woke) != 1 || !woke[0].After(t0) {
		t.Errorf("Now() after the sleep = %v", woke)
	}
}

func TestSleepUntil_PastTimeDoesNotSuspend_AndSwallowedSuspendRunsNothing(t *testing.T) {
	l := newLedger()
	def := Definition{Kind: "sleep2", Run: func(c *Ctx, _ json.RawMessage) error {
		if err := c.SleepUntil(t0.Add(-time.Hour)); err != nil { // already past
			return err
		}
		_ = c.SleepUntil(t0.Add(time.Hour)) // suspends; an author who ignores the error ...
		_ = c.Step("must-not-run", nil, func(context.Context, int) (any, error) { l.did("must-not-run"); return nil, nil })
		return nil // ... and returns nil must not turn the workflow into a success
	}}
	n := newNode(t, dbPath(t), t0, []Definition{def})
	id := mustStart(t, n.Engine, "sleep2", nil)
	n.exec(id)
	if w := status(t, n, id); w.Status != StatusWaiting {
		t.Fatalf("workflow = %+v, want waiting", w)
	}
	if l.effect("must-not-run") != 0 {
		t.Error("a step ran after the workflow was suspended")
	}
}

func TestCancel_AtBoundary(t *testing.T) {
	l := newLedger()
	var n *node
	var id string
	var cancelErr error
	def := Definition{Kind: "cancel", Protective: true, Run: func(c *Ctx, _ json.RawMessage) error {
		if err := c.Step("one", nil, func(context.Context, int) (any, error) {
			l.did("one")
			cancelErr = n.Cancel(context.Background(), id, "stop please") // while step one runs
			return nil, nil
		}); err != nil {
			return err
		}
		return c.Step("two", nil, func(context.Context, int) (any, error) { l.did("two"); return nil, nil })
	}}
	n = newNode(t, dbPath(t), t0, []Definition{def})
	id = mustStart(t, n.Engine, "cancel", nil)
	n.exec(id)
	if cancelErr != nil {
		t.Fatalf("Cancel: %v", cancelErr)
	}
	w := status(t, n, id)
	if w.Status != StatusCancelled || w.Error != "stop please" {
		t.Fatalf("workflow = %+v, want cancelled with the reason", w)
	}
	if l.effect("one") != 1 || l.effect("two") != 0 {
		t.Errorf("effects one=%d two=%d: the running step finishes, the next one never starts", l.effect("one"), l.effect("two"))
	}
	if !w.Protective {
		t.Error("the protective flag was not stored")
	}
	if n.Stats().Cancelled != 1 {
		t.Errorf("stats = %+v", n.Stats())
	}
	if err := n.Cancel(context.Background(), id, "again"); err != nil {
		t.Errorf("cancelling a cancelled workflow: %v", err)
	}
	if err := n.Cancel(context.Background(), "wf_nope", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown id: %v", err)
	}
}

func TestCancel_DurableAcrossRestart(t *testing.T) {
	l := newLedger()
	var a *node
	var id string
	def := Definition{Kind: "cancel2", Run: func(c *Ctx, _ json.RawMessage) error {
		for _, name := range []string{"one", "two", "three"} {
			if err := c.Step(name, nil, func(context.Context, int) (any, error) {
				l.did(name)
				if name == "one" {
					if err := a.Cancel(context.Background(), id, "kill it"); err != nil {
						return nil, err
					}
				}
				return nil, nil
			}); err != nil {
				return err
			}
		}
		return nil
	}}
	path := dbPath(t)
	a = newNode(t, path, t0, []Definition{def})
	id = mustStart(t, a.Engine, "cancel2", nil)
	// The process dies right after step one's output is journaled and the cancel
	// request is stored, before the boundary is reached.
	a.armKill(func(point, step string, _ int) bool { return point == pointAfterOutput && step == "one" })
	a.exec(id)
	if w := status(t, a, id); w.Status != StatusRunning || !w.CancelRequested {
		t.Fatalf("after the crash: %+v, want running with a stored cancel request", w)
	}

	b := newNode(t, path, t0.Add(time.Hour), []Definition{def})
	b.exec(id)
	if w := status(t, b, id); w.Status != StatusCancelled || w.Error != "kill it" {
		t.Fatalf("after the restart: %+v, want cancelled", w)
	}
	if l.effect("one") != 1 || l.effect("two") != 0 || l.effect("three") != 0 {
		t.Errorf("effects after a durable cancel: one=%d two=%d three=%d", l.effect("one"), l.effect("two"), l.effect("three"))
	}
	// Cancelled stays cancelled, however many engines come and go.
	c := newNode(t, path, t0.Add(48*time.Hour), []Definition{def})
	if c.claimable(t, id) {
		t.Error("a cancelled workflow is due again")
	}
}

func TestCancel_NotRunningFinishesAtOnce(t *testing.T) {
	def := Definition{Kind: "idle", Protective: true, Run: func(c *Ctx, _ json.RawMessage) error {
		return c.SleepUntil(t0.Add(time.Hour))
	}}
	n := newNode(t, dbPath(t), t0, []Definition{def})
	pending := mustStart(t, n.Engine, "idle", nil)
	if err := n.Cancel(context.Background(), pending, "never mind"); err != nil {
		t.Fatal(err)
	}
	if w := status(t, n, pending); w.Status != StatusCancelled {
		t.Errorf("pending workflow: %+v", w)
	}
	waiting := mustStart(t, n.Engine, "idle", nil)
	n.exec(waiting)
	if w := status(t, n, waiting); w.Status != StatusWaiting {
		t.Fatalf("setup: %+v", w)
	}
	if err := n.Cancel(context.Background(), waiting, "protective ones can be cancelled by id"); err != nil {
		t.Fatal(err)
	}
	if w := status(t, n, waiting); w.Status != StatusCancelled {
		t.Errorf("waiting workflow: %+v", w)
	}
	n.clock.Advance(2 * time.Hour)
	if n.claimable(t, waiting) || n.claimable(t, pending) {
		t.Error("a cancelled workflow was resumed")
	}
	done := mustStart(t, n.Engine, "idle", nil)
	exec(t, n.db, `UPDATE workflows SET status='succeeded', finished_at_ms=1 WHERE id=?`, done)
	if err := n.Cancel(context.Background(), done, ""); !errors.Is(err, ErrAlreadyFinished) {
		t.Errorf("cancelling a succeeded workflow: %v", err)
	}
}

func TestBackoff_PersistedAcrossRestarts(t *testing.T) {
	l := newLedger()
	def := Definition{Kind: "flaky", Run: func(c *Ctx, _ json.RawMessage) error {
		return c.Step("call", nil, func(_ context.Context, attempt int) (any, error) {
			l.saw("call", attempt)
			if attempt < 3 {
				return nil, fmt.Errorf("exchange said no (%d)", attempt)
			}
			return "ok", nil
		}, Retries(3), Backoff(time.Minute))
	}}
	path := dbPath(t)
	a := newNode(t, path, t0, []Definition{def})
	id := mustStart(t, a.Engine, "flaky", nil)
	a.exec(id)
	w := status(t, a, id)
	if w.Status != StatusWaiting || w.WakeAt == nil || !w.WakeAt.Equal(t0.Add(time.Minute)) {
		t.Fatalf("after attempt 1: %+v, want waiting until +1m", w)
	}

	// A restart 30 s into the first backoff does not shorten it.
	b := newNode(t, path, t0.Add(30*time.Second), []Definition{def})
	if b.claimable(t, id) {
		t.Fatal("due in the middle of the backoff")
	}
	b.clock.Advance(30 * time.Second) // +60s: due
	if !b.claimable(t, id) {
		t.Fatal("not due after the backoff")
	}
	b.exec(id)
	w = status(t, b, id)
	if w.Status != StatusWaiting || !w.WakeAt.Equal(t0.Add(time.Minute+2*time.Minute)) {
		t.Fatalf("after attempt 2: %+v, want a doubled backoff (+2m) from the failure time", w)
	}

	// A claim that races ahead of the persisted wake time re-suspends instead of running.
	c := newNode(t, path, t0.Add(2*time.Minute), []Definition{def})
	exec(t, c.db, `UPDATE workflows SET status='pending' WHERE id=?`, id) // as if a stop had released it
	c.exec(id)
	if got := l.seen("call"); len(got) != 2 {
		t.Fatalf("attempts after the early claim = %v, the persisted backoff was shortened", got)
	}
	if w := status(t, c, id); w.Status != StatusWaiting || !w.WakeAt.Equal(t0.Add(3*time.Minute)) {
		t.Fatalf("after the early claim: %+v", w)
	}

	d := newNode(t, path, t0.Add(3*time.Minute), []Definition{def})
	d.exec(id)
	if w := status(t, d, id); w.Status != StatusSucceeded {
		t.Fatalf("final: %+v", w)
	}
	if got := l.seen("call"); len(got) != 3 || got[2] != 3 {
		t.Errorf("attempts = %v, want [1 2 3]", got)
	}
	recs := rawSteps(t, d.db, id)
	if len(recs) != 1 || recs[0].Attempts != 3 || recs[0].Failures != 2 {
		t.Errorf("journal = %+v", recs)
	}
}

func TestRetries_OptInBoundedAndNonRetryable(t *testing.T) {
	l := newLedger()
	permanent := errors.New("insufficient funds")
	def := Definition{Kind: "retry", Run: func(c *Ctx, in json.RawMessage) error {
		switch string(in) {
		case `"default"`: // no retries unless asked
			return c.Step("s", nil, func(_ context.Context, a int) (any, error) { l.saw("default", a); return nil, errors.New("boom") })
		case `"permanent"`:
			return c.Step("s", nil, func(_ context.Context, a int) (any, error) { l.saw("permanent", a); return nil, permanent },
				Retries(5), NonRetryable(func(err error) bool { return errors.Is(err, permanent) }))
		case `"exhausted"`: // immediate retries (no backoff) stay in the same run
			return c.Step("s", nil, func(_ context.Context, a int) (any, error) { l.saw("exhausted", a); return nil, errors.New("again") },
				Retries(2))
		}
		return nil
	}}
	n := newNode(t, dbPath(t), t0, []Definition{def})
	for name, want := range map[string]int{"default": 1, "permanent": 1, "exhausted": 3} {
		id := mustStart(t, n.Engine, "retry", name)
		n.exec(id)
		w := status(t, n, id)
		if w.Status != StatusFailed {
			t.Errorf("%s: %+v, want failed", name, w)
		}
		if got := len(l.seen(name)); got != want {
			t.Errorf("%s: step ran %d times, want %d", name, got, want)
		}
	}
}

func TestFailedStepIsReplayedNotRerun_AndHandledErrorsContinue(t *testing.T) {
	l := newLedger()
	var errs []string
	def := Definition{Kind: "comp", Run: func(c *Ctx, _ json.RawMessage) error {
		err := c.Step("risky", nil, func(context.Context, int) (any, error) {
			l.did("risky")
			return nil, errors.New("rejected by the venue")
		})
		var se *StepError
		if !errors.As(err, &se) {
			return fmt.Errorf("want a StepError, got %v", err)
		}
		errs = append(errs, se.Error())
		return c.Step("undo", nil, func(context.Context, int) (any, error) { l.did("undo"); return nil, nil })
	}}
	path := dbPath(t)
	a := newNode(t, path, t0, []Definition{def})
	id := mustStart(t, a.Engine, "comp", nil)
	a.armKill(func(point, step string, _ int) bool { return point == pointAfterStarted && step == "undo" })
	a.exec(id)
	b := newNode(t, path, t0.Add(time.Hour), []Definition{def})
	b.exec(id)
	if w := status(t, b, id); w.Status != StatusSucceeded {
		t.Fatalf("workflow = %+v", w)
	}
	if l.effect("risky") != 1 || l.effect("undo") != 1 {
		t.Errorf("risky=%d undo=%d, want once each", l.effect("risky"), l.effect("undo"))
	}
	if len(errs) != 2 || errs[0] != errs[1] {
		t.Errorf("the error seen live and on replay differ: %v", errs)
	}
}

func TestNonDeterministic_Detected(t *testing.T) {
	l := newLedger()
	mk := func(names ...string) Definition {
		return Definition{Kind: "det", Run: func(c *Ctx, _ json.RawMessage) error {
			for _, name := range names {
				if err := c.Step(name, nil, func(context.Context, int) (any, error) { l.did(name); return nil, nil }); err != nil {
					return err
				}
			}
			return nil
		}}
	}
	for _, tc := range []struct {
		name string
		v2   Definition
	}{
		{"renamed step", mk("a", "x", "c")},
		{"removed step", mk("a", "b")},
		{"reordered", mk("b", "a", "c")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := dbPath(t)
			a := newNode(t, path, t0, []Definition{mk("a", "b", "c")})
			id := mustStart(t, a.Engine, "det", nil)
			a.armKill(func(point, step string, _ int) bool { return point == pointAfterStarted && step == "c" })
			a.exec(id)
			before := map[string]int{"a": l.effect("a"), "b": l.effect("b"), "c": l.effect("c"), "x": l.effect("x")}

			b := newNode(t, path, t0.Add(time.Hour), []Definition{tc.v2})
			b.exec(id)
			w := status(t, b, id)
			if w.Status != StatusFailed || !strings.Contains(w.Error, ErrNonDeterministic.Error()[:30]) {
				t.Fatalf("workflow = %+v, want failed with a non-determinism error", w)
			}
			for k, v := range before {
				if l.effect(k) != v {
					t.Errorf("step %s ran after non-determinism was detected (%d -> %d)", k, v, l.effect(k))
				}
			}
		})
	}
}

func TestPanics_FailTheWorkflowNotTheProcess(t *testing.T) {
	l := newLedger()
	def := Definition{Kind: "panic", Run: func(c *Ctx, in json.RawMessage) error {
		if string(in) == `"workflow"` {
			panic("kaboom in the workflow function")
		}
		if err := c.Step("boom", nil, func(context.Context, int) (any, error) {
			l.did("boom")
			panic("kaboom in a step")
		}); err != nil {
			return nil // swallowed: the panic still fails the workflow
		}
		return c.Step("later", nil, func(context.Context, int) (any, error) { l.did("later"); return nil, nil })
	}}
	n := newNode(t, dbPath(t), t0, []Definition{def})
	idW := mustStart(t, n.Engine, "panic", "workflow")
	idS := mustStart(t, n.Engine, "panic", "step")
	n.exec(idW)
	n.exec(idS)
	for _, id := range []string{idW, idS} {
		if w := status(t, n, id); w.Status != StatusFailed || !strings.Contains(w.Error, "panic") {
			t.Errorf("workflow %s = %+v, want failed by a panic", id, w)
		}
	}
	if n.Stats().Panics != 2 || n.Stats().Failed != 2 {
		t.Errorf("stats = %+v", n.Stats())
	}
	if l.effect("later") != 0 {
		t.Error("a step ran after a panic in a step")
	}
	// The panicked step is never run again.
	exec(t, n.db, `UPDATE workflows SET status='pending', finished_at_ms=NULL WHERE id=?`, idS)
	n.exec(idS)
	if l.effect("boom") != 1 {
		t.Errorf("the panicking step ran %d times", l.effect("boom"))
	}
}

func TestStepCountLimit(t *testing.T) {
	l := newLedger()
	def := Definition{Kind: "loop", Run: func(c *Ctx, _ json.RawMessage) error {
		for i := 0; ; i++ { // a runaway loop
			if err := c.Step(fmt.Sprintf("s%d", i), nil, func(context.Context, int) (any, error) { l.did("step"); return i, nil }); err != nil {
				return err
			}
		}
	}}
	n := newNode(t, dbPath(t), t0, []Definition{def})
	id := mustStart(t, n.Engine, "loop", nil)
	n.exec(id)
	w := status(t, n, id)
	if w.Status != StatusFailed || !strings.Contains(w.Error, "step limit") {
		t.Fatalf("workflow = %+v, want failed by the step limit", w)
	}
	if got := l.effect("step"); got != MaxSteps {
		t.Errorf("%d steps ran, want exactly %d", got, MaxSteps)
	}
	if got := len(rawSteps(t, n.db, id)); got != MaxSteps {
		t.Errorf("%d journal rows, want %d", got, MaxSteps)
	}
}

func TestAttemptCapStopsACrashLoop(t *testing.T) {
	l := newLedger()
	def := Definition{Kind: "poison", Run: func(c *Ctx, _ json.RawMessage) error {
		return c.Step("poison", nil, func(_ context.Context, a int) (any, error) { l.saw("poison", a); return nil, nil })
	}}
	path := dbPath(t)
	n := newNode(t, path, t0, []Definition{def})
	id := mustStart(t, n.Engine, "poison", nil)
	n.armKill(func(point, _ string, _ int) bool { return point == pointAfterStarted })
	n.exec(id)
	// Pretend the step has already killed the process MaxStepAttempts times.
	exec(t, n.db, `UPDATE workflow_steps SET attempts=? WHERE workflow_id=?`, MaxStepAttempts, id)
	b := newNode(t, path, t0.Add(time.Hour), []Definition{def})
	b.exec(id)
	w := status(t, b, id)
	if w.Status != StatusFailed || !strings.Contains(w.Error, "giving up") {
		t.Fatalf("workflow = %+v", w)
	}
	if len(l.seen("poison")) != 0 {
		t.Errorf("the step ran again after hitting the attempt cap: %v", l.seen("poison"))
	}
}

func TestNamesAndDefinitionsAreValidated(t *testing.T) {
	good := func(c *Ctx, _ json.RawMessage) error { return nil }
	db := openAt(t, dbPath(t))
	for _, kind := range []string{"", "has space", "semi;colon", "$now", ".dot", strings.Repeat("k", 65), "unié"} {
		if _, err := New(db, []Definition{{Kind: kind, Run: good}}, nil); !errors.Is(err, ErrInvalidName) {
			t.Errorf("kind %q: %v, want ErrInvalidName", kind, err)
		}
	}
	if _, err := New(db, []Definition{{Kind: "a.b-c_d9", Run: good}}, nil); err != nil {
		t.Errorf("a valid kind was refused: %v", err)
	}
	if _, err := New(db, []Definition{{Kind: "x"}}, nil); err == nil {
		t.Error("a definition without Run was accepted")
	}
	if _, err := New(db, []Definition{{Kind: "x", Run: good}, {Kind: "x", Run: good}}, nil); err == nil {
		t.Error("a duplicate kind was accepted")
	}
	if _, err := New(nil, nil, nil); err == nil {
		t.Error("a nil database was accepted")
	}

	n := newNode(t, dbPath(t), t0, []Definition{{Kind: "names", Run: func(c *Ctx, _ json.RawMessage) error {
		for _, name := range []string{"", "$now", "has space", strings.Repeat("s", 65), "a:b"} {
			err := c.Step(name, nil, func(context.Context, int) (any, error) { t.Errorf("step %q ran", name); return nil, nil })
			if !errors.Is(err, ErrInvalidName) {
				t.Errorf("step %q: %v, want ErrInvalidName", name, err)
			}
		}
		return nil
	}}})
	id := mustStart(t, n.Engine, "names", nil)
	n.exec(id)
	if _, err := n.Start(context.Background(), "unregistered", nil); !errors.Is(err, ErrUnknownKind) {
		t.Errorf("Start of an unknown kind: %v", err)
	}
}

func TestOptionsAreValidated(t *testing.T) {
	db := openAt(t, dbPath(t))
	for name, opt := range map[string]Option{
		"zero lease":            WithLease(0),
		"grace below renewal":   WithGrace(time.Second),
		"no workers":            WithWorkers(0),
		"non-positive tick":     WithTick(0),
		"non-positive max step": WithMaxSteps(0),
	} {
		if _, err := New(db, nil, nil, opt); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if _, err := New(db, nil, nil, WithLease(3*time.Second), WithGrace(time.Second)); err != nil {
		t.Errorf("grace equal to one renewal interval was refused: %v", err)
	}
}

func TestNew_MigratesAndIsIdempotent(t *testing.T) {
	path := dbPath(t)
	for i := 0; i < 3; i++ {
		n := newNode(t, path, t0, nil)
		var v int
		if err := n.db.QueryRow(`SELECT MAX(version) FROM schema_migrations WHERE component = ?`, Component).Scan(&v); err != nil || v != 1 {
			t.Fatalf("round %d: journal schema version = %d (%v), want 1", i, v, err)
		}
	}
	db := openAt(t, path)
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name IN ('workflows','workflow_steps')`).Scan(&n); err != nil || n != 2 {
		t.Errorf("tables = %d (%v), want 2", n, err)
	}
}

func TestRetention_PrunesOnlyFinishedWorkflows(t *testing.T) {
	def := Definition{Kind: "ret", Run: func(c *Ctx, _ json.RawMessage) error {
		return c.Step("s", nil, func(context.Context, int) (any, error) { return 1, nil })
	}}
	n := newNode(t, dbPath(t), t0, []Definition{def}, WithRetention(24*time.Hour))
	ids := map[Status]string{}
	for _, st := range []Status{StatusSucceeded, StatusFailed, StatusCancelled, StatusPending, StatusRunning, StatusWaiting} {
		id := mustStart(t, n.Engine, "ret", nil)
		n.exec(id)
		exec(t, n.db, `UPDATE workflows SET status=?, finished_at_ms=?, updated_at_ms=1, wake_at_ms=1 WHERE id=?`,
			string(st), func() any {
				if st.Terminal() {
					return ms(t0)
				}
				return nil
			}(), id)
		ids[st] = id
	}
	recent := mustStart(t, n.Engine, "ret", nil)
	n.exec(recent)
	exec(t, n.db, `UPDATE workflows SET finished_at_ms=? WHERE id=?`, ms(t0.Add(20*time.Hour)), recent)

	n.housekeeping(context.Background(), t0.Add(12*time.Hour)) // nothing is old enough
	count := func() int {
		var c int
		if err := n.db.QueryRow(`SELECT COUNT(*) FROM workflows`).Scan(&c); err != nil {
			t.Fatal(err)
		}
		return c
	}
	if got := count(); got != 7 {
		t.Fatalf("%d workflows after an early housekeeping, want 7", got)
	}
	n.housekeeping(context.Background(), t0.Add(25*time.Hour))
	if got := count(); got != 4 {
		t.Fatalf("%d workflows left, want the 3 unfinished ones and the recent finished one (4)", got)
	}
	for st, id := range ids {
		_, err := n.Get(context.Background(), id)
		if st.Terminal() && !errors.Is(err, ErrNotFound) {
			t.Errorf("%s workflow survived pruning (%v)", st, err)
		}
		if !st.Terminal() && err != nil {
			t.Errorf("%s workflow was deleted: %v", st, err)
		}
	}
	var steps int
	if err := n.db.QueryRow(`SELECT COUNT(*) FROM workflow_steps WHERE workflow_id IN (?,?,?)`,
		ids[StatusSucceeded], ids[StatusFailed], ids[StatusCancelled]).Scan(&steps); err != nil || steps != 0 {
		t.Errorf("%d orphan step rows (%v)", steps, err)
	}
}
