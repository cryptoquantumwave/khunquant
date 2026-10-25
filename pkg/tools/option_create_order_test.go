package tools

import (
	"context"
	"fmt"
	"strings"
	"testing"

	ccxt "github.com/ccxt/ccxt/go/v4"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

func TestOptionCreateOrderTool_NameDescriptionParameters(t *testing.T) {
	tool := NewOptionCreateOrderTool(config.DefaultConfig())
	if tool.Name() != NameOptionCreateOrder {
		t.Errorf("Name() = %q, want %q", tool.Name(), NameOptionCreateOrder)
	}
	if tool.Description() == "" {
		t.Error("Description() should not be empty")
	}
	params := tool.Parameters()
	props, ok := params["properties"].(map[string]any)
	if !ok {
		t.Fatal("properties should be a map")
	}
	for _, prop := range []string{"provider", "underlying", "expiry", "strike", "option_type", "side", "quantity", "type", "confirm"} {
		if _, ok := props[prop]; !ok {
			t.Errorf("expected property %q not found", prop)
		}
	}
}

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
		Info: map[string]any{
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

// registerOptionValidationStub registers a live (non-paper) option provider under
// a unique ID, so the validation tests below fail for the reason they assert
// rather than for lack of a provider. Validation runs before the provider is
// built, so a refusal must name the validation rule, never the provider.
func registerOptionValidationStub(t *testing.T) string {
	t.Helper()
	const id = "opt-validation-stub"
	broker.RegisterFactory(id, func(*config.Config) (broker.Provider, error) {
		return optionCreateOrderFullStub{balances: []broker.Balance{{Asset: "USD", Free: 1000}}}, nil
	})
	t.Cleanup(func() {
		broker.RegisterFactory(id, func(*config.Config) (broker.Provider, error) {
			return nil, fmt.Errorf("broker: provider %s not registered", id)
		})
	})
	return id
}

func TestOptionCreateOrderRejectMarket(t *testing.T) {
	provider := registerOptionValidationStub(t)
	tool := NewOptionCreateOrderTool(&config.Config{})

	args := liveOptionOrderArgs(provider, false)
	args["type"] = "market" // Should be rejected
	delete(args, "limit_price")

	result := tool.Execute(context.Background(), args)
	if !result.IsError {
		t.Fatalf("expected error for market order type, got none")
	}
	if !strings.Contains(result.ForLLM, "not supported for options") {
		t.Errorf("expected the order-type rule to be the reason, got: %s", result.ForLLM)
	}
}

func TestOptionCreateOrderRejectGTCOnSell(t *testing.T) {
	provider := registerOptionValidationStub(t)
	tool := NewOptionCreateOrderTool(&config.Config{})

	args := liveOptionOrderArgs(provider, false)
	args["side"] = "sell"
	args["time_in_force"] = "GTC" // Should be rejected on SELL

	result := tool.Execute(context.Background(), args)
	if !result.IsError {
		t.Fatalf("expected error for GTC on SELL, got none")
	}
	if !strings.Contains(result.ForLLM, "GTC") {
		t.Errorf("expected the GTC rule to be the reason, got: %s", result.ForLLM)
	}
}

func TestOptionCreateOrderValidation(t *testing.T) {
	provider := registerOptionValidationStub(t)
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
				"provider":      provider,
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
				"provider":      provider,
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
				"provider":      provider,
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

	tool := NewOptionCreateOrderTool(&config.Config{})

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tool.Execute(context.Background(), tt.args)
			if tt.wantErr && !result.IsError {
				t.Errorf("expected error, got none")
			}
			if tt.wantErr && !strings.Contains(result.ForLLM, tt.errMatch) {
				t.Errorf("expected error containing %q, got: %s", tt.errMatch, result.ForLLM)
			}
			if !tt.wantErr && result.IsError {
				t.Errorf("expected no error, got: %s", result.ForLLM)
			}
		})
	}
}

// optionCreateOrderFullStub is a broker.OptionTradingProvider +
// broker.PortfolioProvider used to exercise OptionCreateOrderTool.Execute's
// live (non-paper) gates: market status, balance preflight, and order
// placement.
type optionCreateOrderFullStub struct {
	marketStatus broker.MarketStatus
	balances     []broker.Balance
	placeErr     error
}

func (s optionCreateOrderFullStub) ID() string                     { return "opt-create-full-stub" }
func (s optionCreateOrderFullStub) Category() broker.AssetCategory { return broker.CategoryStock }
func (s optionCreateOrderFullStub) GetMarketStatus(context.Context, string) (broker.MarketStatus, error) {
	if s.marketStatus == "" {
		return broker.MarketOpen, nil
	}
	return s.marketStatus, nil
}
func (s optionCreateOrderFullStub) PlaceOptionOrder(_ context.Context, req broker.OptionOrderRequest) (ccxt.Order, error) {
	if s.placeErr != nil {
		return ccxt.Order{}, s.placeErr
	}
	id := "kq-full-000001"
	status := "open"
	amount := req.Quantity
	return ccxt.Order{Id: &id, Status: &status, Amount: &amount}, nil
}
func (s optionCreateOrderFullStub) CancelOptionOrder(context.Context, string) (ccxt.Order, error) {
	return ccxt.Order{}, nil
}
func (s optionCreateOrderFullStub) FetchOptionOrder(context.Context, string) (ccxt.Order, error) {
	return ccxt.Order{}, nil
}
func (s optionCreateOrderFullStub) FetchOpenOptionOrders(context.Context) ([]ccxt.Order, error) {
	return nil, nil
}
func (s optionCreateOrderFullStub) GetBalances(context.Context) ([]broker.Balance, error) {
	return s.balances, nil
}
func (s optionCreateOrderFullStub) GetWalletBalances(context.Context, string) ([]broker.WalletBalance, error) {
	return nil, nil
}
func (s optionCreateOrderFullStub) FetchPrice(context.Context, string, string) (float64, error) {
	return 0, nil
}
func (s optionCreateOrderFullStub) SupportedWalletTypes() []string { return []string{"stock"} }

func liveOptionOrderArgs(provider string, confirm bool) map[string]any {
	return map[string]any{
		"provider":      provider,
		"underlying":    "AAPL",
		"expiry":        "2026-08-21",
		"strike":        320.0,
		"option_type":   "CALL",
		"side":          "buy",
		"quantity":      1.0,
		"type":          "limit",
		"limit_price":   1.50,
		"time_in_force": "DAY",
		"confirm":       confirm,
	}
}

func TestOptionCreateOrderTool_Execute_LiveDryRun(t *testing.T) {
	const provider = "opt-create-live-dryrun"
	broker.RegisterFactory(provider, func(*config.Config) (broker.Provider, error) {
		return optionCreateOrderFullStub{balances: []broker.Balance{{Asset: "USD", Free: 1000}}}, nil
	})

	cfg := &config.Config{TradingRisk: config.TradingRiskConfig{PaperTradingMode: false}}
	tool := NewOptionCreateOrderTool(cfg)

	result := tool.Execute(context.Background(), liveOptionOrderArgs(provider, false))
	if result.IsError {
		t.Fatalf("expected dry-run success, got error: %s", result.ForLLM)
	}
}

func TestOptionCreateOrderTool_Execute_LiveSuccess(t *testing.T) {
	const provider = "opt-create-live-success"
	broker.RegisterFactory(provider, func(*config.Config) (broker.Provider, error) {
		return optionCreateOrderFullStub{balances: []broker.Balance{{Asset: "USD", Free: 1000}}}, nil
	})

	cfg := &config.Config{TradingRisk: config.TradingRiskConfig{PaperTradingMode: false}}
	tool := NewOptionCreateOrderTool(cfg)

	result := tool.Execute(context.Background(), liveOptionOrderArgs(provider, true))
	if result.IsError {
		t.Fatalf("expected success, got error: %s", result.ForLLM)
	}
}

func TestOptionCreateOrderTool_Execute_LiveUpstreamError(t *testing.T) {
	const provider = "opt-create-live-error"
	broker.RegisterFactory(provider, func(*config.Config) (broker.Provider, error) {
		return optionCreateOrderFullStub{
			balances: []broker.Balance{{Asset: "USD", Free: 1000}},
			placeErr: fmt.Errorf("broker rejected order"),
		}, nil
	})

	cfg := &config.Config{TradingRisk: config.TradingRiskConfig{PaperTradingMode: false}}
	tool := NewOptionCreateOrderTool(cfg)

	result := tool.Execute(context.Background(), liveOptionOrderArgs(provider, true))
	if !result.IsError {
		t.Fatal("expected error for upstream PlaceOptionOrder failure")
	}
}

func TestOptionCreateOrderTool_Execute_LiveMarketClosed(t *testing.T) {
	const provider = "opt-create-live-closed"
	broker.RegisterFactory(provider, func(*config.Config) (broker.Provider, error) {
		return optionCreateOrderFullStub{
			marketStatus: broker.MarketClosed,
			balances:     []broker.Balance{{Asset: "USD", Free: 1000}},
		}, nil
	})

	cfg := &config.Config{TradingRisk: config.TradingRiskConfig{PaperTradingMode: false}}
	tool := NewOptionCreateOrderTool(cfg)

	result := tool.Execute(context.Background(), liveOptionOrderArgs(provider, true))
	if !result.IsError {
		t.Fatal("expected error for closed market")
	}
}

func TestOptionCreateOrderTool_Execute_LiveInsufficientBalance(t *testing.T) {
	const provider = "opt-create-live-insufficient"
	broker.RegisterFactory(provider, func(*config.Config) (broker.Provider, error) {
		return optionCreateOrderFullStub{balances: []broker.Balance{{Asset: "USD", Free: 1}}}, nil
	})

	cfg := &config.Config{TradingRisk: config.TradingRiskConfig{PaperTradingMode: false}}
	tool := NewOptionCreateOrderTool(cfg)

	result := tool.Execute(context.Background(), liveOptionOrderArgs(provider, true))
	if !result.IsError {
		t.Fatal("expected error for insufficient balance")
	}
}

func TestOptionCreateOrderTool_Execute_LivePositionWarning(t *testing.T) {
	const provider = "opt-create-live-warning"
	broker.RegisterFactory(provider, func(*config.Config) (broker.Provider, error) {
		return optionCreateOrderFullStub{balances: []broker.Balance{{Asset: "USD", Free: 1_000_000}}}, nil
	})

	cfg := &config.Config{TradingRisk: config.TradingRiskConfig{PaperTradingMode: false}}
	tool := NewOptionCreateOrderTool(cfg)

	args := liveOptionOrderArgs(provider, false) // confirm=false, large notional
	args["quantity"] = 200.0                     // 200 * 1.50 * 100 = 30,000 USD > 10,000 threshold
	result := tool.Execute(context.Background(), args)
	if !result.IsError {
		t.Fatal("expected large-order warning error when confirm=false")
	}
}

func TestOptionCreateOrderTool_Execute_LiveProviderNotRegistered(t *testing.T) {
	cfg := &config.Config{TradingRisk: config.TradingRiskConfig{PaperTradingMode: false}}
	tool := NewOptionCreateOrderTool(cfg)

	result := tool.Execute(context.Background(), liveOptionOrderArgs("opt-create-does-not-exist", true))
	if !result.IsError {
		t.Fatal("expected error for unregistered provider")
	}
}

// optionOrderingUnavailableStub wraps optionCreateOrderFullStub and reports
// option ordering as unavailable, mirroring the Webull TH-region block so the
// tool-layer check (which must fire before PlaceOptionOrder, including on the
// confirm=false dry-run path) can be tested without a real Webull adapter.
type optionOrderingUnavailableStub struct {
	optionCreateOrderFullStub
	reason string
}

func (s optionOrderingUnavailableStub) OptionOrderingUnavailableReason() string { return s.reason }

var _ broker.OptionOrderingUnavailable = optionOrderingUnavailableStub{}

func TestOptionCreateOrderTool_Execute_BlockedByOptionOrderingUnavailable(t *testing.T) {
	const provider = "opt-create-ordering-unavailable"
	broker.RegisterFactory(provider, func(*config.Config) (broker.Provider, error) {
		return optionOrderingUnavailableStub{
			optionCreateOrderFullStub: optionCreateOrderFullStub{
				balances: []broker.Balance{{Asset: "USD", Free: 1_000_000}},
				placeErr: fmt.Errorf("PlaceOptionOrder should never be reached when blocked"),
			},
			reason: "webull: option order placement/cancellation is not currently available for Webull Thailand accounts",
		}, nil
	})

	cfg := &config.Config{TradingRisk: config.TradingRiskConfig{PaperTradingMode: false}}
	tool := NewOptionCreateOrderTool(cfg)

	for _, confirm := range []bool{false, true} {
		t.Run(fmt.Sprintf("confirm=%v", confirm), func(t *testing.T) {
			result := tool.Execute(context.Background(), liveOptionOrderArgs(provider, confirm))
			if !result.IsError {
				t.Fatalf("expected the option-ordering-unavailable reason to be reported (confirm=%v), got success: %s", confirm, result.ForLLM)
			}
		})
	}
}

func TestOptionCreateOrderTool_Execute_LiveWrongProviderType(t *testing.T) {
	const provider = "opt-create-live-non-trading"
	broker.RegisterFactory(provider, func(*config.Config) (broker.Provider, error) {
		return optionNonTradingProvider{}, nil
	})

	cfg := &config.Config{TradingRisk: config.TradingRiskConfig{PaperTradingMode: false}}
	tool := NewOptionCreateOrderTool(cfg)

	result := tool.Execute(context.Background(), liveOptionOrderArgs(provider, true))
	if !result.IsError {
		t.Fatal("expected error for provider without option trading support")
	}
}
