// Package attentiontest holds a scriptable stand-in for the attention governor,
// for packages that consume attention.Decider and attention.Muter.
package attentiontest

import (
	"context"
	"sync"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/attention"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// MuteCall records one Mute or Unmute call.
type MuteCall struct {
	Prefix string
	Until  time.Time // zero for Unmute
	Reason string
	Unmute bool
}

// Delivered records one MarkDelivered call.
type Delivered struct {
	Decision attention.Decision
	Delivery attention.Delivery
}

// Fake is a scriptable attention.Decider and attention.Muter. The zero value is
// ready: with nothing scripted, Decide answers notify/"template".
//
// Scripting, in order of precedence: DecideFn, then DecideErr, then Script (one
// Decision per call, in order, the last one repeating), then the default. The
// returned decision always carries the candidate that was passed in, and gets a
// fresh ID when the script leaves it empty. MarkErr and MuteErr are returned
// from MarkDelivered and from Mute/Unmute. Every call is recorded, and all
// methods are safe for concurrent use.
type Fake struct {
	DecideFn func(ctx context.Context, c proactive.Candidate) (attention.Decision, error)
	Script   []attention.Decision
	// DecideErr, MarkErr and MuteErr are returned after the call is recorded.
	DecideErr, MarkErr, MuteErr error

	mu         sync.Mutex
	candidates []proactive.Candidate
	delivered  []Delivered
	mutes      []MuteCall
	next       int
}

var (
	_ attention.Decider = (*Fake)(nil)
	_ attention.Muter   = (*Fake)(nil)
)

// Decide records c and returns the scripted decision.
func (f *Fake) Decide(ctx context.Context, c proactive.Candidate) (attention.Decision, error) {
	f.mu.Lock()
	f.candidates = append(f.candidates, c)
	fn, err := f.DecideFn, f.DecideErr
	d := attention.Decision{Disposition: proactive.DispNotify, Reason: attention.ReasonTemplate}
	if len(f.Script) > 0 {
		d = f.Script[min(f.next, len(f.Script)-1)]
		f.next++
	}
	f.mu.Unlock()

	if fn != nil {
		return fn(ctx, c)
	}
	if err != nil {
		return attention.Decision{}, err
	}
	if d.ID == "" {
		d.ID = proactive.NewID()
	}
	d.Candidate = c
	return d, nil
}

// MarkDelivered records the call.
func (f *Fake) MarkDelivered(_ context.Context, d attention.Decision, r attention.Delivery) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delivered = append(f.delivered, Delivered{Decision: d, Delivery: r})
	return f.MarkErr
}

// Mute records the call.
func (f *Fake) Mute(_ context.Context, keyPrefix string, until time.Time, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mutes = append(f.mutes, MuteCall{Prefix: keyPrefix, Until: until, Reason: reason})
	return f.MuteErr
}

// Unmute records the call.
func (f *Fake) Unmute(_ context.Context, keyPrefix string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mutes = append(f.mutes, MuteCall{Prefix: keyPrefix, Unmute: true})
	return f.MuteErr
}

// Candidates returns the candidates passed to Decide, in order.
func (f *Fake) Candidates() []proactive.Candidate {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]proactive.Candidate(nil), f.candidates...)
}

// Deliveries returns the MarkDelivered calls, in order.
func (f *Fake) Deliveries() []Delivered {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Delivered(nil), f.delivered...)
}

// Mutes returns the Mute and Unmute calls, in order.
func (f *Fake) Mutes() []MuteCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]MuteCall(nil), f.mutes...)
}
