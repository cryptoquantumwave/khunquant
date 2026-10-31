package sources

import (
	"context"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/perception"
)

// blockingDCAReader holds Plans until released.
type blockingDCAReader struct {
	*dcaReader
	entered chan struct{}
	gate    chan struct{}
}

func (b *blockingDCAReader) Plans(ctx context.Context) ([]perception.DCAPlan, error) {
	close(b.entered)
	<-b.gate
	return b.dcaReader.Plans(ctx)
}

// The source is busy for the whole of a step, from before the plan read (here held in the
// reader) through the promotion hook (held in OnPlans), and idle between steps.
func TestDCAPlansSource_IdleCoversAStep(t *testing.T) {
	created := utc(2026, time.June, 10, 0, 0)
	inner := &dcaReader{}
	inner.set(dcaPlan(1, "0 9 * * *", "Asia/Bangkok", created))
	rd := &blockingDCAReader{dcaReader: inner, entered: make(chan struct{}), gate: make(chan struct{})}
	hookIn, hookGate := make(chan struct{}), make(chan struct{})
	s := newDCA(t, rd, func(o *DCAPlansOptions) {
		o.OnPlans = func(context.Context, []perception.DCAPlan) { close(hookIn); <-hookGate }
	})
	if !s.Idle() {
		t.Fatal("a source with no step running is idle")
	}

	done := make(chan struct{})
	go func() { defer close(done); s.step(context.Background(), &dcaRec{}, created.Add(time.Second)) }()
	<-rd.entered // blocked in the plan read
	stats := s.Stats()
	if s.Idle() || s.Idle() {
		t.Fatal("idle while a step is blocked in the plan read")
	}
	if s.Stats() != stats {
		t.Fatal("Idle changed the source")
	}
	close(rd.gate)
	<-hookIn // read done, blocked in the promotion hook
	if s.Idle() {
		t.Fatal("idle while a step is blocked in the promotion hook")
	}
	close(hookGate)
	<-done
	if !s.Idle() {
		t.Fatal("busy after the step returned")
	}
}
