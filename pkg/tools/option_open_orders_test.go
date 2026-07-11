package tools

import (
	"context"
	"errors"
	"strings"
	"testing"

	ccxt "github.com/ccxt/ccxt/go/v4"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// optionOpenOrdersStubProvider is a minimal broker.OptionTradingProvider used
// to exercise OptionOpenOrdersTool.Execute success/error branches.
type optionOpenOrdersStubProvider struct {
	orders []ccxt.Order
	err    error
}

func (p optionOpenOrdersStubProvider) ID() string                     { return "opt-open-stub" }
func (p optionOpenOrdersStubProvider) Category() broker.AssetCategory { return broker.CategoryStock }
func (p optionOpenOrdersStubProvider) GetMarketStatus(context.Context, string) (broker.MarketStatus, error) {
	return broker.MarketOpen, nil
}
func (p optionOpenOrdersStubProvider) PlaceOptionOrder(context.Context, broker.OptionOrderRequest) (ccxt.Order, error) {
	return ccxt.Order{}, nil
}
func (p optionOpenOrdersStubProvider) CancelOptionOrder(context.Context, string) (ccxt.Order, error) {
	return ccxt.Order{}, nil
}
func (p optionOpenOrdersStubProvider) FetchOptionOrder(context.Context, string) (ccxt.Order, error) {
	return ccxt.Order{}, nil
}
func (p optionOpenOrdersStubProvider) FetchOpenOptionOrders(context.Context) ([]ccxt.Order, error) {
	return p.orders, p.err
}

func TestOptionOpenOrdersTool_NameDescriptionParameters(t *testing.T) {
	tool := NewOptionOpenOrdersTool(config.DefaultConfig())
	if tool.Name() != NameOptionOpenOrders {
		t.Errorf("Name() = %q, want %q", tool.Name(), NameOptionOpenOrders)
	}
	if tool.Description() == "" {
		t.Error("Description() should not be empty")
	}
	params := tool.Parameters()
	props, ok := params["properties"].(map[string]any)
	if !ok {
		t.Fatal("properties should be a map")
	}
	for _, prop := range []string{"provider", "account"} {
		if _, ok := props[prop]; !ok {
			t.Errorf("expected property %q not found", prop)
		}
	}
}

func TestOptionOpenOrdersTool_Execute_MissingProvider(t *testing.T) {
	tool := NewOptionOpenOrdersTool(config.DefaultConfig())
	result := tool.Execute(context.Background(), map[string]any{})
	if !result.IsError {
		t.Fatal("expected error for missing provider")
	}
}

func TestOptionOpenOrdersTool_Execute_ProviderNotRegistered(t *testing.T) {
	tool := NewOptionOpenOrdersTool(config.DefaultConfig())
	result := tool.Execute(context.Background(), map[string]any{
		"provider": "does-not-exist-open-orders",
	})
	if !result.IsError {
		t.Fatal("expected error for unregistered provider")
	}
}

func TestOptionOpenOrdersTool_Execute_WrongProviderType(t *testing.T) {
	const provider = "opt-open-orders-non-trading"
	broker.RegisterFactory(provider, func(*config.Config) (broker.Provider, error) {
		return optionNonTradingProvider{}, nil
	})

	tool := NewOptionOpenOrdersTool(config.DefaultConfig())
	result := tool.Execute(context.Background(), map[string]any{
		"provider": provider,
	})
	if !result.IsError {
		t.Fatal("expected error for provider without option trading support")
	}
}

func TestOptionOpenOrdersTool_Execute_Empty(t *testing.T) {
	const provider = "opt-open-orders-empty"
	broker.RegisterFactory(provider, func(*config.Config) (broker.Provider, error) {
		return optionOpenOrdersStubProvider{orders: nil}, nil
	})

	tool := NewOptionOpenOrdersTool(config.DefaultConfig())
	result := tool.Execute(context.Background(), map[string]any{
		"provider": provider,
	})
	if result.IsError {
		t.Fatalf("expected success, got error: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, "No open option orders") {
		t.Errorf("expected empty-orders message, got: %s", result.ForLLM)
	}
}

func TestOptionOpenOrdersTool_Execute_Success(t *testing.T) {
	const provider = "opt-open-orders-success"
	id1, id2 := "kq1", "kq2"
	symbol, side, orderType, status := "AAPL260821C00320000", "buy", "limit", "open"
	amount, filled, price := 3.0, 1.0, 2.05
	broker.RegisterFactory(provider, func(*config.Config) (broker.Provider, error) {
		return optionOpenOrdersStubProvider{orders: []ccxt.Order{
			{Id: &id1, Symbol: &symbol, Side: &side, Type: &orderType, Status: &status, Amount: &amount, Filled: &filled, Price: &price},
			{Id: &id2}, // nil fields exercise the "-" defaults
		}}, nil
	})

	tool := NewOptionOpenOrdersTool(config.DefaultConfig())
	result := tool.Execute(context.Background(), map[string]any{
		"provider": provider,
	})
	if result.IsError {
		t.Fatalf("expected success, got error: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, "kq1") || !strings.Contains(result.ForLLM, "kq2") {
		t.Errorf("expected both order IDs in output, got: %s", result.ForLLM)
	}
}

func TestOptionOpenOrdersTool_Execute_UpstreamError(t *testing.T) {
	const provider = "opt-open-orders-error"
	broker.RegisterFactory(provider, func(*config.Config) (broker.Provider, error) {
		return optionOpenOrdersStubProvider{err: errors.New("fetch failed")}, nil
	})

	tool := NewOptionOpenOrdersTool(config.DefaultConfig())
	result := tool.Execute(context.Background(), map[string]any{
		"provider": provider,
	})
	if !result.IsError {
		t.Fatal("expected error for upstream fetch failure")
	}
}
