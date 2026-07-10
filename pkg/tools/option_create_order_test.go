package tools

import (
	"context"
	"fmt"
	"testing"

	ccxt "github.com/ccxt/ccxt/go/v4"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// mockOptionTradingProvider is a fake OptionTradingProvider for testing.
type mockOptionTradingProvider struct {
	shouldFail bool
}

func (m *mockOptionTradingProvider) ID() string                     { return "mock" }
func (m *mockOptionTradingProvider) Category() broker.AssetCategory { return broker.CategoryStock }
func (m *mockOptionTradingProvider) GetMarketStatus(ctx context.Context, symbol string) (broker.MarketStatus, error) {
	return broker.MarketOpen, nil
}
func (m *mockOptionTradingProvider) PlaceOptionOrder(ctx context.Context, req broker.OptionOrderRequest) (ccxt.Order, error) {
	if m.shouldFail {
		return ccxt.Order{}, fmt.Errorf("mock error")
	}
	id := "kq1234567890abcdef"
	sym := req.Underlying
	side := req.Side
	orderType := req.OrderType
	status := "open"
	amount := req.Quantity
	return ccxt.Order{
		Id:     &id,
		Symbol: &sym,
		Side:   &side,
		Type:   &orderType,
		Amount: &amount,
		Status: &status,
		Info: map[string]interface{}{
			"order_id": "9999999999",
		},
	}, nil
}
func (m *mockOptionTradingProvider) CancelOptionOrder(ctx context.Context, clientOrderID string) (ccxt.Order, error) {
	return ccxt.Order{}, nil
}
func (m *mockOptionTradingProvider) FetchOptionOrder(ctx context.Context, clientOrderID string) (ccxt.Order, error) {
	return ccxt.Order{}, nil
}
func (m *mockOptionTradingProvider) FetchOpenOptionOrders(ctx context.Context) ([]ccxt.Order, error) {
	return nil, nil
}

func TestOptionCreateOrderRejectMarket(t *testing.T) {
	cfg := &config.Config{
		Exchanges: config.ExchangesConfig{
			Webull: config.WebullExchangeConfig{
				Enabled: true,
				Accounts: []config.WebullExchangeAccount{
					{
						ExchangeAccount: config.ExchangeAccount{
							APIKey: *config.NewSecureString("key"),
							Secret: *config.NewSecureString("secret"),
						},
						AccountID: "ACC123",
					},
				},
			},
		},
		TradingRisk: config.TradingRiskConfig{
			PaperTradingMode: true, // Use paper trading to avoid needing a real provider
		},
	}

	tool := NewOptionCreateOrderTool(cfg)

	args := map[string]any{
		"provider":    "webull",
		"underlying":  "AAPL",
		"expiry":      "2026-08-21",
		"strike":      320.0,
		"option_type": "CALL",
		"side":        "buy",
		"quantity":    1.0,
		"type":        "market", // Should be rejected
		"confirm":     false,
	}

	result := tool.Execute(context.Background(), args)
	if !result.IsError {
		t.Errorf("expected error for market order type, got none")
	}
}

func TestOptionCreateOrderRejectGTCOnSell(t *testing.T) {
	cfg := &config.Config{
		Exchanges: config.ExchangesConfig{
			Webull: config.WebullExchangeConfig{
				Enabled: true,
				Accounts: []config.WebullExchangeAccount{
					{
						ExchangeAccount: config.ExchangeAccount{
							APIKey: *config.NewSecureString("key"),
							Secret: *config.NewSecureString("secret"),
						},
						AccountID: "ACC123",
					},
				},
			},
		},
		TradingRisk: config.TradingRiskConfig{
			PaperTradingMode: true,
		},
	}

	tool := NewOptionCreateOrderTool(cfg)

	args := map[string]any{
		"provider":      "webull",
		"underlying":    "AAPL",
		"expiry":        "2026-08-21",
		"strike":        320.0,
		"option_type":   "CALL",
		"side":          "sell",
		"quantity":      1.0,
		"type":          "limit",
		"limit_price":   1.50,
		"time_in_force": "GTC", // Should be rejected on SELL
		"confirm":       false,
	}

	result := tool.Execute(context.Background(), args)
	if !result.IsError {
		t.Errorf("expected error for GTC on SELL, got none")
	}
}

func TestOptionCreateOrderValidation(t *testing.T) {
	tests := []struct {
		name     string
		args     map[string]any
		wantErr  bool
		errMatch string
	}{
		{
			name: "missing provider",
			args: map[string]any{
				"underlying":  "AAPL",
				"expiry":      "2026-08-21",
				"strike":      320.0,
				"option_type": "CALL",
				"side":        "buy",
				"quantity":    1.0,
				"type":        "limit",
				"limit_price": 1.50,
				"confirm":     false,
			},
			wantErr:  true,
			errMatch: "required",
		},
		{
			name: "invalid option type",
			args: map[string]any{
				"provider":      "webull",
				"underlying":    "AAPL",
				"expiry":        "2026-08-21",
				"strike":        320.0,
				"option_type":   "INVALID",
				"side":          "buy",
				"quantity":      1.0,
				"type":          "limit",
				"limit_price":   1.50,
				"time_in_force": "DAY",
				"confirm":       false,
			},
			wantErr:  true,
			errMatch: "must be CALL or PUT",
		},
		{
			name: "negative quantity",
			args: map[string]any{
				"provider":      "webull",
				"underlying":    "AAPL",
				"expiry":        "2026-08-21",
				"strike":        320.0,
				"option_type":   "CALL",
				"side":          "buy",
				"quantity":      -1.0,
				"type":          "limit",
				"limit_price":   1.50,
				"time_in_force": "DAY",
				"confirm":       false,
			},
			wantErr:  true,
			errMatch: "must be positive",
		},
		{
			name: "limit order without limit price",
			args: map[string]any{
				"provider":      "webull",
				"underlying":    "AAPL",
				"expiry":        "2026-08-21",
				"strike":        320.0,
				"option_type":   "CALL",
				"side":          "buy",
				"quantity":      1.0,
				"type":          "limit",
				"time_in_force": "DAY",
				"confirm":       false,
			},
			wantErr:  true,
			errMatch: "limit_price is required",
		},
	}

	cfg := &config.Config{
		TradingRisk: config.TradingRiskConfig{
			PaperTradingMode: true,
		},
	}

	tool := NewOptionCreateOrderTool(cfg)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tool.Execute(context.Background(), tt.args)
			if tt.wantErr && !result.IsError {
				t.Errorf("expected error, got none")
			}
			if !tt.wantErr && result.IsError {
				t.Errorf("expected no error, got: %s", result.ForLLM)
			}
		})
	}
}
