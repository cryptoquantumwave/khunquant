package labjournal

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

func TestObserverOptionsPinned(t *testing.T) {
	want := events.SubscribeOptions{
		Name: "labjournal", Buffer: 8192, Concurrency: events.Locked,
		Backpressure: events.DropNewest, Timeout: 0, PanicPolicy: events.RecoverAndLog,
	}
	if observerOpts != want {
		t.Fatalf("observerOpts = %+v, want %+v", observerOpts, want)
	}
}

// stalledHandler blocks every event until release is called.
func stalledHandler() (h events.Handler, release func()) {
	gate := make(chan struct{})
	var once sync.Once
	return func(ctx context.Context, _ events.Event) error {
			select {
			case <-gate:
			case <-ctx.Done():
			}
			return nil
		}, func() {
			once.Do(func() { close(gate) })
		}
}

func TestObserverDropsInsteadOfBlocking(t *testing.T) {
	j := openTest(t, Options{})
	bus := events.NewBus()
	t.Cleanup(func() { _ = bus.Close() })
	h, release := stalledHandler()
	t.Cleanup(release)
	obs, err := startObserver(context.Background(), bus, j, h)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = obs.Close() })

	n := observerOpts.Buffer * 10
	start := time.Now()
	for i := 0; i < n; i++ {
		// The blocking Publish: with Block backpressure this would hang.
		bus.Publish(context.Background(), events.Event{Kind: events.KindPerceptionMarketTick})
	}
	// The plan's 100 ms is for an idle machine; a blocking publisher would not return at all.
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("%d publishes took %s with a stalled observer", n, d)
	}
	var dropped uint64
	for _, s := range bus.Stats().SubscriberStats {
		if s.Name == "labjournal" {
			dropped = s.Dropped
		}
	}
	if dropped < uint64(n-observerOpts.Buffer-1) || j.Drops() < dropped {
		t.Fatalf("dropped = %d (journal Drops %d), want about %d", dropped, j.Drops(), n-observerOpts.Buffer)
	}
}

func TestObserverDoesNotSlowFeedback(t *testing.T) {
	j := openTest(t, Options{})
	bus := events.NewBus()
	t.Cleanup(func() { _ = bus.Close() })
	h, release := stalledHandler()
	t.Cleanup(release)
	obs, err := startObserver(context.Background(), bus, j, h)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = obs.Close() })

	got := make(chan struct{}, 1)
	sub, err := bus.Channel().OfKind(events.KindPerceptionOrderFilled).
		Subscribe(context.Background(), events.SubscribeOptions{Name: "feedback", Buffer: 64, Timeout: time.Second},
			func(context.Context, events.Event) error { got <- struct{}{}; return nil })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sub.Close() })

	for i := 0; i < observerOpts.Buffer+10; i++ { // the observer's queue is full
		bus.PublishNonBlocking(events.Event{Kind: events.KindPerceptionMarketTick})
	}
	bus.PublishNonBlocking(events.Event{Kind: events.KindPerceptionOrderFilled})
	select {
	case <-got:
	case <-time.After(time.Second):
		t.Fatal("the feedback subscriber did not get the event while the observer was stalled")
	}
}

func journalOf(t *testing.T, publish func(*events.EventBus)) []Entry {
	t.Helper()
	j := openTest(t, Options{})
	bus := events.NewBus()
	t.Cleanup(func() { _ = bus.Close() })
	obs, err := StartObserver(context.Background(), bus, j)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = obs.Close() })
	publish(bus)
	// The handler runs on the subscription's goroutine: wait for it to see everything.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s := bus.Stats().SubscriberStats[0]
		if s.Handled+s.Dropped >= s.Received && s.Received > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	return query(t, j, Query{})
}

func TestObserverMapsKindsOnce(t *testing.T) {
	entries := journalOf(t, func(bus *events.EventBus) {
		bus.PublishNonBlocking(events.Event{Kind: events.KindJournalStep, Source: events.Source{Component: "journal"},
			Payload: map[string]any{"kind": "dca.execute", "step": "place"}})
		bus.PublishNonBlocking(events.Event{Kind: events.KindApprovalDecided,
			Payload: map[string]any{"code": "AB12"}})
		bus.PublishNonBlocking(events.Event{Kind: events.KindPolicyDecision, Attrs: map[string]any{"tool": "exchange_order"}})
		bus.PublishNonBlocking(events.Event{Kind: events.KindTriggerCandidate, Attrs: map[string]any{"subject": "price:BTC"}})
		bus.PublishNonBlocking(events.Event{Kind: events.KindAgentTurnStart, Scope: events.Scope{ChatID: "c1"}})
	})
	var got []string
	for _, e := range entries {
		got = append(got, e.Kind+"|"+e.Subject)
	}
	want := []string{
		"workflow.step|dca.execute", "proposal|AB12", "policy.decision|exchange_order",
		"event.trigger.candidate|price:BTC", "event.agent.turn.start|c1",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("entries = %v, want %v", got, want)
	}
	for _, e := range entries {
		if e.Kind == "event.journal.step" || !json.Valid(e.Payload) {
			t.Errorf("bad entry %+v", e)
		}
	}
}

func TestObserverRecordableAtCapture(t *testing.T) {
	exchange := events.TrustPtr(proactive.TrustExchange)
	entries := journalOf(t, func(bus *events.EventBus) {
		bus.PublishNonBlocking(events.Event{Kind: events.KindPerceptionExternalTrigger, Trust: exchange,
			Payload: perception.ExternalTrigger{Name: "n", Message: "secret text"}})
		bus.PublishNonBlocking(events.Event{Kind: events.KindPerceptionMarketTick, Trust: exchange,
			Payload: map[string]any{"symbol": "BTC/USDT"}})
	})
	if len(entries) != 2 {
		t.Fatalf("entries = %d", len(entries))
	}
	flag := func(e Entry) bool {
		var p struct {
			Recordable bool `json:"recordable"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatal(err)
		}
		return p.Recordable
	}
	if flag(entries[0]) || !flag(entries[1]) {
		t.Fatalf("recordable flags = %v, %v; want false (private payload), true (tick)", flag(entries[0]), flag(entries[1]))
	}
}

func TestObserverOversizedAndUnmarshalableEventsStayValidJSON(t *testing.T) {
	entries := journalOf(t, func(bus *events.EventBus) {
		bus.PublishNonBlocking(events.Event{Kind: events.KindAgentError, Payload: strings.Repeat("x", maxPayload+1)})
		bus.PublishNonBlocking(events.Event{Kind: events.KindAgentError, Payload: make(chan int)})
	})
	if len(entries) != 2 {
		t.Fatalf("entries = %d", len(entries))
	}
	for _, e := range entries {
		if !json.Valid(e.Payload) || len(e.Payload) > 4096 {
			t.Errorf("payload %d bytes, valid=%v", len(e.Payload), json.Valid(e.Payload))
		}
	}
}

// labjournal must stay a leaf: simchat (SBX-02) and the sim provider (SBX-04) import
// it, and pkg/approval reaches pkg/channels, so a dependency on any of these would be a cycle.
func TestLabjournalImportBoundary(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Skipf("go list unavailable: %v", err)
	}
	deps := string(out)
	if !strings.Contains(deps, "khunquant/pkg/events") {
		t.Fatalf("go list -deps returned no khunquant packages: %.200s", deps)
	}
	for _, banned := range []string{"/pkg/providers", "/pkg/channels", "/pkg/approval", "/pkg/tools", "/pkg/agent", "/pkg/sandbox/lab"} {
		for _, d := range strings.Fields(deps) {
			if strings.HasPrefix(d, "github.com/cryptoquantumwave/khunquant") && (strings.HasSuffix(d, banned) || strings.Contains(d, banned+"/")) {
				t.Errorf("labjournal depends on %s", d)
			}
		}
	}
}
