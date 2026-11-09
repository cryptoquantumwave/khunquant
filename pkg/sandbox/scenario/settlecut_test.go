package scenario

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/supervisor"
)

// An advance call whose wall budget ends inside the settle after its last step leaves the
// clock at the target with that step's work still in flight. advanceTo (the timeline's at:,
// the advance step, every run_until chunk) must not return over it: it calls again, and
// the instance settles the step before it answers done:true.
func TestAdvanceTo_SettlesALastStepTheBudgetCut(t *testing.T) {
	sup := newFakeSup(t)
	info, err := sup.Up(context.Background(), supervisor.UpOptions{ClockStart: "2026-10-20T09:00:00+07:00", ClockMode: "manual"})
	if err != nil {
		t.Fatal(err)
	}
	in, _ := sup.get(info.Name)
	c, err := sup.Client(info.Name)
	if err != nil {
		t.Fatal(err)
	}
	target := in.lab.Clock.Now().Add(time.Minute)
	var cut, settledAfterCut atomic.Bool
	in.lab.Settle = func(ctx context.Context) error {
		if !in.lab.Clock.Now().Before(target) && cut.CompareAndSwap(false, true) {
			<-ctx.Done() // the call's wall budget ends inside the last step's settle
			return ctx.Err()
		}
		if cut.Load() {
			settledAfterCut.Store(true)
		}
		return nil
	}

	if err := advanceTo(context.Background(), c, target, 200*time.Millisecond, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if !cut.Load() {
		t.Fatal("no settle was cut: the test proves nothing")
	}
	if !settledAfterCut.Load() {
		t.Fatal("advanceTo returned over a last step whose settle the wall budget cut")
	}
}
