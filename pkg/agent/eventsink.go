package agent

import (
	"context"
	"errors"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// eventSink receives the sub-turn lifecycle events (SubTurnSpawnEvent and the
// other SubTurn*Event values). Publishing is for observability only: an
// implementation must not block, and nothing the agent does may depend on an
// event having been delivered.
//
// The sink belongs to one AgentLoop, so tests that replace it never share
// state with each other or with a loop that is running in parallel. The
// default is a no-op, which is what a loop with no event bus has.
type eventSink interface {
	emit(event any)
}

// eventSinkFunc adapts a function to eventSink (tests use it to record events).
type eventSinkFunc func(event any)

func (f eventSinkFunc) emit(event any) { f(event) }

// noopSink discards every event.
type noopSink struct{}

func (noopSink) emit(any) {}

// sinkHolder lets an eventSink of any concrete type sit in one atomic.Pointer.
type sinkHolder struct{ sink eventSink }

// subTurnSink is the loop's current sink; it is never nil. Callers read it once
// per operation so that one sub-turn's events all go to the same place.
func (al *AgentLoop) subTurnSink() eventSink {
	if al == nil {
		return noopSink{}
	}
	if h := al.eventSink.Load(); h != nil && h.sink != nil {
		return h.sink
	}
	return noopSink{}
}

// setEventSink replaces the loop's sink; nil restores the no-op.
func (al *AgentLoop) setEventSink(s eventSink) {
	if s == nil {
		al.eventSink.Store(nil)
		return
	}
	al.eventSink.Store(&sinkHolder{sink: s})
}

// SetEventBus makes the loop publish its sub-turn lifecycle events
// (agent.subturn.*) on b, with the non-blocking publish: a full or slow
// subscriber costs the sub-turn nothing. nil removes the bus and the loop goes
// back to publishing nothing. It is safe to call while turns are running, and
// it may be called again with the same bus.
func (al *AgentLoop) SetEventBus(b events.Bus) {
	if eb, ok := b.(*events.EventBus); ok && eb == nil {
		b = nil // a typed nil must not become a non-nil sink
	}
	if b == nil {
		al.setEventSink(nil)
		return
	}
	al.setEventSink(busEventSink{bus: b})
}

// busEventSink publishes sub-turn events on an events.Bus.
type busEventSink struct{ bus events.Bus }

// emit converts the event and publishes it without blocking. It never panics
// and never returns an error: it is called from deferred cleanup of a sub-turn,
// and a failure to observe must not become a failure to run.
func (s busEventSink) emit(event any) {
	defer func() {
		if r := recover(); r != nil {
			logger.WarnCF("subturn", "event bus publish panicked; event dropped",
				map[string]any{"panic": r})
		}
	}()
	evt, ok := subTurnBusEvent(event)
	if !ok {
		return
	}
	s.bus.PublishNonBlocking(evt)
}

// ====================== Sub-turn events ======================
//
// These are what a sub-turn reports about itself. They hold identifiers,
// flags, counts, durations and a status, and nothing else: no prompt text, no
// tool arguments, no tool result and no error text, because a sub-turn's inputs
// and outputs can hold balances and credentials and the bus has subscribers
// that are not the agent.

// SubTurnStatus is how a sub-turn ended.
type SubTurnStatus string

const (
	// SubTurnOK means the sub-turn ran to completion.
	SubTurnOK SubTurnStatus = "ok"
	// SubTurnFailed means it ended with an error other than a cancel or timeout.
	SubTurnFailed SubTurnStatus = "error"
	// SubTurnCanceled means its context was canceled.
	SubTurnCanceled SubTurnStatus = "canceled"
	// SubTurnTimedOut means it ran past its timeout.
	SubTurnTimedOut SubTurnStatus = "timeout"
	// SubTurnPanicked means it panicked; the panic was recovered.
	SubTurnPanicked SubTurnStatus = "panic"
)

// subTurnStatus classifies how a sub-turn ended.
func subTurnStatus(err error, panicked bool) SubTurnStatus {
	switch {
	case panicked:
		return SubTurnPanicked
	case err == nil:
		return SubTurnOK
	case errors.Is(err, context.Canceled):
		return SubTurnCanceled
	case errors.Is(err, context.DeadlineExceeded):
		return SubTurnTimedOut
	default:
		return SubTurnFailed
	}
}

// SubTurnSpawnEvent reports that a sub-turn started.
type SubTurnSpawnEvent struct {
	ParentID  string
	ChildID   string
	AgentID   string
	Model     string
	Async     bool
	Critical  bool
	ToolCount int
	Timeout   time.Duration
}

// SubTurnEndEvent reports that a sub-turn ended.
type SubTurnEndEvent struct {
	ParentID    string
	ChildID     string
	Status      SubTurnStatus
	HasResult   bool
	ResultIsErr bool
	ResultBytes int
	Duration    time.Duration
}

// SubTurnResultDeliveredEvent reports that an async result reached its parent.
type SubTurnResultDeliveredEvent struct {
	ParentID string
	ChildID  string
}

// SubTurnOrphanResultEvent reports an async result that could not be delivered
// because its parent had finished or its queue was full.
type SubTurnOrphanResultEvent struct {
	ParentID string
	ChildID  string
}

// The payloads below are what goes on the bus. They are separate types from the
// events above so the wire format is stated in one place and cannot grow a
// field by accident.

type subTurnSpawnPayload struct {
	ParentID  string `json:"parent_id"`
	ChildID   string `json:"child_id"`
	AgentID   string `json:"agent_id,omitempty"`
	Model     string `json:"model"`
	Async     bool   `json:"async"`
	Critical  bool   `json:"critical"`
	ToolCount int    `json:"tool_count"`
	TimeoutMS int64  `json:"timeout_ms"`
}

type subTurnEndPayload struct {
	ParentID    string        `json:"parent_id"`
	ChildID     string        `json:"child_id"`
	Status      SubTurnStatus `json:"status"`
	HasResult   bool          `json:"has_result"`
	ResultIsErr bool          `json:"result_is_error,omitempty"`
	ResultBytes int           `json:"result_bytes,omitempty"`
	DurationMS  int64         `json:"duration_ms"`
}

type subTurnDeliveryPayload struct {
	ParentID string `json:"parent_id"`
	ChildID  string `json:"child_id"`
}

// subTurnBusEvent converts an in-process sub-turn event to its bus form. The
// second result is false for a value that is not a sub-turn event.
func subTurnBusEvent(event any) (events.Event, bool) {
	evt := events.Event{
		Source:   events.Source{Component: "agent", Name: "subturn"},
		Severity: events.SeverityInfo,
		// Computed by KhunQuant from its own state, not from outside data.
		Trust: events.TrustPtr(proactive.TrustSystem),
	}
	switch e := event.(type) {
	case SubTurnSpawnEvent:
		evt.Kind = events.KindAgentSubTurnSpawn
		evt.Scope.TurnID = e.ParentID
		evt.Correlation.ParentTurnID = e.ParentID
		evt.Payload = subTurnSpawnPayload{
			ParentID: e.ParentID, ChildID: e.ChildID, AgentID: e.AgentID, Model: e.Model,
			Async: e.Async, Critical: e.Critical, ToolCount: e.ToolCount,
			TimeoutMS: e.Timeout.Milliseconds(),
		}
	case SubTurnEndEvent:
		evt.Kind = events.KindAgentSubTurnEnd
		evt.Scope.TurnID = e.ParentID
		evt.Correlation.ParentTurnID = e.ParentID
		if e.Status != SubTurnOK {
			evt.Severity = events.SeverityWarn
		}
		evt.Payload = subTurnEndPayload{
			ParentID: e.ParentID, ChildID: e.ChildID, Status: e.Status,
			HasResult: e.HasResult, ResultIsErr: e.ResultIsErr, ResultBytes: e.ResultBytes,
			DurationMS: e.Duration.Milliseconds(),
		}
	case SubTurnResultDeliveredEvent:
		evt.Kind = events.KindAgentSubTurnResultDelivered
		evt.Scope.TurnID = e.ParentID
		evt.Correlation.ParentTurnID = e.ParentID
		evt.Payload = subTurnDeliveryPayload{ParentID: e.ParentID, ChildID: e.ChildID}
	case SubTurnOrphanResultEvent:
		evt.Kind = events.KindAgentSubTurnOrphan
		evt.Scope.TurnID = e.ParentID
		evt.Correlation.ParentTurnID = e.ParentID
		evt.Severity = events.SeverityWarn
		evt.Payload = subTurnDeliveryPayload{ParentID: e.ParentID, ChildID: e.ChildID}
	default:
		return events.Event{}, false
	}
	return evt, true
}
