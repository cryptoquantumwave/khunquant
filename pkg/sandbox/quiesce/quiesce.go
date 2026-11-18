// Package quiesce answers "is the gateway done reacting?". Every component that can
// hold work in flight registers a Quiescer; Settle polls them all in wall time until
// two polls in a row find every one idle.
//
// An Idle method is true when no goroutine holds work it can do now without a timer
// firing. Work due later (a window still open, a backoff, the next poll) is a timer,
// not busy. It must be cheap, free of side effects and unchanged in production.
package quiesce

import (
	"context"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
)

// Gap bounds of the wall-time pause between polls: it starts at MinGap and doubles
// to MaxGap while something is busy, and returns to MinGap once a busy poll turns idle.
const (
	MinGap = 200 * time.Microsecond
	MaxGap = 5 * time.Millisecond
)

// Quiescer is a component that can tell whether it holds work in flight.
type Quiescer interface {
	Name() string
	Idle() bool
}

type funcQuiescer struct {
	name string
	idle func() bool
}

func (f funcQuiescer) Name() string { return f.name }
func (f funcQuiescer) Idle() bool   { return f.idle() }

// Func adapts a closure, so the gateway can register one that reads the current
// service generation each time (a reload builds new components).
func Func(name string, idle func() bool) Quiescer { return funcQuiescer{name: name, idle: idle} }

// Status is one quiescer's state at a moment.
type Status struct {
	Name string `json:"name"`
	Idle bool   `json:"idle"`
}

// BusyError is what Settle returns when the components did not go quiet in time.
type BusyError struct {
	Names []string // the components that were busy at the last poll
	Wall  time.Duration
	Cause error // the context's error when the caller's context ended before the cap
}

func (e *BusyError) Error() string {
	msg := "not settled after " + e.Wall.Round(time.Millisecond).String()
	if len(e.Names) == 0 {
		return msg + " (no component was busy at the last poll)"
	}
	return msg + ": " + strings.Join(e.Names, ", ")
}

// Unwrap lets a caller tell that its own deadline ended the wait (errors.Is).
func (e *BusyError) Unwrap() error { return e.Cause }

// Report is how a Settle went.
type Report struct {
	Polls int
	Wall  time.Duration
	Busy  []string // busy at the last poll; empty when settled
}

// Registry is the set of quiescers, by name.
type Registry struct {
	mu sync.RWMutex
	qs map[string]Quiescer
	// external, when set, says whether the gateway is waiting on something outside the lab's
	// control (a call to a real model that takes a minute). Wall time spent while it is true
	// does not count against a settle's wall cap: the work is in flight, not stuck. The
	// caller's context still ends the wait, so an advance's wall budget cuts it and the next
	// call settles again.
	external func() bool
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{qs: map[string]Quiescer{}} }

// Register adds q; a quiescer of the same name is replaced (a reload registers again).
func (r *Registry) Register(q Quiescer) {
	r.mu.Lock()
	r.qs[q.Name()] = q
	r.mu.Unlock()
}

// SetExternalWait sets the check that makes a settle's wall cap stop counting (see Registry).
func (r *Registry) SetExternalWait(f func() bool) {
	r.mu.Lock()
	r.external = f
	r.mu.Unlock()
}

// ExternalWait reports whether the gateway is waiting on something outside the lab now.
func (r *Registry) ExternalWait() bool {
	r.mu.RLock()
	f := r.external
	r.mu.RUnlock()
	return f != nil && f()
}

// list returns the quiescers sorted by name, without holding the lock while they run.
func (r *Registry) list() []Quiescer {
	r.mu.RLock()
	out := make([]Quiescer, 0, len(r.qs))
	for _, q := range r.qs {
		out = append(out, q)
	}
	r.mu.RUnlock()
	slices.SortFunc(out, func(a, b Quiescer) int { return strings.Compare(a.Name(), b.Name()) })
	return out
}

// Snapshot is every quiescer and its state now, by name, without waiting.
func (r *Registry) Snapshot() []Status {
	qs := r.list()
	out := make([]Status, len(qs))
	for i, q := range qs {
		out[i] = Status{Name: q.Name(), Idle: q.Idle()}
	}
	return out
}

// busy returns the names of the quiescers that are not idle.
func (r *Registry) busy() []string {
	var names []string
	for _, q := range r.list() {
		if !q.Idle() {
			names = append(names, q.Name())
		}
	}
	return names
}

// Settle returns nil once two polls in a row found every quiescer idle, and a
// *BusyError once wallCap of wall time has passed (or ctx ended) first. It never
// passes on a timeout.
func (r *Registry) Settle(ctx context.Context, wallCap time.Duration) error {
	_, err := r.SettleReport(ctx, wallCap)
	return err
}

// SettleReport is Settle with how it went (for the journal and the inspect op).
func (r *Registry) SettleReport(ctx context.Context, wallCap time.Duration) (Report, error) {
	//simclock:wall the settle cap is wall time (design 5.3)
	start := time.Now()
	deadline := start.Add(wallCap)
	rep := Report{}
	gap, clean := MinGap, 0
	last := start
	for {
		rep.Polls++
		//simclock:wall the settle cap is wall time
		now := time.Now()
		if r.ExternalWait() {
			deadline = deadline.Add(now.Sub(last)) // time spent waiting on a real model is not busy time
		}
		last = now
		if busy := r.busy(); len(busy) == 0 {
			clean++
			rep.Busy = nil // the last poll was clean: an error below names no stale poll
			if clean == 2 {
				//simclock:wall measured, not decided on
				rep.Wall = time.Since(start)
				return rep, nil
			}
			gap = MinGap
		} else {
			clean, rep.Busy = 0, busy
		}
		//simclock:wall the wall deadline of the settle
		if !time.Now().Before(deadline) || ctx.Err() != nil {
			//simclock:wall measured, not decided on
			rep.Wall = time.Since(start)
			return rep, &BusyError{Names: rep.Busy, Wall: rep.Wall, Cause: ctx.Err()}
		}
		// The gap lets a goroutine that a fired timer just woke mark itself busy.
		runtime.Gosched()
		//simclock:wall the gap between polls is wall time
		t := time.NewTimer(gap)
		select {
		case <-ctx.Done():
		case <-t.C:
		}
		t.Stop()
		if clean == 0 {
			gap = min(gap*2, MaxGap)
		}
	}
}
