package engine

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/cryptoquantumwave/khunquant/pkg/attention"
)

// Handover is the part of *attention.Outbox the default dispatcher uses: the
// entry point that takes a decision that has already been made and does no I/O.
type Handover interface {
	Accept(d attention.Decision, it attention.Item) attention.Handoff
}

// DispatchStats counts what the default dispatcher handed over.
type DispatchStats struct {
	Buffered uint64 // waiting for their group window
	Queued   uint64 // protective: queued for immediate delivery
	Ignored  uint64 // not a message (drop, log, digest)
	Rejected uint64 // the outbox had stopped
}

// DefaultDispatcher hands every decision to the attention outbox and returns. It
// never delivers, never composes and never waits: delivery is the outbox's job,
// on its own workers, with a lane for protective alerts that a slow ordinary
// message cannot hold up, and with a full ordinary lane deferring to the digest
// (counted in the outbox's stats). There is no second delivery path.
type DefaultDispatcher struct {
	out Handover

	buffered, queued, ignored, rejected atomic.Uint64
}

// NewDefaultDispatcher returns a dispatcher over out.
func NewDefaultDispatcher(out Handover) *DefaultDispatcher { return &DefaultDispatcher{out: out} }

// Dispatch hands ds to the outbox. It returns an error only to say that the outbox
// had stopped and did not take some of them (their reservations lapse).
func (d *DefaultDispatcher) Dispatch(_ context.Context, ds []attention.Decision) error {
	rejected := 0
	for _, dec := range ds {
		switch d.out.Accept(dec, attention.Item{Candidate: dec.Candidate}) {
		case attention.HandoffBuffered:
			d.buffered.Add(1)
		case attention.HandoffQueued:
			d.queued.Add(1)
		case attention.HandoffIgnored:
			d.ignored.Add(1)
		case attention.HandoffClosed:
			d.rejected.Add(1)
			rejected++
		}
	}
	if rejected > 0 {
		return fmt.Errorf("dispatch: the outbox has stopped; %d decisions not handed over", rejected)
	}
	return nil
}

// Stats returns the counters.
func (d *DefaultDispatcher) Stats() DispatchStats {
	return DispatchStats{
		Buffered: d.buffered.Load(), Queued: d.queued.Load(),
		Ignored: d.ignored.Load(), Rejected: d.rejected.Load(),
	}
}
