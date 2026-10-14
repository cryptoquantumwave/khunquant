package events

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

func TestEventTrustRoundTrip(t *testing.T) {
	t.Parallel()

	// Not set: the key is absent, so events that never set a trust marshal
	// exactly as they did before the field existed, and decode back to nil.
	t.Run("not set is omitted and decodes to nil", func(t *testing.T) {
		t.Parallel()
		raw, err := json.Marshal(Event{ID: "e1", Kind: KindAgentTurnStart})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "trust") {
			t.Fatalf("an event with no trust marshalled a trust key: %s", raw)
		}
		var back Event
		if err := json.Unmarshal(raw, &back); err != nil {
			t.Fatal(err)
		}
		if back.Trust != nil {
			t.Fatalf("Trust = %v after the round trip, want nil (not set)", *back.Trust)
		}
		if got := back.TrustLevel(); got != proactive.TrustUntrusted {
			t.Fatalf("TrustLevel of an unset trust = %v, want untrusted (fail closed)", got)
		}
	})

	// Explicitly untrusted is NOT the same as not set: it is on the wire.
	t.Run("explicit untrusted survives", func(t *testing.T) {
		t.Parallel()
		raw, err := json.Marshal(Event{ID: "e2", Kind: KindPerceptionNewsItem, Trust: TrustPtr(proactive.TrustUntrusted)})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), `"trust":"untrusted"`) {
			t.Fatalf("explicit untrusted missing from the wire: %s", raw)
		}
		var back Event
		if err := json.Unmarshal(raw, &back); err != nil {
			t.Fatal(err)
		}
		if back.Trust == nil || *back.Trust != proactive.TrustUntrusted {
			t.Fatalf("Trust = %v after the round trip, want a set, untrusted value", back.Trust)
		}
	})

	for tr := proactive.TrustExchange; tr <= proactive.TrustUserAuthored; tr++ {
		raw, err := json.Marshal(Event{ID: "e3", Kind: KindPerceptionPortfolioSnapshot, Trust: TrustPtr(tr)})
		if err != nil {
			t.Fatal(err)
		}
		var back Event
		if err := json.Unmarshal(raw, &back); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if back.Trust == nil || *back.Trust != tr || back.TrustLevel() != tr {
			t.Fatalf("trust %v did not round trip: %s -> %v", tr, raw, back.Trust)
		}
	}

	// A trust the code does not know is an error, never silently "untrusted".
	var back Event
	if err := json.Unmarshal([]byte(`{"id":"e4","kind":"x","trust":"ultra"}`), &back); err == nil {
		t.Fatal("an unknown trust name decoded without error")
	}
}

func TestKnownKindsContainsPerception(t *testing.T) {
	t.Parallel()

	known := map[Kind]bool{}
	for _, k := range KnownKinds() {
		if known[k] {
			t.Errorf("kind %q is listed twice", k)
		}
		known[k] = true
	}

	for _, want := range []Kind{
		"perception.market.tick", "perception.market.candle_closed",
		"perception.portfolio.snapshot", "perception.futures.positions",
		"perception.order.filled", "perception.order.placed",
		"perception.plan.health", "perception.plan.dca_due",
		"perception.clock.session", "perception.clock.day_start", "perception.clock.timer",
		"perception.external.trigger", "perception.device",
		"perception.news.item", "perception.source.health",
		"worldmodel.diff", "trigger.candidate", "attention.decision", "notify.delivered",
		"approval.created", "approval.decided", "approval.expired",
		"journal.step", "journal.workflow", "control.command",
	} {
		if !known[want] {
			t.Errorf("KnownKinds() does not contain %q", want)
		}
	}

	// The kinds that existed before are all still there.
	for _, k := range []Kind{KindAgentTurnStart, KindAgentSubTurnSpawn, KindAgentSubTurnEnd,
		KindAgentSubTurnResultDelivered, KindAgentSubTurnOrphan, KindGatewayReady, KindMCPToolCallEnd} {
		if !known[k] {
			t.Errorf("KnownKinds() lost %q", k)
		}
	}
	if got := len(known); got != 46+24+1 {
		t.Errorf("KnownKinds() has %d kinds, want 71 (46 existing + 24 new + the clock timer)", got)
	}
}
