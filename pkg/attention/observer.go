package attention

import (
	"sync"
	"sync/atomic"
	"time"
)

// Outcome is what became of one message the outbox tried to deliver, reported to the
// Observer after the delivery and its bookkeeping are done.
type Outcome struct {
	// Message is what was handed to the deliverer: the template rendering, or the
	// composer's text when it replaced it. Its Decisions are the decisions that made
	// it up.
	Message Message
	// Delivered is whether the deliverer took it. False means that it was tried
	// again until the tries ran out (or the outbox stopped) and its decisions were
	// moved to the digest (or, for a protective Critical, released). A message is
	// reported once, with its last outcome, not once per try.
	Delivered bool
	// Composed is true when the LLM composer produced the text that was sent: a model
	// turn was spent on this message. It is false for a template message, and for one
	// whose composer failed or was never consulted (a message with a protective
	// Critical in it is never composed).
	Composed bool
	// At is when the outbox finished with the message.
	At time.Time
}

// Observer is told how each message went. It is for audit trails and counters, not
// for anything a delivery depends on: the outbox calls it on a goroutine of its own
// after the message is delivered and recorded, never waits for it, recovers its
// panics and drops the call (counting it) when too many earlier calls are still
// running. It may be called concurrently with itself.
type Observer func(Outcome)

const (
	// maxObserverInflight bounds the observer calls running at once.
	maxObserverInflight = 8
	// observerStopWait is how long shutdown waits for observer calls in flight.
	observerStopWait = 2 * time.Second
)

type observerState struct {
	mu      sync.Mutex
	f       Observer
	stopped bool
	wg      sync.WaitGroup
	sem     chan struct{}

	dropped atomic.Uint64
}

func newObserverState() *observerState {
	return &observerState{sem: make(chan struct{}, maxObserverInflight)}
}

// SetObserver installs f (nil removes it). Messages that finish while none is set are
// not reported later.
func (o *Outbox) SetObserver(f Observer) {
	s := o.obs
	s.mu.Lock()
	s.f = f
	s.mu.Unlock()
}

// observe reports out to the observer, if there is one. It never blocks and never
// fails.
func (o *Outbox) observe(out Outcome) {
	s := o.obs
	s.mu.Lock()
	f := s.f
	if f == nil || s.stopped {
		s.mu.Unlock()
		return
	}
	select {
	case s.sem <- struct{}{}:
	default:
		s.mu.Unlock()
		s.dropped.Add(1)
		return
	}
	s.wg.Add(1)
	s.mu.Unlock()

	go func() {
		defer s.wg.Done()
		defer func() { <-s.sem }()
		defer func() {
			if r := recover(); r != nil {
				o.notePanic("observer", r)
			}
		}()
		f(out)
	}()
}

// stopObservers stops reporting and waits, at most observerStopWait, for the calls in
// flight. A call that outlives the wait is left to finish on its own.
func (o *Outbox) stopObservers() {
	s := o.obs
	s.mu.Lock()
	s.stopped = true
	s.mu.Unlock()
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(observerStopWait):
	}
}
