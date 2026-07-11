package tools

import (
	"context"
	"errors"
	"testing"

	ccxt "github.com/ccxt/ccxt/go/v4"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// optionGetOrderStubProvider is a minimal broker.OptionTradingProvider used to
// exercise OptionGetOrderTool.Execute success/error branches.
type optionGetOrderStubProvider struct {
	order ccxt.Order
	err   error
}

func (p optionGetOrderStubProvider) ID() string                     { return "opt-get-stub" }
func (p optionGetOrderStubProvider) Category() broker.AssetCategory { return broker.CategoryStock }
func (p optionGetOrderStubProvider) GetMarketStatus(context.Context, string) (broker.MarketStatus, error) {
	return broker.MarketOpen, nil
}
func (p optionGetOrderStubProvider) PlaceOptionOrder(context.Context, broker.OptionOrderRequest) (ccxt.Order, error) {
	return ccxt.Order{}, nil
}
func (p optionGetOrderStubProvider) CancelOptionOrder(context.Context, string) (ccxt.Order, error) {
	return ccxt.Order{}, nil
}
func (p optionGetOrderStubProvider) FetchOptionOrder(_ context.Context, _ string) (ccxt.Order, error) {
	return p.order, p.err
}
func (p optionGetOrderStubProvider) FetchOpenOptionOrders(context.Context) ([]ccxt.Order, error) {
	return nil, nil
}

func TestOptionGetOrderTool_NameDescriptionParameters(t *testing.T) {
	tool := NewOptionGetOrderTool(config.DefaultConfig())
	if tool.Name() != NameOptionGetOrder {
		t.Errorf("Name() = %q, want %q", tool.Name(), NameOptionGetOrder)
	}
	if tool.Description() == "" {
		t.Error("Description() should not be empty")
	}
	params := tool.Parameters()
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

func TestOptionGetOrderTool_Execute_MissingArgs(t *testing.T) {
	tool := NewOptionGetOrderTool(config.DefaultConfig())

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

func TestOptionGetOrderTool_Execute_ProviderNotRegistered(t *testing.T) {
	tool := NewOptionGetOrderTool(config.DefaultConfig())
	result := tool.Execute(context.Background(), map[string]any{
		"provider": "does-not-exist-get-order",
		"id":       "kq123",
	})
	if !result.IsError {
		t.Fatal("expected error for unregistered provider")
	}
}

func TestOptionGetOrderTool_Execute_WrongProviderType(t *testing.T) {
	const provider = "opt-get-order-non-trading"
	broker.RegisterFactory(provider, func(*config.Config) (broker.Provider, error) {
		return optionNonTradingProvider{}, nil
	})

	tool := NewOptionGetOrderTool(config.DefaultConfig())
	result := tool.Execute(context.Background(), map[string]any{
		"provider": provider,
		"id":       "kq123",
	})
	if !result.IsError {
		t.Fatal("expected error for provider without option trading support")
	}
}

func TestOptionGetOrderTool_Execute_Success(t *testing.T) {
	const provider = "opt-get-order-success"
	id, symbol, side, orderType, status := "kq123", "AAPL260821C00320000", "buy", "limit", "filled"
	amount, filled, price := 2.0, 2.0, 1.55
	broker.RegisterFactory(provider, func(*config.Config) (broker.Provider, error) {
		return optionGetOrderStubProvider{order: ccxt.Order{
			Id:     &id,
			Symbol: &symbol,
			Side:   &side,
			Type:   &orderType,
			Status: &status,
			Amount: &amount,
			Filled: &filled,
			Price:  &price,
			Info:   map[string]any{"order_id": "9999999999"},
		}}, nil
	})

	tool := NewOptionGetOrderTool(config.DefaultConfig())
	result := tool.Execute(context.Background(), map[string]any{
		"provider": provider,
		"id":       id,
	})
	if result.IsError {
		t.Fatalf("expected success, got error: %s", result.ForLLM)
	}
}

func TestOptionGetOrderTool_Execute_SuccessNilFields(t *testing.T) {
	const provider = "opt-get-order-nilfields"
	broker.RegisterFactory(provider, func(*config.Config) (broker.Provider, error) {
		return optionGetOrderStubProvider{order: ccxt.Order{}}, nil
	})

	tool := NewOptionGetOrderTool(config.DefaultConfig())
	result := tool.Execute(context.Background(), map[string]any{
		"provider": provider,
		"id":       "kq123",
	})
	if result.IsError {
		t.Fatalf("expected success, got error: %s", result.ForLLM)
	}
}

func TestOptionGetOrderTool_Execute_UpstreamError(t *testing.T) {
	const provider = "opt-get-order-error"
	broker.RegisterFactory(provider, func(*config.Config) (broker.Provider, error) {
		return optionGetOrderStubProvider{err: errors.New("order not found")}, nil
	})

	tool := NewOptionGetOrderTool(config.DefaultConfig())
	result := tool.Execute(context.Background(), map[string]any{
		"provider": provider,
		"id":       "kq123",
	})
	if !result.IsError {
		t.Fatal("expected error for upstream fetch failure")
	}
}
