package engine

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
)

const (
	// DefaultInboxSize is the number of events the blocking part of the inbox
	// holds before Emit makes its source wait.
	DefaultInboxSize = 1024

	// DefaultMaxSubjects bounds the coalescing part: at most this many distinct
	// (kind, subject) keys wait at once. A source that invents subjects without
	// end fills it and then loses new keys (counted), rather than memory.
	DefaultMaxSubjects = 4096
)

type latestKey struct {
	kind    events.Kind
	subject string
}

// inbox is the engine's input: a bounded channel for events that must not be
// lost to a faster successor, and a map for the ones that may be (ticks), which
// keeps only the latest per key.
//
// The consumer is the engine's one goroutine and calls pop alone; any number of
// producers call the emit methods. The total held is at most
// cap(blocking) + maxKeys, whatever the producers do.
type inbox struct {
	blocking chan events.Event
	maxKeys  int
	wake     chan struct{}
	done     chan struct{} // closed when the engine stops accepting
	doneOnce sync.Once

	mu     sync.Mutex
	latest map[latestKey]events.Event
	order  []latestKey // keys of latest in arrival order, each once

	preferLatest bool // consumer only: alternates when both parts have events

	highWater atomic.Int64
	c         *counters
}

func newInbox(size, maxKeys int, c *counters) *inbox {
	return &inbox{
		blocking: make(chan events.Event, size),
		maxKeys:  maxKeys,
		wake:     make(chan struct{}, 1),
		done:     make(chan struct{}),
		latest:   map[latestKey]events.Event{},
		c:        c,
	}
}

func (in *inbox) signal() {
	select {
	case in.wake <- struct{}{}:
	default:
	}
}

// close stops the inbox accepting events and releases blocked emitters. Events
// already in it stay for the consumer to drain.
func (in *inbox) close() { in.doneOnce.Do(func() { close(in.done) }) }

func (in *inbox) closed() bool {
	select {
	case <-in.done:
		return true
	default:
		return false
	}
}

// depth is the number of events waiting.
func (in *inbox) depth() int {
	in.mu.Lock()
	n := len(in.order)
	in.mu.Unlock()
	return len(in.blocking) + n
}

func (in *inbox) noteDepth() {
	d := int64(in.depth())
	for {
		hw := in.highWater.Load()
		if d <= hw || in.highWater.CompareAndSwap(hw, d) {
			return
		}
	}
}

// emit blocks until the event is taken, ctx is done or the inbox is closed.
func (in *inbox) emit(ctx context.Context, ev events.Event) bool {
	if in.closed() {
		in.c.droppedClosed.Add(1)
		return false
	}
	select {
	case in.blocking <- ev:
		in.noteDepth()
		in.signal()
		return true
	case <-ctx.Done():
		in.c.droppedCanceled.Add(1)
		return false
	case <-in.done:
		in.c.droppedClosed.Add(1)
		return false
	}
}

// emitLatest keeps ev as the one waiting event for its (kind, subject).
func (in *inbox) emitLatest(ev events.Event) {
	if in.closed() {
		in.c.droppedClosed.Add(1)
		return
	}
	k := latestKey{kind: ev.Kind, subject: perception.SubjectOf(ev)}
	in.mu.Lock()
	if _, waiting := in.latest[k]; waiting {
		in.latest[k] = ev
		in.mu.Unlock()
		in.c.coalesced.Add(1)
		return
	}
	if len(in.latest) >= in.maxKeys {
		in.mu.Unlock()
		in.c.droppedLatestFull.Add(1)
		return
	}
	in.latest[k] = ev
	in.order = append(in.order, k)
	in.mu.Unlock()
	in.noteDepth()
	in.signal()
}

// emitNonBlocking takes ev if there is room right now.
func (in *inbox) emitNonBlocking(ev events.Event) bool {
	if in.closed() {
		in.c.droppedClosed.Add(1)
		return false
	}
	select {
	case in.blocking <- ev:
		in.noteDepth()
		in.signal()
		return true
	default:
		in.c.droppedNonBlocking.Add(1)
		return false
	}
}

// pop returns the next event, alternating between the blocking part and the
// coalesced part while both have some, so that a flood of one cannot starve the
// other. Only the consumer calls it.
func (in *inbox) pop() (events.Event, bool) {
	for range 2 {
		if in.preferLatest {
			if ev, ok := in.popLatest(); ok {
				in.preferLatest = false
				return ev, true
			}
		} else {
			select {
			case ev := <-in.blocking:
				in.preferLatest = true
				return ev, true
			default:
			}
		}
		in.preferLatest = !in.preferLatest
	}
	return events.Event{}, false
}

func (in *inbox) popLatest() (events.Event, bool) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if len(in.order) == 0 {
		return events.Event{}, false
	}
	k := in.order[0]
	in.order[0] = latestKey{}
	in.order = in.order[1:]
	if len(in.order) == 0 {
		in.order = nil
	}
	ev := in.latest[k]
	delete(in.latest, k)
	return ev, true
}

// emitter is the perception.Emitter (and ContextEmitter) the engine hands out.
type emitter struct{ in *inbox }

var (
	_ perception.Emitter        = emitter{}
	_ perception.ContextEmitter = emitter{}
)

func (e emitter) Emit(ev events.Event) { e.in.emit(context.Background(), ev) }

func (e emitter) EmitContext(ctx context.Context, ev events.Event) bool { return e.in.emit(ctx, ev) }

func (e emitter) EmitLatest(ev events.Event) { e.in.emitLatest(ev) }

func (e emitter) EmitNonBlocking(ev events.Event) bool { return e.in.emitNonBlocking(ev) }
