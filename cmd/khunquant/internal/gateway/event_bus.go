package gateway

import (
	"github.com/cryptoquantumwave/khunquant/pkg/events"
)

// eventBusResource is the name the gateway's event bus is registered under.
const eventBusResource = "event-bus"

// The gateway owns ONE events.Bus. It is made with the shared resources, so a
// config reload keeps the very same bus (and its counters and subscribers), and
// it is closed once, by sharedResources.Close at shutdown.
//
// The bus exists whether or not proactive mode is on, because sub-turn events
// use it. An idle bus (no subscriber) starts no goroutine and holds a couple of
// hundred bytes; a publish to it costs a read lock, a clock read and an ID, and
// is counted in the stats. Each subscriber later adds a worker goroutine and a
// bounded queue (16 events by default).
//
// Publishing is for observability only. Nothing may depend on a subscriber
// having seen an event (pkg/events and AgentLoop.SetEventBus both use the
// non-blocking publish).

// eventBus returns the shared bus, or nil when there are no shared resources.
func (r *sharedResources) eventBus() *events.EventBus {
	if r == nil {
		return nil
	}
	return r.Events
}

// wireEventBus hands the shared bus to the agent loop. It is idempotent, so it
// runs on every build and reload; the loop is the same one across reloads and
// simply keeps publishing to the same bus.
func wireEventBus(d assemblyDeps) {
	al := d.AgentLoop
	if al == nil || d.Shared.eventBus() == nil {
		return
	}
	al.SetEventBus(d.Shared.eventBus())
}
