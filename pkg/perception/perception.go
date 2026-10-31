// Package perception is where observations of the outside world enter the
// proactive pipeline.
//
// A Source watches one thing (a device bus, an exchange feed, the clock) and
// emits events.Event values into an Emitter. The Manager owns the sources: it
// runs each on a goroutine of its own, restarts one that fails or panics with a
// growing delay, and reports their health. The Emitter is the pipeline's inbox
// (pkg/proactive/engine implements it); a source never sees the pipeline, only
// the three ways of handing an event over:
//
//   - Emit blocks until the inbox has room. For snapshots and external triggers,
//     where every event matters and the source's own goroutine may wait.
//   - EmitLatest coalesces by (kind, subject): while an event for the same key
//     waits, the newer one replaces it. For ticks, where only the latest counts.
//   - EmitNonBlocking never blocks and reports whether the event was taken. For
//     callers that must not wait: the order feed, anything running inside a turn.
//
// A source must be cheap to stop: it ends when the context it was started with is
// done. It must not call tools, place orders or write to the exchange; it reads.
//
// Dependency rule (A.4): this package imports pkg/events, pkg/proactive and
// pkg/logger, nothing else from this module. In particular it does not import
// pkg/agent, pkg/tools or pkg/notify.
package perception

import (
	"context"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
)

// Source produces events.
//
// Start begins emitting into out and runs the source until ctx is done. The
// Manager calls it on a goroutine it owns, so a Start that blocks until ctx ends
// is fine, and so is one that starts goroutines of its own and returns nil at
// once (those must stop when ctx is done and must recover their own panics: the
// Manager can only protect what it runs). Start returning an error, or
// panicking, before ctx is done means the source failed: the Manager restarts
// it after a delay that doubles from one second to five minutes. Start
// returning nil is a source that is running, or that has finished for good; it
// is not restarted.
//
// Reconcile re-reads the authoritative state and emits it. It is used after a
// reconnect and by the heartbeat sweep, and it must not leave a goroutine
// behind. A source with no state of its own to re-read returns nil.
//
// Health may be called from any goroutine at any time.
type Source interface {
	Name() string
	Start(ctx context.Context, out Emitter) error
	Reconcile(ctx context.Context, out Emitter) error
	Health() SourceHealth
}

// Emitter is the pipeline's inbox as a source sees it. See the package comment for
// the three modes. All three are safe for concurrent use.
type Emitter interface {
	Emit(ev events.Event)
	EmitLatest(ev events.Event)
	EmitNonBlocking(ev events.Event) bool
}

// ContextEmitter is optionally implemented by an Emitter whose Emit can block: it
// lets the Manager give Emit the source's context, so that a source blocked on a
// full inbox is released when it is stopped. It reports whether the event was
// taken.
type ContextEmitter interface {
	EmitContext(ctx context.Context, ev events.Event) bool
}

// SourceHealth describes how a source is doing. All fields are optional.
type SourceHealth struct {
	// ExpectedEvery is how often the source normally emits; zero means "no
	// regular rhythm", for a source that only speaks when something happens.
	ExpectedEvery time.Duration
	// LastEventAt is when the source last emitted.
	LastEventAt time.Time
	// LastErrorAt and LastError describe the last failure. LastError is for logs
	// and tests; the stats route does not publish it.
	LastErrorAt time.Time
	LastError   string
	// ConsecutiveErrors counts failures since the last time the source ran well.
	ConsecutiveErrors int
}

// SubjectOf is the subject an event is about, for coalescing and for routing: the
// string Attrs["subject"] when the source set one, otherwise the source's name.
func SubjectOf(ev events.Event) string {
	if s, ok := ev.Attrs["subject"].(string); ok && s != "" {
		return s
	}
	return ev.Source.Name
}

// Clock is the time source of the Manager and of the engine, so that tests can
// run them without waiting.
type Clock interface {
	Now() time.Time
	// After returns a channel that receives the time once d has elapsed.
	After(d time.Duration) <-chan time.Time
}

// RealClock is the wall clock.
type RealClock struct{}

// Now returns the current time.
//
//simclock:wall the wall clock itself
func (RealClock) Now() time.Time { return time.Now() }

// After is time.After.
//
//simclock:wall the wall clock itself
func (RealClock) After(d time.Duration) <-chan time.Time { return time.After(d) }
