package broker_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// readOnlyProvider is a Provider that cannot place orders.
type readOnlyProvider struct{ id string }

func (r readOnlyProvider) ID() string                     { return r.id }
func (r readOnlyProvider) Category() broker.AssetCategory { return broker.CategoryCrypto }
func (r readOnlyProvider) GetMarketStatus(context.Context, string) (broker.MarketStatus, error) {
	return broker.MarketOpen, nil
}

func paperCfg(on bool) *config.Config {
	cfg := &config.Config{}
	cfg.TradingRisk.PaperTradingMode = on
	return cfg
}

func TestTradingProviderFor_PaperModeSimulatesAndReadsStayReal(t *testing.T) {
	const id = "mode-test-paper"
	creates := 0
	mp := NewMockProvider(id)
	mp.CreateOrderFn = func(string, string, string, float64, *float64, map[string]interface{}) (ccxt.Order, error) {
		creates++
		return ccxt.Order{}, nil
	}
	mp.FetchTickerFn = func(symbol string) (ccxt.Ticker, error) {
		return ccxt.Ticker{Symbol: &symbol, Last: float64Ptr(100)}, nil
	}
	broker.RegisterFactory(id, func(*config.Config) (broker.Provider, error) { return mp, nil })
	t.Cleanup(broker.ResetInstanceCache)
	broker.ResetInstanceCache()

	tp, real, err := broker.TradingProviderFor(paperCfg(true), id, "")
	if err != nil {
		t.Fatalf("TradingProviderFor: %v", err)
	}
	if _, ok := tp.(*broker.SimulationProvider); !ok {
		t.Fatalf("paper mode returned %T, want *SimulationProvider", tp)
	}
	if real != broker.Provider(mp) {
		t.Errorf("underlying provider = %v, want the real one", real)
	}

	order, err := tp.CreateOrder(context.Background(), "BTC/USDT", "market", "buy", 1, nil, nil)
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if creates != 0 {
		t.Errorf("real CreateOrder called %d times in paper mode", creates)
	}
	if order.Id == nil || !strings.HasPrefix(*order.Id, broker.PaperOrderIDPrefix) {
		t.Errorf("simulated order ID = %v, want prefix %q", order.Id, broker.PaperOrderIDPrefix)
	}
}

func TestTradingProviderFor_LiveModeReturnsTheRealProvider(t *testing.T) {
	const id = "mode-test-live"
	mp := NewMockProvider(id)
	broker.RegisterFactory(id, func(*config.Config) (broker.Provider, error) { return mp, nil })
	t.Cleanup(broker.ResetInstanceCache)
	broker.ResetInstanceCache()

	tp, real, err := broker.TradingProviderFor(paperCfg(false), id, "")
	if err != nil {
		t.Fatalf("TradingProviderFor: %v", err)
	}
	if tp != broker.TradingProvider(mp) || real != broker.Provider(mp) {
		t.Errorf("live mode must hand back the real provider, got %T / %T", tp, real)
	}
}

func TestTradingProviderFor_Errors(t *testing.T) {
	const id = "mode-test-readonly"
	broker.RegisterFactory(id, func(*config.Config) (broker.Provider, error) { return readOnlyProvider{id: id}, nil })
	t.Cleanup(broker.ResetInstanceCache)
	broker.ResetInstanceCache()

	for _, paper := range []bool{false, true} {
		tp, real, err := broker.TradingProviderFor(paperCfg(paper), id, "")
		if !errors.Is(err, broker.ErrOrderExecutionUnsupported) || tp != nil || real == nil {
			t.Errorf("paper=%v: got (%v, %v, %v), want (nil, real, ErrOrderExecutionUnsupported)", paper, tp, real, err)
		}
	}

	if _, _, err := broker.TradingProviderFor(paperCfg(true), "mode-test-unregistered", ""); err == nil {
		t.Error("an unregistered provider must be an error")
	}
	if _, _, err := broker.TradingProviderFor(nil, id, ""); err == nil {
		t.Error("a nil config must be an error")
	}
}

func TestIsPaperMode(t *testing.T) {
	if broker.IsPaperMode(nil) || broker.IsPaperMode(paperCfg(false)) || !broker.IsPaperMode(paperCfg(true)) {
		t.Error("IsPaperMode must follow trading_risk.paper_trading_mode and treat nil as live")
	}
}
