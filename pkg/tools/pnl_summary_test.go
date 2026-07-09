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
