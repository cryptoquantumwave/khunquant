package sources

import (
	"context"
	"testing"
)

// A run is in flight from its start, including while it waits on its asset list, and
// the next scheduled run is a timer, not busy.
func TestNews_IdleCoversARun(t *testing.T) {
	entered, gate := make(chan struct{}), make(chan struct{})
	r := newRig(t, nil, nil)
	r.src.o.Assets = func(context.Context) []NewsAsset {
		close(entered)
		<-gate
		return nil
	}
	if !r.src.Idle() {
		t.Fatal("busy before any run")
	}
	done := make(chan struct{})
	go func() { r.src.Run(context.Background(), &newsOut{}); close(done) }()
	<-entered
	runs := r.src.Stats().Runs
	if r.src.Idle() {
		t.Fatal("idle while a run is in progress")
	}
	if r.src.Stats().Runs != runs {
		t.Fatal("Idle changed the source's counters")
	}
	close(gate)
	<-done
	if !r.src.Idle() {
		t.Fatal("busy after the run returned")
	}
}
