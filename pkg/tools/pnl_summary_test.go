package tools

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

func newTestGetPnLSummaryTool(t *testing.T) *GetPnLSummaryTool {
	t.Helper()
	return NewGetPnLSummaryTool(config.DefaultConfig())
}

func TestGetPnLSummaryTool_Name(t *testing.T) {
	tool := newTestGetPnLSummaryTool(t)
	if tool.Name() != NameGetPnLSummary {
		t.Errorf("Name() = %q, want %q", tool.Name(), NameGetPnLSummary)
	}
}

func TestGetPnLSummaryTool_Description(t *testing.T) {
	tool := newTestGetPnLSummaryTool(t)
	desc := tool.Description()
	if desc == "" {
		t.Error("Description() should not be empty")
	}
}

func TestGetPnLSummaryTool_Parameters(t *testing.T) {
	tool := newTestGetPnLSummaryTool(t)
	params := tool.Parameters()

	if params["type"] != "object" {
		t.Errorf("type should be 'object'")
	}

	props, ok := params["properties"].(map[string]any)
	if !ok {
		t.Fatal("properties should be a map")
	}

	expectedProps := []string{"provider", "account", "quote", "assets", "include_realized"}
	for _, prop := range expectedProps {
		if _, ok := props[prop]; !ok {
			t.Errorf("expected property %q not found", prop)
		}
	}
}

func TestGetPnLSummaryTool_Execute_NoArgs(t *testing.T) {
	tool := newTestGetPnLSummaryTool(t)

	result := tool.Execute(context.Background(), map[string]any{})

	// No required args, so should process with defaults
	if result == nil {
		t.Fatal("Execute should return result")
	}
}

func TestGetPnLSummaryTool_Execute_AllExchanges(t *testing.T) {
	tool := newTestGetPnLSummaryTool(t)

	result := tool.Execute(context.Background(), map[string]any{})

	if result == nil {
		t.Fatal("Execute should return result")
	}
}

func TestGetPnLSummaryTool_Execute_SingleProvider(t *testing.T) {
	tool := newTestGetPnLSummaryTool(t)

	result := tool.Execute(context.Background(), map[string]any{
		"provider": "binance",
	})

	if result == nil {
		t.Fatal("Execute should return result")
	}
}

func TestGetPnLSummaryTool_Execute_SpecificAccount(t *testing.T) {
	tool := newTestGetPnLSummaryTool(t)

	result := tool.Execute(context.Background(), map[string]any{
		"provider": "binance",
		"account":  "main",
	})

	if result == nil {
		t.Fatal("Execute should return result")
	}
}

func TestGetPnLSummaryTool_Execute_CustomQuote(t *testing.T) {
	tool := newTestGetPnLSummaryTool(t)

	result := tool.Execute(context.Background(), map[string]any{
		"quote": "USDT",
	})

	if result == nil {
		t.Fatal("Execute should return result")
	}
}

func TestGetPnLSummaryTool_Execute_QuoteAuto(t *testing.T) {
	tool := newTestGetPnLSummaryTool(t)

	result := tool.Execute(context.Background(), map[string]any{
		"quote": "auto",
	})

	if result == nil {
		t.Fatal("Execute should return result")
	}
}

func TestGetPnLSummaryTool_Execute_FilterAssets(t *testing.T) {
	tool := newTestGetPnLSummaryTool(t)

	result := tool.Execute(context.Background(), map[string]any{
		"assets": "BTC,ETH",
	})

	if result == nil {
		t.Fatal("Execute should return result")
	}
}

func TestGetPnLSummaryTool_Execute_SingleAsset(t *testing.T) {
	tool := newTestGetPnLSummaryTool(t)

	result := tool.Execute(context.Background(), map[string]any{
		"assets": "BTC",
	})

	if result == nil {
		t.Fatal("Execute should return result")
	}
}

func TestGetPnLSummaryTool_Execute_IncludeRealized(t *testing.T) {
	tool := newTestGetPnLSummaryTool(t)

	result := tool.Execute(context.Background(), map[string]any{
		"include_realized": true,
	})

	if result == nil {
		t.Fatal("Execute should return result")
	}
}

func TestGetPnLSummaryTool_Execute_AllArgs(t *testing.T) {
	tool := newTestGetPnLSummaryTool(t)

	result := tool.Execute(context.Background(), map[string]any{
		"provider":         "bitkub",
		"account":          "trading",
		"quote":            "THB",
		"assets":           "BTC,ETH,DOGE",
		"include_realized": true,
	})

	if result == nil {
		t.Fatal("Execute should return result")
	}
}

func TestGetPnLSummaryTool_Execute_InvalidProvider(t *testing.T) {
	tool := newTestGetPnLSummaryTool(t)

	result := tool.Execute(context.Background(), map[string]any{
		"provider": "nonexistent",
	})

	if result == nil {
		t.Fatal("Execute should return result")
	}
	// May error for invalid provider, but should not panic
}

func TestGetPnLSummaryTool_Execute_InvalidArgTypes(t *testing.T) {
	tool := newTestGetPnLSummaryTool(t)

	result := tool.Execute(context.Background(), map[string]any{
		"provider":         123,     // int instead of string
		"include_realized": "maybe", // string instead of bool
		"assets":           true,    // bool instead of string
	})

	if result == nil {
		t.Fatal("Execute should return result even with invalid types")
	}
}

type pnlSummaryErrorPortfolioProvider struct{}

func (p pnlSummaryErrorPortfolioProvider) ID() string { return "pnl-summary-error-provider" }

func (p pnlSummaryErrorPortfolioProvider) Category() broker.AssetCategory {
	return broker.CategoryCrypto
}

func (p pnlSummaryErrorPortfolioProvider) GetMarketStatus(context.Context, string) (broker.MarketStatus, error) {
	return broker.MarketUnknown, nil
}

func (p pnlSummaryErrorPortfolioProvider) GetBalances(context.Context) ([]broker.Balance, error) {
	return nil, errors.New("api credentials rejected")
}

func (p pnlSummaryErrorPortfolioProvider) GetWalletBalances(context.Context, string) ([]broker.WalletBalance, error) {
	return nil, errors.New("api credentials rejected")
}

func (p pnlSummaryErrorPortfolioProvider) FetchPrice(context.Context, string, string) (float64, error) {
	return 0, nil
}

func (p pnlSummaryErrorPortfolioProvider) SupportedWalletTypes() []string {
	return []string{"all"}
}

func TestGetPnLSummaryTool_Execute_BalanceFetchErrorIsNotEmptyHoldings(t *testing.T) {
	const provider = "pnl-summary-error-provider"
	broker.RegisterFactory(provider, func(*config.Config) (broker.Provider, error) {
		return pnlSummaryErrorPortfolioProvider{}, nil
	})

	tool := newTestGetPnLSummaryTool(t)
	result := tool.Execute(context.Background(), map[string]any{
		"provider": provider,
		"account":  "main",
	})
	if result == nil {
		t.Fatal("Execute should return result")
	}
	if !strings.Contains(result.ForLLM, "GetWalletBalances: api credentials rejected") {
		t.Fatalf("result does not include balance fetch error: %q", result.ForLLM)
	}
	if strings.Contains(result.ForLLM, "No other holdings found") {
		t.Fatalf("result should not report empty holdings on fetch error: %q", result.ForLLM)
	}
}

func TestGetPnLSummaryTool_Execute_MultipleAssetFormats(t *testing.T) {
	tool := newTestGetPnLSummaryTool(t)

	testCases := []string{
		"BTC,ETH",
		"BTC, ETH, SOL", // with spaces
		"btc,eth",       // lowercase
		"BTC",           // single asset
	}

	for _, assets := range testCases {
		t.Run(assets, func(t *testing.T) {
			result := tool.Execute(context.Background(), map[string]any{
				"assets": assets,
			})
			if result == nil {
				t.Fatal("Execute should return result")
			}
		})
	}
}

func TestGetPnLSummaryTool_Execute_EmptyProvider(t *testing.T) {
	tool := newTestGetPnLSummaryTool(t)

	result := tool.Execute(context.Background(), map[string]any{
		"provider": "",
	})

	if result == nil {
		t.Fatal("Execute should return result")
	}
	// Empty provider should scan all exchanges
}

func TestGetPnLSummaryTool_Execute_EmptyAccount(t *testing.T) {
	tool := newTestGetPnLSummaryTool(t)

	result := tool.Execute(context.Background(), map[string]any{
		"provider": "binance",
		"account":  "",
	})

	if result == nil {
		t.Fatal("Execute should return result")
	}
	// Empty account should use default
}

func TestGetPnLSummaryTool_Execute_NegativeIncludeRealized(t *testing.T) {
	tool := newTestGetPnLSummaryTool(t)

	result := tool.Execute(context.Background(), map[string]any{
		"include_realized": false,
	})

	if result == nil {
		t.Fatal("Execute should return result")
	}
}

// pnlSummaryStockPortfolioProvider is a broker.PortfolioProvider under
// broker.CategoryStock, exercising the stock-adapter PnL branch in Execute
// (parseExtraFloat/parseExtraFloatAny reading avg_cost/market_price/
// current_price/market_value/unrealized_pl/percent_profit/percent_pnl).
type pnlSummaryStockPortfolioProvider struct {
	balances []broker.WalletBalance
}

func (p pnlSummaryStockPortfolioProvider) ID() string { return "pnl-summary-stock-provider" }

func (p pnlSummaryStockPortfolioProvider) Category() broker.AssetCategory {
	return broker.CategoryStock
}

func (p pnlSummaryStockPortfolioProvider) GetMarketStatus(context.Context, string) (broker.MarketStatus, error) {
	return broker.MarketOpen, nil
}

func (p pnlSummaryStockPortfolioProvider) GetBalances(context.Context) ([]broker.Balance, error) {
	out := make([]broker.Balance, len(p.balances))
	for i, b := range p.balances {
		out[i] = b.Balance
	}
	return out, nil
}

func (p pnlSummaryStockPortfolioProvider) GetWalletBalances(context.Context, string) ([]broker.WalletBalance, error) {
	return p.balances, nil
}

func (p pnlSummaryStockPortfolioProvider) FetchPrice(context.Context, string, string) (float64, error) {
	return 0, nil
}

func (p pnlSummaryStockPortfolioProvider) SupportedWalletTypes() []string {
	return []string{"stock"}
}

func TestGetPnLSummaryTool_Execute_StockCategory_MarketPriceKey(t *testing.T) {
	const provider = "pnl-summary-stock-provider-market-price"
	broker.RegisterFactory(provider, func(*config.Config) (broker.Provider, error) {
		return pnlSummaryStockPortfolioProvider{balances: []broker.WalletBalance{
			{
				Balance: broker.Balance{Asset: "AAPL", Free: 10},
				Extra: map[string]string{
					"avg_cost":       "150.00",
					"market_price":   "160.00",
					"market_value":   "1600.00",
					"unrealized_pl":  "100.00",
					"percent_profit": "6.67",
				},
			},
		}}, nil
	})

	tool := newTestGetPnLSummaryTool(t)
	result := tool.Execute(context.Background(), map[string]any{"provider": provider})
	if result.IsError {
		t.Fatalf("expected success, got error: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, "AAPL") {
		t.Errorf("expected AAPL in output, got: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, "160.0000") {
		t.Errorf("expected market_price value reflected in output, got: %s", result.ForLLM)
	}
}

func TestGetPnLSummaryTool_Execute_StockCategory_CurrentPriceFallback(t *testing.T) {
	const provider = "pnl-summary-stock-provider-current-price"
	broker.RegisterFactory(provider, func(*config.Config) (broker.Provider, error) {
		return pnlSummaryStockPortfolioProvider{balances: []broker.WalletBalance{
			{
				Balance: broker.Balance{Asset: "MSFT", Free: 5},
				Extra: map[string]string{
					"avg_cost":      "300.00",
					"current_price": "310.00", // market_price absent -> parseExtraFloatAny falls back
					"percent_pnl":   "3.33",
				},
			},
		}}, nil
	})

	tool := newTestGetPnLSummaryTool(t)
	result := tool.Execute(context.Background(), map[string]any{"provider": provider, "include_realized": true})
	if result.IsError {
		t.Fatalf("expected success, got error: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, "310.0000") {
		t.Errorf("expected current_price fallback reflected in output, got: %s", result.ForLLM)
	}
}

func TestNativeQuoteForProvider(t *testing.T) {
	cases := []struct{ provider, want string }{
		{"bitkub", "THB"},
		{"settrade", "THB"},
		{"binance", "USDT"},
		{"okx", "USDT"},
		{"unknown", "USDT"},
	}
	for _, tc := range cases {
		got := nativeQuoteForProvider(tc.provider)
		if got != tc.want {
			t.Errorf("nativeQuoteForProvider(%q) = %q, want %q", tc.provider, got, tc.want)
		}
	}
}

func TestWalletTypeForPnL(t *testing.T) {
	cases := []struct {
		category broker.AssetCategory
		provider string
		want     string
	}{
		{broker.CategoryStock, "settrade", "stock"},
		{broker.CategoryStock, "webull", "stock"},
		{broker.CategoryCrypto, "okx", "all"},
		{broker.CategoryCrypto, "binance", "all"},
		{broker.CategoryCrypto, "bitkub", "spot"},
		{broker.CategoryCrypto, "unknown", "spot"},
	}
	for _, tc := range cases {
		got := walletTypeForPnL(tc.category, tc.provider)
		if got != tc.want {
			t.Errorf("walletTypeForPnL(%q, %q) = %q, want %q", tc.category, tc.provider, got, tc.want)
		}
	}
}

func TestParseExtraFloat_NilMap(t *testing.T) {
	if got := parseExtraFloat(nil, "key"); got != 0 {
		t.Errorf("parseExtraFloat(nil, key) = %v, want 0", got)
	}
}

func TestParseExtraFloat_MissingKey(t *testing.T) {
	if got := parseExtraFloat(map[string]string{}, "key"); got != 0 {
		t.Errorf("parseExtraFloat empty map = %v, want 0", got)
	}
}

func TestParseExtraFloat_ValidValue(t *testing.T) {
	extra := map[string]string{"price": "3.14"}
	got := parseExtraFloat(extra, "price")
	if got != 3.14 {
		t.Errorf("parseExtraFloat valid = %v, want 3.14", got)
	}
}

func TestParseExtraFloat_InvalidValue(t *testing.T) {
	extra := map[string]string{"price": "not-a-float"}
	if got := parseExtraFloat(extra, "price"); got != 0 {
		t.Errorf("parseExtraFloat invalid = %v, want 0", got)
	}
}

func TestPnlSignStr_Positive(t *testing.T) {
	if got := pnlSignStr(1.5); got != "+" {
		t.Errorf("pnlSignStr(1.5) = %q, want +", got)
	}
}

func TestPnlSignStr_Zero(t *testing.T) {
	if got := pnlSignStr(0); got != "+" {
		t.Errorf("pnlSignStr(0) = %q, want +", got)
	}
}

func TestPnlSignStr_Negative(t *testing.T) {
	if got := pnlSignStr(-1.0); got != "" {
		t.Errorf("pnlSignStr(-1.0) = %q, want empty", got)
	}
}

func TestQuantitiesDiffer(t *testing.T) {
	if quantitiesDiffer(100, 100.5) {
		t.Fatal("0.5% quantity difference should be ignored")
	}
	if !quantitiesDiffer(6.59, 14.86) {
		t.Fatal("large quantity difference should be reported")
	}
	if quantitiesDiffer(0, 0) {
		t.Fatal("zero quantities should not differ")
	}
}
