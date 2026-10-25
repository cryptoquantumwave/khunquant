package tools

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral"
	"github.com/cryptoquantumwave/khunquant/pkg/journal"
)

var dnOpenSteps = []string{"validate", "set_leverage", "place_first_leg", "verify_first_leg", "place_second_leg", "verify_second_leg", "finalize"}

// TestDNOpen_CrashAfterEachStep_NoDuplicateOrders kills the process at every crash
// point of every one of the seven steps, over both classes of exchange adapter, and
// lets a second process resume. On the capable adapter the open completes with one
// order per leg. On the not-capable one a crash DURING a place step cannot be shown
// to have placed or not placed the order, so the resume places nothing and the open
// ends in recovery; everywhere else it completes. On NEITHER is a second order
// placed, and no execution is left in a non-terminal state without a workflow.
func TestDNOpen_CrashAfterEachStep_NoDuplicateOrders(t *testing.T) {
	points := []string{journal.CrashBeforeStarted, journal.CrashAfterStarted, journal.CrashAfterFn, journal.CrashAfterOutput}
	for _, capable := range []bool{true, false} {
		class := map[bool]string{true: "capable", false: "not_capable"}[capable]
		for _, step := range dnOpenSteps {
			for _, point := range points {
				capable, step, point := capable, step, point
				t.Run(fmt.Sprintf("%s/%s/%s", class, step, point), func(t *testing.T) {
					r := newDNRig(t, capable)
					a := r.node()
					id := r.start(a, deltaneutral.LegTypeFutures)
					kill := a.eng.SimulateKill(func(p, s string, _ int) bool { return p == point && s == step })
					a.eng.ExecuteNow(context.Background(), id)
					if !kill.Dead() {
						t.Fatal("the crash point was never reached")
					}

					b := r.node()
					r.clock.Advance(40 * time.Second) // lease (30 s) + grace (10 s)
					w := r.drive(b, id, true)
					if w.Status != journal.StatusSucceeded {
						t.Fatalf("after the resume the workflow is %+v", w)
					}
					// A recovery the open started runs until it waits for the owner.
					if rw, err := b.eng.Get(context.Background(), deltaneutral.RecoverID(r.exec(b)[0].ID)); err == nil && !rw.Status.Terminal() {
						r.drive(b, rw.ID, false)
					}

					if n := r.world.placed("futures"); n > 1 {
						t.Errorf("%d futures orders", n)
					}
					if n := r.world.placed("spot"); n > 1 {
						t.Errorf("%d spot orders", n)
					}
					if r.world.dupRejected != 0 {
						t.Errorf("the exchange had to refuse %d repeated client order ids", r.world.dupRejected)
					}
					ex := r.exec(b)
					if len(ex) != 1 {
						t.Fatalf("%d executions", len(ex))
					}
					r.assertNoOrphanedExecution(b)
					t.Logf("MATRIX %s %s %s -> exec=%s plan=%s futures=%d spot=%d", class, step, point, ex[0].State, r.plan(b).Status,
						r.world.placed("futures"), r.world.placed("spot"))

					placing := step == "place_first_leg" || step == "place_second_leg"
					duringPlace := placing && (point == journal.CrashAfterStarted || point == journal.CrashAfterFn)
					if capable || !duringPlace {
						if ex[0].State != string(deltaneutral.ExecutionStateBothLegsFilled) || r.world.placed("futures") != 1 || r.world.placed("spot") != 1 {
							t.Errorf("want a completed open with one order per leg: exec %q, futures %d, spot %d",
								ex[0].State, r.world.placed("futures"), r.world.placed("spot"))
						}
						return
					}
					if ex[0].State == string(deltaneutral.ExecutionStateBothLegsFilled) {
						t.Errorf("a crash during a place step on a not-capable adapter cannot be shown to have placed it: the open must not claim success")
					}
				})
			}
		}
	}
}
