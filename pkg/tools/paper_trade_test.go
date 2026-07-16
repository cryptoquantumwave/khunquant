package tools

import (
	"context"
	"fmt"
	"strings"
	"testing"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/exchanges"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// reauthMarketDataStub is a MarketDataProvider whose every market-data call
// fails with a wrapped exchanges.ErrNeedsReauth, mimicking a Webull client
// whose session is awaiting in-app approval.
type reauthMarketDataStub struct{ id string }

func (s reauthMarketDataStub) ID() string                     { return s.id }
func (s reauthMarketDataStub) Category() broker.AssetCategory { return broker.CategoryStock }
func (s reauthMarketDataStub) GetMarketStatus(context.Context, string) (broker.MarketStatus, error) {
	return broker.MarketOpen, nil
}
func (s reauthMarketDataStub) FetchTicker(context.Context, string) (ccxt.Ticker, error) {
	return ccxt.Ticker{}, fmt.Errorf("webull: FetchSnapshot: get token: %w", exchanges.ErrNeedsReauth)
}
func (s reauthMarketDataStub) FetchTickers(context.Context, []string) (map[string]ccxt.Ticker, error) {
	return nil, fmt.Errorf("webull: get token: %w", exchanges.ErrNeedsReauth)
}
func (s reauthMarketDataStub) FetchOHLCV(context.Context, string, string, *int64, int) ([]ccxt.OHLCV, error) {
	return nil, fmt.Errorf("webull: get token: %w", exchanges.ErrNeedsReauth)
}
func (s reauthMarketDataStub) FetchOrderBook(context.Context, string, int) (ccxt.OrderBook, error) {
	return ccxt.OrderBook{}, fmt.Errorf("webull: get token: %w", exchanges.ErrNeedsReauth)
}
func (s reauthMarketDataStub) LoadMarkets(context.Context) (map[string]ccxt.MarketInterface, error) {
	return nil, fmt.Errorf("webull: get token: %w", exchanges.ErrNeedsReauth)
}

// TestPaperTradeTool_Execute_SurfacesReauthHint is the regression test for
// the live failure where "buy vrt 0.1" through paper_trade returned a raw
// "needs re-authentication" error with no instruction to call
// webull_reconnect, leaving the agent to flail.
func TestPaperTradeTool_Execute_SurfacesReauthHint(t *testing.T) {
	const provider = "paper-trade-reauth-stub"
	broker.RegisterFactory(provider, func(*config.Config) (broker.Provider, error) {
		return reauthMarketDataStub{id: provider}, nil
	})

	tool := newTestPaperTradeTool(t)
	result := tool.Execute(context.Background(), map[string]any{
		"provider": provider,
		"symbol":   "VRT",
		"type":     "market",
		"side":     "buy",
		"amount":   0.1,
	})
	if !result.IsError {
		t.Fatal("expected an error result")
	}
	if !strings.Contains(result.ForLLM, NameWebullReconnect) {
		t.Fatalf("expected the error to instruct calling %s, got: %s", NameWebullReconnect, result.ForLLM)
	}
}

func newTestPaperTradeTool(t *testing.T) *PaperTradeTool {
	t.Helper()
	return NewPaperTradeTool(config.DefaultConfig())
}

func TestPaperTradeTool_Name(t *testing.T) {
	tool := newTestPaperTradeTool(t)
	if tool.Name() != NamePaperTrade {
		t.Errorf("Name() = %q, want %q", tool.Name(), NamePaperTrade)
	}
}

func TestPaperTradeTool_Description(t *testing.T) {
	tool := newTestPaperTradeTool(t)
	desc := tool.Description()
	if desc == "" {
		t.Error("Description() should not be empty")
	}
}

func TestPaperTradeTool_Parameters(t *testing.T) {
	tool := newTestPaperTradeTool(t)
	params := tool.Parameters()

	if params["type"] != "object" {
		t.Errorf("type should be 'object'")
	}

	props, ok := params["properties"].(map[string]any)
	if !ok {
		t.Fatal("properties should be a map")
	}

	expectedProps := []string{"provider", "account", "symbol", "type", "side", "amount", "price"}
	for _, prop := range expectedProps {
		if _, ok := props[prop]; !ok {
			t.Errorf("expected property %q not found", prop)
		}
	}

	required, ok := params["required"].([]string)
	if !ok {
		t.Fatal("required should be a slice")
	}
	if len(required) != 5 {
		t.Errorf("expected 5 required params, got %d", len(required))
	}
}

func TestPaperTradeTool_Execute_MissingProvider(t *testing.T) {
	tool := newTestPaperTradeTool(t)

	result := tool.Execute(context.Background(), map[string]any{
		"symbol": "BTC/USDT",
		"type":   "market",
		"side":   "buy",
		"amount": 1.0,
	})

	if !result.IsError {
		t.Error("missing provider should return error")
	}
}

func TestPaperTradeTool_Execute_MissingSymbol(t *testing.T) {
	tool := newTestPaperTradeTool(t)

	result := tool.Execute(context.Background(), map[string]any{
		"provider": "binance",
		"type":     "market",
		"side":     "buy",
		"amount":   1.0,
	})

	if !result.IsError {
		t.Error("missing symbol should return error")
	}
}

func TestPaperTradeTool_Execute_MissingType(t *testing.T) {
	tool := newTestPaperTradeTool(t)

	result := tool.Execute(context.Background(), map[string]any{
		"provider": "binance",
		"symbol":   "BTC/USDT",
		"side":     "buy",
		"amount":   1.0,
	})

	if !result.IsError {
		t.Error("missing type should return error")
	}
}

func TestPaperTradeTool_Execute_MissingSide(t *testing.T) {
	tool := newTestPaperTradeTool(t)

	result := tool.Execute(context.Background(), map[string]any{
		"provider": "binance",
		"symbol":   "BTC/USDT",
		"type":     "market",
		"amount":   1.0,
	})

	if !result.IsError {
		t.Error("missing side should return error")
	}
}

func TestPaperTradeTool_Execute_MissingAmount(t *testing.T) {
	tool := newTestPaperTradeTool(t)

	result := tool.Execute(context.Background(), map[string]any{
		"provider": "binance",
		"symbol":   "BTC/USDT",
		"type":     "market",
		"side":     "buy",
	})

	if !result.IsError {
		t.Error("missing amount should return error")
	}
}

func TestPaperTradeTool_Execute_ZeroAmount(t *testing.T) {
	tool := newTestPaperTradeTool(t)

	result := tool.Execute(context.Background(), map[string]any{
		"provider": "binance",
		"symbol":   "BTC/USDT",
		"type":     "market",
		"side":     "buy",
		"amount":   0.0,
	})

	if !result.IsError {
		t.Error("zero amount should return error")
	}
}

func TestPaperTradeTool_Execute_NegativeAmount(t *testing.T) {
	tool := newTestPaperTradeTool(t)

	result := tool.Execute(context.Background(), map[string]any{
		"provider": "binance",
		"symbol":   "BTC/USDT",
		"type":     "market",
		"side":     "buy",
		"amount":   -1.0,
	})

	if !result.IsError {
		t.Error("negative amount should return error")
	}
}

func TestPaperTradeTool_Execute_LimitOrderMissingPrice(t *testing.T) {
	tool := newTestPaperTradeTool(t)

	result := tool.Execute(context.Background(), map[string]any{
		"provider": "binance",
		"symbol":   "BTC/USDT",
		"type":     "limit",
		"side":     "buy",
		"amount":   1.0,
	})

	if !result.IsError {
		t.Error("limit order without price should return error")
	}
	if !strings.Contains(result.ForLLM, "price") {
		t.Errorf("error should mention price, got %q", result.ForLLM)
	}
}

func TestPaperTradeTool_Execute_MarketOrderIgnoresPrice(t *testing.T) {
	tool := newTestPaperTradeTool(t)

	result := tool.Execute(context.Background(), map[string]any{
		"provider": "binance",
		"symbol":   "BTC/USDT",
		"type":     "market",
		"side":     "buy",
		"amount":   1.0,
		"price":    50000.0, // ignored for market orders
	})

	// May error due to missing provider, but should not require price for market
	if result == nil {
		t.Fatal("Execute should return result")
	}
}

func TestPaperTradeTool_Execute_BuySide(t *testing.T) {
	tool := newTestPaperTradeTool(t)

	result := tool.Execute(context.Background(), map[string]any{
		"provider": "binance",
		"symbol":   "BTC/USDT",
		"type":     "market",
		"side":     "buy",
		"amount":   0.5,
	})

	if result == nil {
		t.Fatal("Execute should return result")
	}
}

func TestPaperTradeTool_Execute_SellSide(t *testing.T) {
	tool := newTestPaperTradeTool(t)

	result := tool.Execute(context.Background(), map[string]any{
		"provider": "binance",
		"symbol":   "BTC/USDT",
		"type":     "market",
		"side":     "sell",
		"amount":   0.5,
	})

	if result == nil {
		t.Fatal("Execute should return result")
	}
}

func TestPaperTradeTool_Execute_WithAccount(t *testing.T) {
	tool := newTestPaperTradeTool(t)

	result := tool.Execute(context.Background(), map[string]any{
		"provider": "binance",
		"account":  "main",
		"symbol":   "BTC/USDT",
		"type":     "market",
		"side":     "buy",
		"amount":   1.0,
	})

	if result == nil {
		t.Fatal("Execute should return result")
	}
}

func TestPaperTradeTool_Execute_LimitBuyOrder(t *testing.T) {
	tool := newTestPaperTradeTool(t)

	result := tool.Execute(context.Background(), map[string]any{
		"provider": "binance",
		"symbol":   "BTC/USDT",
		"type":     "limit",
		"side":     "buy",
		"amount":   1.0,
		"price":    30000.0,
	})

	if result == nil {
		t.Fatal("Execute should return result")
	}
}

func TestPaperTradeTool_Execute_LimitSellOrder(t *testing.T) {
	tool := newTestPaperTradeTool(t)

	result := tool.Execute(context.Background(), map[string]any{
		"provider": "binance",
		"symbol":   "BTC/USDT",
		"type":     "limit",
		"side":     "sell",
		"amount":   1.0,
		"price":    70000.0,
	})

	if result == nil {
		t.Fatal("Execute should return result")
	}
}

func TestPaperTradeTool_Execute_InvalidArgTypes(t *testing.T) {
	tool := newTestPaperTradeTool(t)

	result := tool.Execute(context.Background(), map[string]any{
		"provider": 123,
		"symbol":   true,
		"amount":   "not_a_number",
	})

	if result == nil {
		t.Fatal("Execute should return result even with invalid types")
	}
}

func TestPaperTradeTool_Execute_SmallAmount(t *testing.T) {
	tool := newTestPaperTradeTool(t)

	result := tool.Execute(context.Background(), map[string]any{
		"provider": "binance",
		"symbol":   "BTC/USDT",
		"type":     "market",
		"side":     "buy",
		"amount":   0.0001,
	})

	if result == nil {
		t.Fatal("Execute should return result")
	}
}

func TestPaperTradeTool_Execute_LargeAmount(t *testing.T) {
	tool := newTestPaperTradeTool(t)

	result := tool.Execute(context.Background(), map[string]any{
		"provider": "binance",
		"symbol":   "BTC/USDT",
		"type":     "market",
		"side":     "buy",
		"amount":   1000.0,
	})

	if result == nil {
		t.Fatal("Execute should return result")
	}
}

func TestPaperTradeTool_Execute_DifferentSymbols(t *testing.T) {
	tool := newTestPaperTradeTool(t)

	symbols := []string{"BTC/USDT", "ETH/USDT", "SOL/USDT"}

	for _, sym := range symbols {
		t.Run(sym, func(t *testing.T) {
			result := tool.Execute(context.Background(), map[string]any{
				"provider": "binance",
				"symbol":   sym,
				"type":     "market",
				"side":     "buy",
				"amount":   1.0,
			})
			if result == nil {
				t.Fatal("Execute should return result")
			}
		})
	}
}
