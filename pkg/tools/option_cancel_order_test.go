package tools

import (
	"context"
	"errors"
	"testing"

	ccxt "github.com/ccxt/ccxt/go/v4"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// optionCancelStubProvider is a minimal broker.OptionTradingProvider used to
// exercise OptionCancelOrderTool.Execute success/error branches.
type optionCancelStubProvider struct {
	order ccxt.Order
	err   error
}

func (p optionCancelStubProvider) ID() string                     { return "opt-cancel-stub" }
func (p optionCancelStubProvider) Category() broker.AssetCategory { return broker.CategoryStock }
func (p optionCancelStubProvider) GetMarketStatus(context.Context, string) (broker.MarketStatus, error) {
	return broker.MarketOpen, nil
}
func (p optionCancelStubProvider) PlaceOptionOrder(context.Context, broker.OptionOrderRequest) (ccxt.Order, error) {
	return ccxt.Order{}, nil
}
func (p optionCancelStubProvider) CancelOptionOrder(_ context.Context, _ string) (ccxt.Order, error) {
	return p.order, p.err
}
func (p optionCancelStubProvider) FetchOptionOrder(context.Context, string) (ccxt.Order, error) {
	return ccxt.Order{}, nil
}
func (p optionCancelStubProvider) FetchOpenOptionOrders(context.Context) ([]ccxt.Order, error) {
	return nil, nil
}

// optionNonTradingProvider implements only broker.Provider, not
// broker.OptionTradingProvider, to exercise the type-assertion failure branch.
type optionNonTradingProvider struct{}

func (p optionNonTradingProvider) ID() string                     { return "opt-non-trading" }
func (p optionNonTradingProvider) Category() broker.AssetCategory { return broker.CategoryStock }
func (p optionNonTradingProvider) GetMarketStatus(context.Context, string) (broker.MarketStatus, error) {
	return broker.MarketOpen, nil
}

func TestOptionCancelOrderTool_NameDescriptionParameters(t *testing.T) {
	tool := NewOptionCancelOrderTool(config.DefaultConfig())
	if tool.Name() != NameOptionCancelOrder {
		t.Errorf("Name() = %q, want %q", tool.Name(), NameOptionCancelOrder)
	}
	if tool.Description() == "" {
		t.Error("Description() should not be empty")
	}
	params := tool.Parameters()
	if params["type"] != "object" {
		t.Errorf("type should be object")
	}
	props, ok := params["properties"].(map[string]any)
	if !ok {
		t.Fatal("properties should be a map")
	}
	for _, prop := range []string{"provider", "account", "id"} {
		if _, ok := props[prop]; !ok {
			t.Errorf("expected property %q not found", prop)
		}
	}
}

func TestOptionCancelOrderTool_Execute_MissingArgs(t *testing.T) {
	tool := NewOptionCancelOrderTool(config.DefaultConfig())

	cases := []map[string]any{
		{},
		{"provider": "webull"},
		{"id": "kq123"},
	}
	for _, args := range cases {
		result := tool.Execute(context.Background(), args)
		if !result.IsError {
			t.Errorf("args %v: expected error, got none", args)
		}
	}
}

func TestOptionCancelOrderTool_Execute_ProviderNotRegistered(t *testing.T) {
	tool := NewOptionCancelOrderTool(config.DefaultConfig())
	result := tool.Execute(context.Background(), map[string]any{
		"provider": "does-not-exist-cancel",
		"id":       "kq123",
	})
	if !result.IsError {
		t.Fatal("expected error for unregistered provider")
	}
}

func TestOptionCancelOrderTool_Execute_WrongProviderType(t *testing.T) {
	const provider = "opt-non-trading"
	broker.RegisterFactory(provider, func(*config.Config) (broker.Provider, error) {
		return optionNonTradingProvider{}, nil
	})

	tool := NewOptionCancelOrderTool(config.DefaultConfig())
	result := tool.Execute(context.Background(), map[string]any{
		"provider": provider,
		"id":       "kq123",
	})
	if !result.IsError {
		t.Fatal("expected error for provider without option trading support")
	}
}

func TestOptionCancelOrderTool_Execute_Success(t *testing.T) {
	const provider = "opt-cancel-success"
	status := "cancelled"
	broker.RegisterFactory(provider, func(*config.Config) (broker.Provider, error) {
		return optionCancelStubProvider{order: ccxt.Order{Status: &status}}, nil
	})

	tool := NewOptionCancelOrderTool(config.DefaultConfig())
	result := tool.Execute(context.Background(), map[string]any{
		"provider": provider,
		"id":       "kq123",
	})
	if result.IsError {
		t.Fatalf("expected success, got error: %s", result.ForLLM)
	}
}

func TestOptionCancelOrderTool_Execute_SuccessNilStatus(t *testing.T) {
	const provider = "opt-cancel-nilstatus"
	broker.RegisterFactory(provider, func(*config.Config) (broker.Provider, error) {
		return optionCancelStubProvider{order: ccxt.Order{}}, nil
	})

	tool := NewOptionCancelOrderTool(config.DefaultConfig())
	result := tool.Execute(context.Background(), map[string]any{
		"provider": provider,
		"id":       "kq123",
	})
	if result.IsError {
		t.Fatalf("expected success, got error: %s", result.ForLLM)
	}
}

func TestOptionCancelOrderTool_Execute_UpstreamError(t *testing.T) {
	const provider = "opt-cancel-error"
	broker.RegisterFactory(provider, func(*config.Config) (broker.Provider, error) {
		return optionCancelStubProvider{err: errors.New("cancel rejected")}, nil
	})

	tool := NewOptionCancelOrderTool(config.DefaultConfig())
	result := tool.Execute(context.Background(), map[string]any{
		"provider": provider,
		"id":       "kq123",
	})
	if !result.IsError {
		t.Fatal("expected error for upstream cancel failure")
	}
}
