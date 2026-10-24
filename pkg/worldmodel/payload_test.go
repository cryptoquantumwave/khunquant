package worldmodel

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

type secretPayload struct{ Text string }

// An event of a kind the model does not track hands its own payload to the trigger
// that handles it, in a Diff that is still Empty and never serialises it.
func TestApply_UntrackedPayloadIsCarriedButNotSerialised(t *testing.T) {
	const secret = "PAYLOAD-SECRET-TEXT"
	s := newMemStore(t, newClock())
	ev := events.Event{
		ID: "x1", Kind: events.KindPerceptionExternalTrigger, Time: t0,
		Source:  events.Source{Component: "perception", Name: "external"},
		Payload: secretPayload{Text: secret}, Attrs: map[string]any{"a": secret},
		Trust: events.TrustPtr(proactive.TrustUntrusted),
	}
	d, err := s.Apply(context.Background(), ev)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := d.Payload.(secretPayload); !ok || got.Text != secret {
		t.Fatalf("Payload = %#v", d.Payload)
	}
	if !d.Empty() {
		t.Fatal("the diff of an untracked event is not empty")
	}
	if d.Event.Payload != nil || d.Event.Attrs != nil {
		t.Fatalf("the envelope kept the payload or attributes: %+v", d.Event)
	}
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), secret) || strings.Contains(strings.ToLower(string(b)), "payload") {
		t.Fatalf("the diff serialises the payload: %s", b)
	}
	if st := s.Stats(); st.Ignored != 1 {
		t.Fatalf("stats = %+v", st)
	}
}

// A tracked payload is never carried, whatever happens to it.
func TestApply_TrackedPayloadIsNeverCarried(t *testing.T) {
	s := newMemStore(t, newClock())
	d := apply(t, s, tickAt(t0, "binance", "BTC/USDT", 62000))
	if d.Payload != nil {
		t.Fatalf("a tracked payload was carried: %#v", d.Payload)
	}
	// One that cannot be decoded gives an error and still no payload.
	bad := events.Event{Kind: events.KindPerceptionMarketTick, Time: t0, Payload: make(chan int)}
	d, err := s.Apply(context.Background(), bad)
	if err == nil || d.Payload != nil {
		t.Fatalf("payload %#v, err %v", d.Payload, err)
	}
}

// Every kind Tracks reports false for carries its payload; every kind it reports
// true for does not.
func TestApply_PayloadFollowsTracks(t *testing.T) {
	kinds := []events.Kind{
		events.KindPerceptionMarketTick, events.KindPerceptionPortfolioSnapshot, events.KindPerceptionFuturesPositions,
		events.KindPerceptionPlanHealth, events.KindPerceptionSourceHealth, events.KindPerceptionOrderFilled,
		events.KindPerceptionClockDayStart, events.KindPerceptionExternalTrigger, events.KindPerceptionDevice,
		events.KindPerceptionNewsItem, events.KindPerceptionClockSession, events.KindAgentTurnStart,
	}
	for _, k := range kinds {
		s := newMemStore(t, newClock())
		d, _ := s.Apply(context.Background(), events.Event{Kind: k, Time: t0, Payload: "p"})
		if carried := d.Payload != nil; carried == Tracks(k) {
			t.Errorf("%s: payload carried = %v but Tracks = %v", k, carried, Tracks(k))
		}
	}
}
