package reflection

import (
	"context"
	"testing"
	"time"
)

// The service is busy while the daily reflection runs (here inside the model turn) and
// idle before it and after it; the wait for the next day is a timer.
func TestReflection_IdleCoversARun(t *testing.T) {
	r := newSRig(t)
	entered, release := make(chan struct{}), make(chan struct{})
	r.turn.fn = func(string) (TurnOutcome, error) {
		close(entered)
		<-release
		return TurnOutcome{Called: true}, nil
	}
	r.dismissed("a", today.Add(10*time.Hour), 3, true)
	if !r.svc.Idle() {
		t.Fatal("a service that never ran is idle")
	}

	done := make(chan Result, 1)
	go func() {
		res, _ := r.svc.RunDay(context.Background())
		done <- res
	}()
	<-entered
	if r.svc.Idle() || r.svc.Idle() {
		t.Fatal("idle while the reflection turn is running")
	}
	if r.turn.calls() != 1 {
		t.Fatalf("Idle started a turn: %d calls", r.turn.calls())
	}
	close(release)
	if res := <-done; res.Outcome != OutcomeDone {
		t.Fatalf("outcome %q", res.Outcome)
	}
	if !r.svc.Idle() {
		t.Fatal("busy after the run returned")
	}
	var nilSvc *Service
	if !nilSvc.Idle() {
		t.Fatal("a nil service is idle")
	}
}
