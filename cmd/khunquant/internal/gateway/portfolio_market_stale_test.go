package gateway

import (
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/calendar"
	"github.com/cryptoquantumwave/khunquant/pkg/triggers"
)

func staleNames(srcs []triggers.StaleSource) []string {
	var out []string
	for _, s := range srcs {
		out = append(out, s.Name)
	}
	return out
}

// The market source is one of the sources data_stale watches (O-13): with an exchange
// account beside the two money sources, without one on its own, at the cadence of the
// source's poll, and Active only while the demands ask for a crypto price.
func TestPortfolioConfig_WatchesTheMarketSource(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Exchanges.Binance = config.BinanceExchangeConfig{Enabled: true, Accounts: []config.ExchangeAccount{{Name: "main"}}}
	demands := perception.NewStaticDemands()
	pc := portfolioConfig(assemblyDeps{Cfg: cfg, Demands: demands})

	srcs := pc.StaleSources()
	if got := strings.Join(staleNames(srcs), ","); got != "market,portfolio,futures" {
		t.Fatalf("stale sources %q", got)
	}
	m := srcs[0]
	if want := time.Duration(cfg.Proactive.Perception.MarketPollSeconds) * time.Second; m.ExpectedEvery != want || want <= 0 {
		t.Fatalf("expected every %v, the poll is %v", m.ExpectedEvery, want)
	}
	if m.Market != calendar.Crypto || m.UnreadableAfter != 1 || m.Active == nil {
		t.Fatalf("%+v", m)
	}
	if m.Active(nil) {
		t.Fatal("active with no price alert or watch")
	}
	demands.Set([]perception.Demand{{Provider: "binance", Symbol: "BTC/USDT", Kind: perception.DemandTicker}})
	if !m.Active(nil) {
		t.Fatal("not active with a price alert")
	}
	demands.Set([]perception.Demand{{Provider: "settrade", Symbol: "PTT", Kind: perception.DemandTicker}})
	if m.Active(nil) {
		t.Fatal("active for an equity venue, whose market may be closed")
	}

	// no exchange account: the money sources do not exist, the market source still does
	// (an alert is blind without it whatever is configured here)
	bare := portfolioConfig(assemblyDeps{Cfg: config.DefaultConfig()})
	if got := strings.Join(staleNames(bare.StaleSources()), ","); got != "market" {
		t.Fatalf("stale sources without an account %q", got)
	}
}

// The registry does not exist when the factories are built; the market source's Active
// reads its demands through the late reference that attachTriggers fills in.
func TestPortfolioConfig_MarketActiveFollowsTheLateRegistry(t *testing.T) {
	late := &lateDemands{}
	pc := portfolioConfig(assemblyDeps{Cfg: config.DefaultConfig(), lateDemands: late})
	m := pc.StaleSources()[0]
	if m.Active(nil) {
		t.Fatal("active before the registry exists")
	}
	late.set(perception.NewStaticDemands(perception.Demand{Provider: "okx", Symbol: "ETH/USDT", Kind: perception.DemandTicker}))
	if !m.Active(nil) {
		t.Fatal("not active once the registry holds a price alert")
	}
}
