package labjournal

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/replay"
)

// observerOpts is the observer's subscription, pinned by a test. Locked keeps publish
// order; DropNewest loses an event on a full queue rather than make a publisher wait;
// Timeout 0 starts no per-call timeout goroutine; a panicking handler is recovered.
// The handler only marshals and calls Append, which never blocks.
var observerOpts = events.SubscribeOptions{
	Name:         "labjournal",
	Buffer:       8192,
	Concurrency:  events.Locked,
	Backpressure: events.DropNewest,
	Timeout:      0,
	PanicPolicy:  events.RecoverAndLog,
}

// maxPayload bounds one journaled event; a larger one keeps only its envelope.
const maxPayload = 256 << 10

// Observer journals every bus event.
type Observer struct{ events.Subscription }

// StartObserver subscribes to every event on bus and journals it. Events the
// subscription's queue had no room for are counted in j.Drops(). Close stops it.
func StartObserver(ctx context.Context, bus *events.EventBus, j *Journal) (*Observer, error) {
	return startObserver(ctx, bus, j, func(_ context.Context, ev events.Event) error {
		j.Append(entryFor(ev))
		return nil
	})
}

// Idle is true when the subscription has handled, failed, panicked on or dropped every
// event it received (see events.SubscriberStats: a timeout is counted with Failed). It
// changes nothing; only the lab's settle calls it.
func (o *Observer) Idle() bool {
	st := o.Stats()
	return st.Received <= st.Handled+st.Failed+st.Panicked+st.Dropped
}

func startObserver(ctx context.Context, bus *events.EventBus, j *Journal, h events.Handler) (*Observer, error) {
	sub, err := bus.Channel().Subscribe(ctx, observerOpts, h)
	if err != nil {
		return nil, fmt.Errorf("labjournal: subscribe: %w", err)
	}
	j.AddDropSource(func() uint64 { return sub.Stats().Dropped })
	return &Observer{sub}, nil
}

// entryFor maps a bus event to a journal entry. Workflow, approval and policy events
// get their own kinds and are not also journaled as event.<kind>. replay.Recordable
// runs here, on the live event: it recognises a private payload by its Go type, which
// a JSON round trip would erase.
func entryFor(ev events.Event) Entry {
	e := Entry{WallTime: ev.Time, Kind: "event." + string(ev.Kind), Source: ev.Source.Component}
	switch ev.Kind {
	case events.KindJournalStep, events.KindJournalWorkflow:
		e.Kind, e.Subject = "workflow.step", payloadString(ev, "kind")
	case events.KindApprovalCreated, events.KindApprovalDecided, events.KindApprovalExpired:
		e.Kind, e.Subject = "proposal", payloadString(ev, "code")
	case events.KindPolicyDecision:
		e.Kind, e.Subject = "policy.decision", cmp.Or(attrString(ev, "tool"), ev.Source.Name)
	}
	e.Subject = cmp.Or(e.Subject, attrString(ev, "subject"), ev.Scope.ChatID, ev.Source.Component)

	stored, ok := replay.Recordable(ev)
	if !ok {
		stored = ev
	}
	b, err := json.Marshal(struct {
		Recordable bool         `json:"recordable"`
		Event      events.Event `json:"event"`
	}{ok, stored})
	if err == nil && len(b) > maxPayload {
		err = fmt.Errorf("%d bytes", len(b))
	}
	if err != nil { // keep the envelope only
		env := events.Event{ID: ev.ID, Kind: ev.Kind, Time: ev.Time, Source: ev.Source, Scope: ev.Scope}
		b, _ = json.Marshal(map[string]any{"recordable": false, "payload_dropped": err.Error(), "event": env})
	}
	e.Payload = b
	return e
}

func attrString(ev events.Event, key string) string {
	s, _ := ev.Attrs[key].(string)
	return s
}

// payloadString reads a string field of the payload through its JSON form (payload
// types differ: a struct, a map).
func payloadString(ev events.Event, key string) string {
	b, _ := json.Marshal(ev.Payload)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	s, _ := m[key].(string)
	return s
}
