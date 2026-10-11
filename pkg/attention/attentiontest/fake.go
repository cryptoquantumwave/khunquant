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
	// Digest is what PendingDigest hands out next (see SetDigest); DigestErr is
	// returned from it instead when set. MarkErr is also returned from
	// CompleteDigest.
	Digest    *attention.Digest
	DigestErr error

	mu          sync.Mutex
	candidates  []proactive.Candidate
	delivered   []Delivered
	completions []Completed
	mutes       []MuteCall
	next        int
}

// Completed records one CompleteDigest call.
type Completed struct {
	Digest   *attention.Digest
	Delivery attention.Delivery
}

var (
	_ attention.Decider = (*Fake)(nil)
	_ attention.Muter   = (*Fake)(nil)
	_ attention.Gate    = (*Fake)(nil)
)

// Deliverer is a recording attention.Deliverer. The zero value delivers every
// message. Set Fn to script the outcome (it runs without any lock held, so it
// may call back into the outbox), or Result and Err for a fixed one.
type Deliverer struct {
	Fn     func(ctx context.Context, m attention.Message) (bool, error)
	Result *bool // nil = delivered
	Err    error

	mu   sync.Mutex
	msgs []attention.Message
}

var _ attention.Deliverer = (*Deliverer)(nil)

// Deliver records m and returns the scripted outcome.
func (d *Deliverer) Deliver(ctx context.Context, m attention.Message) (bool, error) {
	d.mu.Lock()
	d.msgs = append(d.msgs, m)
	d.mu.Unlock()
	if d.Fn != nil {
		return d.Fn(ctx, m)
	}
	if d.Err != nil {
		return false, d.Err
	}
	if d.Result != nil {
		return *d.Result, nil
	}
	return true, nil
}

// Messages returns the messages delivered so far, in order.
func (d *Deliverer) Messages() []attention.Message {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]attention.Message(nil), d.msgs...)
}

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

// PendingDigest returns the scripted digest: the first call after a Digest is
// set returns it (and clears it, as a real governor stops handing out a digest
// once it is checked out). DigestErr is returned instead when set.
func (f *Fake) PendingDigest(context.Context) (*attention.Digest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.DigestErr != nil {
		return nil, f.DigestErr
	}
	dg := f.Digest
	f.Digest = nil
	return dg, nil
}

// CompleteDigest records the call.
func (f *Fake) CompleteDigest(_ context.Context, dg *attention.Digest, r attention.Delivery) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.completions = append(f.completions, Completed{Digest: dg, Delivery: r})
	return f.MarkErr
}

// SetDigest scripts the digest PendingDigest hands out next.
func (f *Fake) SetDigest(dg *attention.Digest) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Digest = dg
}

// Completions returns the CompleteDigest calls, in order.
func (f *Fake) Completions() []Completed {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Completed(nil), f.completions...)
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
