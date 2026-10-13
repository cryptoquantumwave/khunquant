package journal

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// The acceptance test: a 5-step workflow, the process killed at every crash point
// of every step, completes with each side effect counted exactly once.
//
//	before_started  nothing of the step is journaled
//	after_started   the "started" row is committed, the function has not run
//	after_fn        the function ran (the order is on the exchange), no output row
//	after_output    the output row is committed
func TestCrashInjection_HardKillAtEveryPointOfEveryStep(t *testing.T) {
	points := []string{pointBeforeStarted, pointAfterStarted, pointAfterFn, pointAfterOutput}
	for k := 1; k <= 5; k++ {
		for _, point := range points {
			k, point := k, point
			t.Run(fmt.Sprintf("step%d/%s", k, point), func(t *testing.T) {
				l, x := newLedger(), newExchange()
				defs := []Definition{fiveSteps(l, x)}
				path := dbPath(t)
				victim := fmt.Sprintf("s%d", k)

				a := newNode(t, path, t0, defs)
				id := mustStart(t, a.Engine, "five", nil)
				a.armKill(func(p, step string, _ int) bool { return p == point && step == victim })
				a.exec(id)
				if !a.kill.dead.Load() {
					t.Fatal("the crash point was never reached")
				}
				w := status(t, a, id)
				if w.Status != StatusRunning {
					t.Fatalf("a dead process left the workflow %s, want running (claimed)", w.Status)
				}

				// A new process cannot take over before the lease and the grace period are over.
				b := newNode(t, path, t0, defs)
				b.exec(id)
				if status(t, b, id).Status != StatusRunning || len(l.seen(victim)) > 1 {
					t.Fatal("a second process ran a workflow whose lease was still alive")
				}
				b.clock.Advance(testLease + testGrace - time.Millisecond)
				if b.claimable(t, id) {
					t.Fatal("claimable 1ms before lease end + grace")
				}
				b.clock.Advance(time.Millisecond)
				if !b.claimable(t, id) {
					t.Fatal("not claimable at lease end + grace")
				}
				b.exec(id)

				if w := status(t, b, id); w.Status != StatusSucceeded {
					t.Fatalf("after the restart: %+v", w)
				}
				calls, orders := x.snapshot()
				if calls != 5 || len(orders) != 5 {
					t.Errorf("the exchange saw %d placements for %d orders, want 5 and 5", calls, len(orders))
				}
				for key, n := range orders {
					if n != 1 {
						t.Errorf("order %s was placed %d times", key, n)
					}
				}
				for i := 1; i <= 5; i++ {
					name := fmt.Sprintf("s%d", i)
					if got := l.effect(name); got != 1 {
						t.Errorf("step %s: the real effect happened %d times, want once", name, got)
					}
				}
				seen := l.seen(victim)
				switch point {
				case pointBeforeStarted:
					if len(seen) != 1 || seen[0] != 1 {
						t.Errorf("attempts seen by the victim = %v, want [1]", seen)
					}
				case pointAfterStarted:
					if len(seen) != 1 || seen[0] != 2 {
						t.Errorf("attempts seen by the victim = %v, want [2]", seen)
					}
				case pointAfterFn:
					if len(seen) != 2 || seen[0] != 1 || seen[1] != 2 {
						t.Errorf("attempts seen by the victim = %v, want [1 2]", seen)
					}
				case pointAfterOutput:
					if len(seen) != 1 || seen[0] != 1 {
						t.Errorf("attempts seen by the victim = %v, want [1] (replayed from its output)", seen)
					}
				}
				// Steps that finished before the crash are never seen again.
				for i := 1; i < k; i++ {
					if got := l.seen(fmt.Sprintf("s%d", i)); len(got) != 1 {
						t.Errorf("completed step s%d ran %d times", i, len(got))
					}
				}
			})
		}
	}
}

// The graceful variant: the engine's context is cancelled in the middle of every
// step. The running step finishes, the workflow is released at the boundary, and a
// new engine continues at once, without waiting for any lease: nothing runs twice
// and no step is ever re-run.
func TestCrashInjection_GracefulStopAtEveryStep(t *testing.T) {
	for k := 1; k <= 5; k++ {
		k := k
		t.Run(fmt.Sprintf("step%d", k), func(t *testing.T) {
			l, x := newLedger(), newExchange()
			defs := []Definition{fiveSteps(l, x)}
			path := dbPath(t)
			victim := fmt.Sprintf("s%d", k)

			a := newNode(t, path, t0, defs)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			a.hook = func(point, _, step string, _ int) {
				if point == pointAfterStarted && step == victim {
					cancel() // the stop arrives while the step is about to run
				}
			}
			id := mustStart(t, a.Engine, "five", nil)
			done := make(chan struct{})
			go func() { defer close(done); _ = a.Run(ctx) }()
			waitClosed(t, done, "Run to return after cancel")

			w := status(t, a, id)
			if k < 5 {
				if w.Status != StatusPending {
					t.Fatalf("after a clean stop: %+v, want the workflow released as pending", w)
				}
			}
			b := newNode(t, path, t0, defs) // same instant: no lease to wait for
			b.exec(id)
			if w := status(t, b, id); w.Status != StatusSucceeded {
				t.Fatalf("after the restart: %+v", w)
			}
			calls, orders := x.snapshot()
			if calls != 5 || len(orders) != 5 {
				t.Errorf("placements = %d for %d orders, want 5 and 5", calls, len(orders))
			}
			for i := 1; i <= 5; i++ {
				name := fmt.Sprintf("s%d", i)
				if got := l.seen(name); len(got) != 1 || got[0] != 1 {
					t.Errorf("step %s ran with attempts %v, want exactly [1]", name, got)
				}
			}
		})
	}
}

// Replaying the same journal must also work when the process dies again during the
// replay: three crashes in a row, each at a different step, still one order each.
func TestCrashInjection_RepeatedCrashes(t *testing.T) {
	l, x := newLedger(), newExchange()
	defs := []Definition{fiveSteps(l, x)}
	path := dbPath(t)
	now := t0
	id := ""
	for round, victim := range []string{"s2", "s3", "s3", "s5"} {
		n := newNode(t, path, now, defs)
		if id == "" {
			id = mustStart(t, n.Engine, "five", nil)
		}
		n.armKill(func(p, step string, _ int) bool { return p == pointAfterFn && step == victim })
		n.exec(id)
		if !n.kill.dead.Load() {
			t.Fatalf("round %d: crash point not reached", round)
		}
		now = now.Add(time.Hour)
	}
	final := newNode(t, path, now, defs)
	final.exec(id)
	if w := status(t, final, id); w.Status != StatusSucceeded {
		t.Fatalf("workflow = %+v", w)
	}
	calls, orders := x.snapshot()
	if calls != 5 || len(orders) != 5 {
		t.Errorf("placements = %d for %d orders, want 5 and 5", calls, len(orders))
	}
	if got := l.seen("s3"); len(got) != 3 || got[2] != 3 {
		t.Errorf("s3 attempts = %v, want [1 2 3]", got)
	}
}
