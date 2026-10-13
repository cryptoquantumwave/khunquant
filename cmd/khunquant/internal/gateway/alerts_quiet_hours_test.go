package gateway

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/attention"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/sources"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

// quietNowProfile stores an owner profile whose active hours are two hours that are
// nowhere near the present, in UTC, so that the real clock the pipeline reads is in
// the owner's quiet hours (as it is at 03:00) whatever time the test runs.
func quietNowProfile(t *testing.T, f *assemblyFixture) {
	t.Helper()
	store, err := intent.NewStore(context.Background(), fixtureDB(t, f))
	if err != nil {
		t.Fatal(err)
	}
	p := intent.DefaultProfile()
	p.Timezone = "UTC"
	start := 18
	if time.Now().UTC().Hour() >= 12 {
		start = 2
	}
	p.ActiveHours = intent.HoursRange{Start: fmt.Sprintf("%02d:00", start), End: fmt.Sprintf("%02d:00", start+2)}
	if _, err := store.PutProfile(context.Background(), p, "test", proactive.TrustUserAuthored); err != nil {
		t.Fatal(err)
	}
	if p.InActiveHours(time.Now()) {
		t.Fatalf("the profile %+v is active now", p.ActiveHours)
	}
}

// TestAlerts_OwnerAlertCrossingInQuietHoursIsDeliveredNotDigested: the owner's price
// alert crosses while the owner is in quiet hours. The decision is a notify with the
// reason that says why it came at night, it is handed over for delivery, and nothing
// waits in the digest.
func TestAlerts_OwnerAlertCrossingInQuietHoursIsDeliveredNotDigested(t *testing.T) {
	useSources(t, false, &countingSource{name: "fake"})
	// A one second grouping window: the outbox sends a decision when its window closes.
	f := newAssemblyFixture(t, compose(alertsMode(config.ProactiveModeLive),
		func(c *config.Config) { c.Proactive.Attention.GroupWindowSeconds = 1 }))
	rt := f.services.Perception
	quietNowProfile(t, f)

	res := alertTool(t, f, tools.NameSetPriceAlert).Execute(tools.WithToolContext(context.Background(), "telegram", "-100group"),
		map[string]any{"action": "create", "provider": "okx", "symbol": "BTC/USDT", "condition": "above", "threshold": 60000.0})
	if res.IsError {
		t.Fatal(res.ForLLM)
	}
	tick := func(p float64) {
		now := time.Now()
		rt.engine.Emitter().Emit(events.Event{
			Kind: events.KindPerceptionMarketTick, Time: now, Source: events.Source{Component: "perception", Name: "market"},
			Payload: sources.MarketTick{Provider: "okx", Symbol: "BTC/USDT", Last: p, At: now},
			Trust:   events.TrustPtr(proactive.TrustExchange),
		})
	}
	tick(59000)
	tick(61000)

	db := fixtureDB(t, f)
	waitUntil(t, "the decision", func() bool {
		var n int
		_ = db.QueryRow(`SELECT COUNT(*) FROM attention_decisions WHERE dedup_key LIKE 'price_cross:%'`).Scan(&n)
		return n == 1
	})
	var disp, reason, cand string
	var shadow int
	if err := db.QueryRow(`SELECT disposition, reason, shadow, candidate_json FROM attention_decisions WHERE dedup_key LIKE 'price_cross:%'`).
		Scan(&disp, &reason, &shadow, &cand); err != nil {
		t.Fatal(err)
	}
	if disp != string(proactive.DispNotify) || reason != attention.ReasonOwnerAlertQuiet || shadow != 0 {
		t.Fatalf("decision = %s/%s shadow=%d, want notify/%s", disp, reason, shadow, attention.ReasonOwnerAlertQuiet)
	}
	if want := `"OwnerRequested":true`; !contains(cand, want) {
		t.Fatalf("the stored candidate lost the mark: %s", cand)
	}
	// The outbox took it to the owner: one send was attempted for it. The fixture has no
	// live Telegram channel, so the send fails and the outbox releases the decision (a
	// delivered_at_ms on a row that is still not delivered); that stamp is what shows the
	// governor put it in front of the delivery layer at night instead of the digest.
	waitUntil(t, "the delivery attempt", func() bool {
		var n int
		_ = db.QueryRow(`SELECT COUNT(*) FROM attention_decisions WHERE dedup_key LIKE 'price_cross:%' AND delivered_at_ms IS NOT NULL`).Scan(&n)
		return n == 1
	})
	// And nothing is waiting for the digest.
	var queued int
	if err := db.QueryRow(`SELECT COUNT(*) FROM attention_digest_queue`).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if queued != 0 {
		t.Fatalf("%d items wait in the digest queue", queued)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
