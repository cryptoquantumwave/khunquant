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

	// The outside-read ledger, kept apart from floor on purpose. The floor is
	// lowered for a whole batch before any call of it runs, so it cannot say
	// whether outside text was read by an EARLIER call. The ledger can: see
	// BeginBatch.
	seenOutside bool // some call of this turn has read, or is reading, outside text
	baseSeen    bool // seenOutside as it stood before the current batch began
	egressUsed  bool // the current batch has already spent its one egress call
	pending     int  // calls of the current batch that have not yet reached the registry
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

// BeginBatch opens a batch of tool calls (the calls of one model response) and
// returns nothing. outside says whether the batch holds a call that reads outside
// text. The turn's state before the batch is kept as the batch's base, then the
// ledger is updated, so every call of the batch sees the same base whatever order
// the calls run in. size is the number of calls that will report in with CallDone.
func (t *Taint) BeginBatch(size int, outside bool) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.baseSeen = t.seenOutside
	t.egressUsed = false
	t.pending = size
	if outside {
		t.seenOutside = true
	}
	t.mu.Unlock()
}

// BatchOpen reports whether a batch is open, that is, some call of it has not yet
// reached CallDone.
func (t *Taint) BatchOpen() bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.pending > 0
}

// CallDone records that one call of the open batch has reached the registry.
func (t *Taint) CallDone() {
	if t == nil {
		return
	}
	t.mu.Lock()
	if t.pending > 0 {
		t.pending--
	}
	t.mu.Unlock()
}

// ClaimEgress asks for the right to send data out in the open batch. It is granted
// when no call BEFORE the batch read outside text and no other call of the batch has
// claimed it: the first egress of a clean turn, at most one per batch. A nil taint
// grants nothing.
func (t *Taint) ClaimEgress() bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.baseSeen || t.egressUsed {
		return false
	}
	t.egressUsed = true
	return true
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
