package gateway

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/orderfeed"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// noFinanceSources keeps the money sources out of a test: they would ask exchanges.
func noFinanceSources(t *testing.T) {
	t.Helper()
	prev := financeSources
	financeSources = func(assemblyDeps) ([]perception.Source, []io.Closer) { return nil, nil }
	t.Cleanup(func() { financeSources = prev })
}

// The daily loss guard's sampler is not replaced by the portfolio source: with
// perception on it is started exactly as it is with perception off, on the config's
// own limit.
func TestFinance_LossGuardSamplerStillStartsWithPerceptionOn(t *testing.T) {
	noFinanceSources(t)
	var limits []float64
	prev := lossGuardStarter
	lossGuardStarter = func(cfg *config.Config) *lossGuardSampler {
		limits = append(limits, cfg.TradingRisk.DailyLossLimitUSD)
		return nil
	}
	t.Cleanup(func() { lossGuardStarter = prev })

	f := newAssemblyFixture(t, compose(liveWithPerception(), func(cfg *config.Config) {
		cfg.TradingRisk.DailyLossLimitUSD = 250
	}))
	if f.services.Perception == nil {
		t.Fatal("perception not built")
	}
	if len(limits) != 1 || limits[0] != 250 {
		t.Fatalf("loss guard started with limits %v, want [250]", limits)
	}
}

func TestFinance_NoSourcesWithoutAnExchangeAccount(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = t.TempDir()
	if srcs, closers := financeSources(assemblyDeps{Cfg: cfg}); len(srcs) != 0 || len(closers) != 0 {
		t.Fatalf("%d sources and %d closers with no account configured", len(srcs), len(closers))
	}
}

func TestFinance_SourcesWhenAnAccountIsConfigured(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = t.TempDir()
	cfg.Proactive.Perception.SnapshotMinutes = 20
	cfg.Exchanges.Binance = config.BinanceExchangeConfig{Enabled: true, Accounts: []config.ExchangeAccount{{Name: "main"}}}

	srcs, closers := financeSources(assemblyDeps{Cfg: cfg})
	defer func() {
		for _, c := range closers {
			_ = c.Close()
		}
	}()
	var names []string
	for _, s := range srcs {
		names = append(names, s.Name())
	}
	if len(names) != 2 || names[0] != "portfolio" || names[1] != "futures" || len(closers) != 1 {
		t.Fatalf("sources %v, closers %d", names, len(closers))
	}
	if h := srcs[0].Health(); h.ExpectedEvery != 20*time.Minute {
		t.Fatalf("portfolio interval = %v, want the configured 20 minutes", h.ExpectedEvery)
	}
}

func TestOrderEvent_KindAndPaper(t *testing.T) {
	at := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		ev   orderfeed.OrderEvent
		want events.Kind
	}{
		{"closed", orderfeed.OrderEvent{Status: "closed"}, events.KindPerceptionOrderFilled},
		{"filled", orderfeed.OrderEvent{Status: "filled"}, events.KindPerceptionOrderFilled},
		{"open", orderfeed.OrderEvent{Status: "open"}, events.KindPerceptionOrderPlaced},
		{"no status", orderfeed.OrderEvent{}, events.KindPerceptionOrderPlaced},
		{"paper, simulated as closed", orderfeed.OrderEvent{Status: "closed", Paper: true}, events.KindPerceptionOrderPlaced},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.ev.At, tc.ev.Provider, tc.ev.Account = at, "okx", "main"
			got := orderEvent(tc.ev)
			if got.Kind != tc.want {
				t.Fatalf("kind = %s, want %s", got.Kind, tc.want)
			}
			if got.TrustLevel() != proactive.TrustExchange || !got.Time.Equal(at) || got.Attrs["subject"] != "okx/main" {
				t.Fatalf("event = %+v", got)
			}
			if got.Attrs["paper"] != tc.ev.Paper {
				t.Fatalf("paper attr = %v", got.Attrs["paper"])
			}
			if p, ok := got.Payload.(orderfeed.OrderEvent); !ok || p.Paper != tc.ev.Paper {
				t.Fatalf("payload = %#v: the Paper flag must travel with the event", got.Payload)
			}
		})
	}
}

func TestFinance_OrdersReachTheEngineAndTheFeedFollowsReloads(t *testing.T) {
	noFinanceSources(t)
	f := newAssemblyFixture(t, liveWithPerception())
	first := f.services.Perception
	if first == nil {
		t.Fatal("perception not built")
	}
	base := orderfeed.ReadStats().Published

	orderfeed.Publish(context.Background(), orderfeed.OrderEvent{Provider: "okx", Symbol: "BTC/USDT", Status: "closed"})
	waitUntil(t, "the order to reach the engine", func() bool { return first.engine.Stats().Processed == 1 })
	if got := orderfeed.ReadStats().Published - base; got != 1 {
		t.Fatalf("published = %d", got)
	}

	// A reload builds the next generation's feed before it stops this one's; the old
	// generation's stop must not take the new publisher away.
	if err := f.reload(liveWithPerception()); err != nil {
		t.Fatal(err)
	}
	second := f.services.Perception
	if second == nil || second == first {
		t.Fatal("reload did not rebuild perception")
	}
	orderfeed.Publish(context.Background(), orderfeed.OrderEvent{Provider: "okx", Symbol: "ETH/USDT"})
	waitUntil(t, "the order to reach the new engine", func() bool { return second.engine.Stats().Processed == 1 })

	// Stopping removes the publisher: a later order goes nowhere and costs nothing.
	second.stop()
	before := orderfeed.ReadStats()
	orderfeed.Publish(context.Background(), orderfeed.OrderEvent{Provider: "okx"})
	if after := orderfeed.ReadStats(); after != before {
		t.Fatalf("counters moved after stop: %+v -> %+v", before, after)
	}
}
