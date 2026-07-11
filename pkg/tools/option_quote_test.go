package tools

import (
	"context"
	"errors"
	"testing"

	ccxt "github.com/ccxt/ccxt/go/v4"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// optionQuoteStubProvider is a minimal broker.OptionMarketDataProvider used to
// exercise OptionQuoteTool.Execute success/error branches.
type optionQuoteStubProvider struct {
	quotes []broker.OptionQuote
	err    error
}

func (p optionQuoteStubProvider) ID() string                     { return "opt-quote-stub" }
func (p optionQuoteStubProvider) Category() broker.AssetCategory { return broker.CategoryStock }
func (p optionQuoteStubProvider) GetMarketStatus(context.Context, string) (broker.MarketStatus, error) {
	return broker.MarketOpen, nil
}
func (p optionQuoteStubProvider) FetchOptionSnapshot(_ context.Context, _ []broker.OptionContract) ([]broker.OptionQuote, error) {
	return p.quotes, p.err
}
func (p optionQuoteStubProvider) FetchOptionOHLCV(context.Context, broker.OptionContract, string, int) ([]ccxt.OHLCV, error) {
	return nil, nil
}

func TestOptionQuoteTool_NameDescriptionParameters(t *testing.T) {
	tool := NewOptionQuoteTool(config.DefaultConfig())
	if tool.Name() != NameOptionQuote {
		t.Errorf("Name() = %q, want %q", tool.Name(), NameOptionQuote)
	}
	if tool.Description() == "" {
		t.Error("Description() should not be empty")
	}
	params := tool.Parameters()
	props, ok := params["properties"].(map[string]any)
	if !ok {
		t.Fatal("properties should be a map")
	}
	for _, prop := range []string{"provider", "account", "underlying", "expiry", "strike", "option_type"} {
		if _, ok := props[prop]; !ok {
			t.Errorf("expected property %q not found", prop)
		}
	}
}

func TestOptionQuoteTool_Execute_Validation(t *testing.T) {
	tool := NewOptionQuoteTool(config.DefaultConfig())

	tests := []struct {
		name string
		args map[string]any
	}{
		{"missing provider", map[string]any{"underlying": "AAPL", "expiry": "2026-08-21", "strike": 320.0, "option_type": "CALL"}},
		{"missing underlying", map[string]any{"provider": "webull", "expiry": "2026-08-21", "strike": 320.0, "option_type": "CALL"}},
		{"missing expiry", map[string]any{"provider": "webull", "underlying": "AAPL", "strike": 320.0, "option_type": "CALL"}},
		{"missing option_type", map[string]any{"provider": "webull", "underlying": "AAPL", "expiry": "2026-08-21", "strike": 320.0}},
		{"zero strike", map[string]any{"provider": "webull", "underlying": "AAPL", "expiry": "2026-08-21", "strike": 0.0, "option_type": "CALL"}},
		{"negative strike", map[string]any{"provider": "webull", "underlying": "AAPL", "expiry": "2026-08-21", "strike": -5.0, "option_type": "CALL"}},
		{"invalid option_type", map[string]any{"provider": "webull", "underlying": "AAPL", "expiry": "2026-08-21", "strike": 320.0, "option_type": "INVALID"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tool.Execute(context.Background(), tt.args)
			if !result.IsError {
				t.Errorf("expected error, got none")
			}
		})
	}
}

func TestOptionQuoteTool_Execute_ProviderNotRegistered(t *testing.T) {
	tool := NewOptionQuoteTool(config.DefaultConfig())
	result := tool.Execute(context.Background(), map[string]any{
		"provider": "does-not-exist-option-quote", "underlying": "AAPL",
		"expiry": "2026-08-21", "strike": 320.0, "option_type": "CALL",
	})
	if !result.IsError {
		t.Fatal("expected error for unregistered provider")
	}
}

// optionQuoteNonMarketDataProvider implements only broker.Provider, not
// broker.OptionMarketDataProvider, to exercise the type-assertion failure branch.
type optionQuoteNonMarketDataProvider struct{}

func (p optionQuoteNonMarketDataProvider) ID() string { return "opt-quote-non-md" }
func (p optionQuoteNonMarketDataProvider) Category() broker.AssetCategory {
	return broker.CategoryStock
}
func (p optionQuoteNonMarketDataProvider) GetMarketStatus(context.Context, string) (broker.MarketStatus, error) {
	return broker.MarketOpen, nil
}

func TestOptionQuoteTool_Execute_WrongProviderType(t *testing.T) {
	const provider = "opt-quote-non-md"
	broker.RegisterFactory(provider, func(*config.Config) (broker.Provider, error) {
		return optionQuoteNonMarketDataProvider{}, nil
	})

	tool := NewOptionQuoteTool(config.DefaultConfig())
	result := tool.Execute(context.Background(), map[string]any{
		"provider": provider, "underlying": "AAPL",
		"expiry": "2026-08-21", "strike": 320.0, "option_type": "CALL",
	})
	if !result.IsError {
		t.Fatal("expected error for provider without option market data support")
	}
}

func TestOptionQuoteTool_Execute_Success(t *testing.T) {
	const provider = "opt-quote-success"
	broker.RegisterFactory(provider, func(*config.Config) (broker.Provider, error) {
		return optionQuoteStubProvider{quotes: []broker.OptionQuote{
			{
				Symbol: "AAPL260821C00320000", Price: 5.2, Bid: 5.1, Ask: 5.3,
				Open: 5.0, PreClose: 4.9, High: 5.5, Low: 4.8,
				Change: 0.3, ChangeRatio: 0.06,
				Delta: 0.5, Gamma: 0.02, Theta: -0.01, Vega: 0.1, Rho: 0.05,
				ImpVol: 0.35, OpenInterest: 1200, Volume: 300, Timestamp: 1700000000000,
			},
		}}, nil
	})

	tool := NewOptionQuoteTool(config.DefaultConfig())
	result := tool.Execute(context.Background(), map[string]any{
		"provider": provider, "underlying": "aapl",
		"expiry": "2026-08-21", "strike": 320.0, "option_type": "call",
	})
	if result.IsError {
		t.Fatalf("expected success, got error: %s", result.ForLLM)
	}
}

func TestOptionQuoteTool_Execute_EmptyQuotes(t *testing.T) {
	const provider = "opt-quote-empty"
	broker.RegisterFactory(provider, func(*config.Config) (broker.Provider, error) {
		return optionQuoteStubProvider{quotes: nil}, nil
	})

	tool := NewOptionQuoteTool(config.DefaultConfig())
	result := tool.Execute(context.Background(), map[string]any{
		"provider": provider, "underlying": "AAPL",
		"expiry": "2026-08-21", "strike": 320.0, "option_type": "CALL",
	})
	if !result.IsError {
		t.Fatal("expected error for empty quote list")
	}
}

func TestOptionQuoteTool_Execute_SubscriptionError(t *testing.T) {
	const provider = "opt-quote-subscription-error"
	broker.RegisterFactory(provider, func(*config.Config) (broker.Provider, error) {
		return optionQuoteStubProvider{err: errors.New("Insufficient permission: US_OPTION subscription required")}, nil
	})

	tool := NewOptionQuoteTool(config.DefaultConfig())
	result := tool.Execute(context.Background(), map[string]any{
		"provider": provider, "underlying": "AAPL",
		"expiry": "2026-08-21", "strike": 320.0, "option_type": "CALL",
	})
	if !result.IsError {
		t.Fatal("expected error for subscription failure")
	}
}

func TestOptionQuoteTool_Execute_GenericUpstreamError(t *testing.T) {
	const provider = "opt-quote-generic-error"
	broker.RegisterFactory(provider, func(*config.Config) (broker.Provider, error) {
		return optionQuoteStubProvider{err: errors.New("network timeout")}, nil
	})

	tool := NewOptionQuoteTool(config.DefaultConfig())
	result := tool.Execute(context.Background(), map[string]any{
		"provider": provider, "underlying": "AAPL",
		"expiry": "2026-08-21", "strike": 320.0, "option_type": "CALL",
	})
	if !result.IsError {
		t.Fatal("expected error for generic upstream failure")
	}
}
