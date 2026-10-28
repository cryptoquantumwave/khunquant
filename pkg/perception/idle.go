package perception

import "sync/atomic"

// Idler is optionally implemented by a source that can hold work in flight (a poll, a
// collection, a run). The Manager's Idle is true only when every Idler is. It is read
// by the sandbox lab's settle and by nothing in production.
type Idler interface{ Idle() bool }

// InFlight counts the passes a source has begun and not finished, for its Idle. It is
// pure bookkeeping, and its zero value is ready.
type InFlight struct{ n atomic.Int32 }

// Enter marks a pass as begun; call the returned func exactly once when it ends
// (defer it).
func (f *InFlight) Enter() (done func()) {
	f.n.Add(1)
	return func() { f.n.Add(-1) }
}

// Idle is true when no pass is in flight.
func (f *InFlight) Idle() bool { return f.n.Load() == 0 }
