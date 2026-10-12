package gateway

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/sources"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/triggers"
)

type lockedBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// TestAlerts_DailyComparisonLogsBothPaths: while shadowed, the daily line says what
// the cron path delivered and what the trigger would have.
func TestAlerts_DailyComparisonLogsBothPaths(t *testing.T) {
	useSources(t, false, &countingSource{name: "fake"})
	f := newAssemblyFixture(t, alertsMode(config.ProactiveModeShadow))
	rt := f.services.Perception
	a := f.services.Alerts
	t.Cleanup(func() { logger.SetAdditionalWriter(nil) })

	// Two crossings the trigger would have sent...
	for _, sym := range []string{"BTC/USDT", "ETH/USDT"} {
		p := triggers.PriceCrossParams{Provider: "okx", Symbol: sym, Direction: "above", Threshold: 100}
		if _, err := rt.triggers.Add(context.Background(), triggers.Spec{
			Kind: triggers.KindPriceCross, Params: p.JSON(), Mode: triggers.ModeLive, Origin: "test",
		}); err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		rt.engine.Emitter().Emit(events.Event{
			Kind: events.KindPerceptionMarketTick, Time: now, Source: events.Source{Component: "perception", Name: "market"},
			Payload: sources.MarketTick{Provider: "okx", Symbol: sym, Last: 150, At: now},
			Trust:   events.TrustPtr(proactive.TrustExchange),
		})
	}
	waitUntil(t, "the shadow candidates", func() bool {
		return rt.triggers.ShadowCandidates(triggers.KindPriceCross, triggers.KindIndicatorCross) == 2
	})

	// ...and three the cron path really delivered.
	legacyAlertFired.Store(0)
	legacyAlertFired.Add(3)
	buf := &lockedBuf{}
	logger.SetAdditionalWriter(buf)
	a.compare(rt.triggers)
	if out := buf.String(); !strings.Contains(out, "legacy fired 3, trigger would fire 2") {
		t.Fatalf("log = %q", out)
	}
	// The counts are per interval.
	buf2 := &lockedBuf{}
	logger.SetAdditionalWriter(buf2)
	a.compare(rt.triggers)
	if out := buf2.String(); !strings.Contains(out, "legacy fired 0, trigger would fire 0") {
		t.Fatalf("second log = %q", out)
	}
}

func TestAlerts_NoComparisonOutsideShadow(t *testing.T) {
	useSources(t, false, &countingSource{name: "fake"})
	f := newAssemblyFixture(t, alertsMode(config.ProactiveModeLive))
	buf := &lockedBuf{}
	logger.SetAdditionalWriter(buf)
	t.Cleanup(func() { logger.SetAdditionalWriter(nil) })
	legacyAlertFired.Store(5)
	defer legacyAlertFired.Store(0)
	f.services.Alerts.compare(f.services.Perception.triggers)
	if strings.Contains(buf.String(), "comparison") {
		t.Fatalf("a live registry logged a comparison: %q", buf.String())
	}
}
