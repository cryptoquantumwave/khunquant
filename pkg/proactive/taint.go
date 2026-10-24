package proactive

import (
	"context"
	"sync"
)

// Taint is the provenance floor of one turn: the lowest trust of everything that
// has influenced it so far. It starts at the trust of what the turn was given (its
// candidates) and only ever goes down, as the turn reads text that somebody else
// may have written. Code lowers it (the tool loop, before a batch of tool calls
// runs); nothing a model passes in an argument reaches it.
//
// It is safe for concurrent use: the calls of one model response run at the same
// time. A nil *Taint answers TrustUntrusted, so a turn that lost its holder fails
// closed.
type Taint struct {
	mu    sync.Mutex
	floor Trust
}

// NewTaint returns a taint that starts at the given trust.
func NewTaint(start Trust) *Taint { return &Taint{floor: start} }

// Trust is the turn's trust floor now.
func (t *Taint) Trust() Trust {
	if t == nil {
		return TrustUntrusted
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.floor
}

// Lower drops the floor to at most to. It never raises it.
func (t *Taint) Lower(to Trust) {
	if t == nil {
		return
	}
	t.mu.Lock()
	if to < t.floor {
		t.floor = to
	}
	t.mu.Unlock()
}

type taintKey struct{}

// WithTaint returns a context carrying t. A turn has one taint; sub-turns started
// from its context share it.
func WithTaint(ctx context.Context, t *Taint) context.Context {
	return context.WithValue(ctx, taintKey{}, t)
}

// TaintFrom returns the turn's taint, or nil when the context carries none.
func TaintFrom(ctx context.Context) *Taint {
	t, _ := ctx.Value(taintKey{}).(*Taint)
	return t
}
