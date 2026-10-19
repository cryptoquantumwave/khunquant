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

// mockFuturesProvider is a minimal broker.FuturesProvider for unit tests.
type mockFuturesProvider struct {
	markPriceFn                     func(ctx context.Context, symbol string) (float64, error)
	fundingRateFn                   func(ctx context.Context, symbol string) (ccxt.FundingRate, error)
	fundingRatesFn                  func(ctx context.Context, symbols []string) (map[string]ccxt.FundingRate, error)
	loadMarketsFn                   func(ctx context.Context) (map[string]ccxt.MarketInterface, error)
	fetchFuturesOrderFn             func(ctx context.Context, id, symbol string) (ccxt.Order, error)
	fetchFuturesPositionsFn         func(ctx context.Context, symbols []string) ([]ccxt.Position, error)
	fetchFundingHistoryFn           func(ctx context.Context, symbol string, since *int64, limit int) ([]ccxt.FundingHistory, error)
	fetchPublicFundingRateHistoryFn func(ctx context.Context, symbol string, since *int64, limit int) ([]ccxt.FundingRateHistory, error)
}

func (m *mockFuturesProvider) ID() string                     { return "mock" }
func (m *mockFuturesProvider) Category() broker.AssetCategory { return broker.CategoryCrypto }
func (m *mockFuturesProvider) GetMarketStatus(_ context.Context, _ string) (broker.MarketStatus, error) {
	return broker.MarketOpen, nil
}
func (m *mockFuturesProvider) FetchFuturesMarkPrice(ctx context.Context, symbol string) (float64, error) {
	if m.markPriceFn != nil {
		return m.markPriceFn(ctx, symbol)
	}
	return 0, errors.New("not implemented")
}
func (m *mockFuturesProvider) FetchFuturesFundingRate(ctx context.Context, symbol string) (ccxt.FundingRate, error) {
	if m.fundingRateFn != nil {
		return m.fundingRateFn(ctx, symbol)
	}
	return ccxt.FundingRate{}, errors.New("not implemented")
}
func (m *mockFuturesProvider) FetchFuturesFundingRates(ctx context.Context, symbols []string) (map[string]ccxt.FundingRate, error) {
	if m.fundingRatesFn != nil {
		return m.fundingRatesFn(ctx, symbols)
	}
	return map[string]ccxt.FundingRate{}, nil
}
func (m *mockFuturesProvider) LoadFuturesMarkets(ctx context.Context) (map[string]ccxt.MarketInterface, error) {
	if m.loadMarketsFn != nil {
		return m.loadMarketsFn(ctx)
	}
	return nil, errors.New("not implemented")
}
func (m *mockFuturesProvider) FetchFuturesOrder(ctx context.Context, id, symbol string) (ccxt.Order, error) {
	if m.fetchFuturesOrderFn != nil {
		return m.fetchFuturesOrderFn(ctx, id, symbol)
	}
	return ccxt.Order{}, errors.New("not implemented")
}

// Unused interface methods — satisfy the interface.
func (m *mockFuturesProvider) SetFuturesLeverage(_ context.Context, _ string, _ int64, _, _ string) (map[string]interface{}, error) {
	return nil, nil
}
func (m *mockFuturesProvider) CreateFuturesOrder(_ context.Context, _ broker.FuturesOrderRequest) (ccxt.Order, error) { //nolint:gocritic
	return ccxt.Order{}, nil
}
func (m *mockFuturesProvider) FetchFuturesOpenOrders(_ context.Context, _ string) ([]ccxt.Order, error) {
	return nil, nil
}
func (m *mockFuturesProvider) FetchFuturesPositions(ctx context.Context, symbols []string) ([]ccxt.Position, error) {
	if m.fetchFuturesPositionsFn != nil {
		return m.fetchFuturesPositionsFn(ctx, symbols)
	}
	return nil, nil
}
func (m *mockFuturesProvider) FetchFuturesFundingHistory(ctx context.Context, symbol string, since *int64, limit int) ([]ccxt.FundingHistory, error) {
	if m.fetchFundingHistoryFn != nil {
		return m.fetchFundingHistoryFn(ctx, symbol, since, limit)
	}
	return nil, nil
}
func (m *mockFuturesProvider) FetchPublicFundingRateHistory(ctx context.Context, symbol string, since *int64, limit int) ([]ccxt.FundingRateHistory, error) {
	if m.fetchPublicFundingRateHistoryFn != nil {
		return m.fetchPublicFundingRateHistoryFn(ctx, symbol, since, limit)
	}
	return nil, nil
}
func (m *mockFuturesProvider) CancelFuturesOrder(_ context.Context, _, _ string) (ccxt.Order, error) {
	return ccxt.Order{}, nil
}
func (m *mockFuturesProvider) CancelAllFuturesOrders(_ context.Context, _ string) ([]ccxt.Order, error) {
	return nil, nil
}

// --- helper function coverage ---

func TestFuturesCloseSide(t *testing.T) {
	if got := futuresCloseSide("short"); got != "buy" {
		t.Errorf("futuresCloseSide(short) = %q, want buy", got)
	}
	if got := futuresCloseSide("SHORT"); got != "buy" {
		t.Errorf("futuresCloseSide(SHORT) = %q, want buy", got)
	}
	if got := futuresCloseSide("long"); got != "sell" {
		t.Errorf("futuresCloseSide(long) = %q, want sell", got)
	}
	if got := futuresCloseSide(""); got != "sell" {
		t.Errorf("futuresCloseSide('') = %q, want sell", got)
	}
}

func TestFuturesPositionSide(t *testing.T) {
	order, pos, err := futuresPositionSide("short")
	if err != nil || order != "sell" || pos != "short" {
		t.Errorf("futuresPositionSide(short) = %q,%q,%v, want sell,short,nil", order, pos, err)
	}
	order, pos, err = futuresPositionSide("sell")
	if err != nil || order != "sell" || pos != "short" {
		t.Errorf("futuresPositionSide(sell) = %q,%q,%v", order, pos, err)
	}
	order, pos, err = futuresPositionSide("buy")
	if err != nil || order != "buy" || pos != "long" {
		t.Errorf("futuresPositionSide(buy) = %q,%q,%v", order, pos, err)
	}
	_, _, err = futuresPositionSide("invalid")
	if err == nil {
		t.Error("futuresPositionSide(invalid) should return error")
	}
}

func TestMarginModeOrDefault(t *testing.T) {
	if got := marginModeOrDefault(""); got != "cross" {
		t.Errorf("marginModeOrDefault('') = %q, want cross", got)
	}
	if got := marginModeOrDefault("ISOLATED"); got != "isolated" {
		t.Errorf("marginModeOrDefault(ISOLATED) = %q, want isolated", got)
	}
}

func TestCopyToolParams(t *testing.T) {
	src := map[string]interface{}{"a": 1, "b": "x"}
	out := copyToolParams(src)
	if out["a"] != 1 || out["b"] != "x" {
		t.Errorf("copyToolParams result = %v, want original values", out)
	}
	// Non-map input returns empty map
	if got := copyToolParams("notamap"); len(got) != 0 {
		t.Errorf("copyToolParams(string) = %v, want empty map", got)
	}
}

func TestContractsFromNotional(t *testing.T) {
	// 1000 USD notional, mark=50000, contractSize=0.001 → usdPerContract=50 → contracts=20
	got, err := contractsFromNotional(1000, 50000, 0.001, 1)
	if err != nil {
		t.Fatalf("contractsFromNotional error: %v", err)
	}
	if got != 20 {
		t.Errorf("contractsFromNotional = %v, want 20", got)
	}

	// Zero mark price should error
	_, err = contractsFromNotional(1000, 0, 1, 1)
	if err == nil {
		t.Error("expected error for zero mark price")
	}
}

func TestMarginHealthFromPosition(t *testing.T) {
	mark := 50000.0
	liq := 25000.0 // dist=50% > 20 → safe
	pos := ccxt.Position{MarkPrice: &mark, LiquidationPrice: &liq}
	distPct, _, label := marginHealthFromPosition(pos)
	if label != "safe" {
		t.Errorf("label = %q, want safe", label)
	}
	if distPct <= 0 {
		t.Errorf("distPct = %v, expected positive", distPct)
	}

	// Near liquidation (warn range: dist 5-20%)
	liq2 := 45000.0 // dist=10% → warn
	pos2 := ccxt.Position{MarkPrice: &mark, LiquidationPrice: &liq2}
	_, _, label2 := marginHealthFromPosition(pos2)
	if label2 != "warn" {
		t.Errorf("label = %q, want warn", label2)
	}

	// Critical
	liq3 := 49000.0
	pos3 := ccxt.Position{MarkPrice: &mark, LiquidationPrice: &liq3}
	_, _, label3 := marginHealthFromPosition(pos3)
	if label3 != "critical" {
		t.Errorf("label = %q, want critical", label3)
	}

	// No mark/liq prices → unknown
	pos4 := ccxt.Position{}
	_, _, label4 := marginHealthFromPosition(pos4)
	if label4 != "unknown" {
		t.Errorf("label = %q, want unknown", label4)
	}
}

// --- format helpers ---

func TestPriceText(t *testing.T) {
	if got := priceText(nil); got != "" {
		t.Errorf("priceText(nil) = %q, want empty", got)
	}
	p := 42000.0
	got := priceText(&p)
	if !strings.Contains(got, "42000") {
		t.Errorf("priceText = %q, expected 42000", got)
	}
}

func TestProtectionText(t *testing.T) {
	if got := protectionText(0, 0); got != "" {
		t.Errorf("protectionText(0,0) = %q, want empty", got)
	}
	got := protectionText(40000, 60000)
	if !strings.Contains(got, "SL") || !strings.Contains(got, "TP") {
		t.Errorf("protectionText = %q, expected SL and TP", got)
	}
	got = protectionText(40000, 0)
	if !strings.Contains(got, "SL") || strings.Contains(got, "TP") {
		t.Errorf("protectionText(sl only) = %q", got)
	}
}

func TestOrderIDHelper(t *testing.T) {
	empty := ""
	o := ccxt.Order{Id: &empty}
	if got := orderID(o); got != "-" {
		t.Errorf("orderID(empty) = %q, want -", got)
	}
	id := "abc123"
	o2 := ccxt.Order{Id: &id}
	if got := orderID(o2); got != "abc123" {
		t.Errorf("orderID = %q, want abc123", got)
	}
}

func TestFormatOrderLine(t *testing.T) {
	id := "ord1"
	o := ccxt.Order{Id: &id}
	if got := formatOrderLine("entry", o, nil); !strings.Contains(got, "ord1") {
		t.Errorf("formatOrderLine = %q", got)
	}
	got := formatOrderLine("entry", ccxt.Order{}, &errorString{"boom"})
	if !strings.Contains(got, "failed") {
		t.Errorf("formatOrderLine(err) = %q", got)
	}
}

type errorString struct{ s string }

func (e *errorString) Error() string { return e.s }

func TestFormatFuturesOrder(t *testing.T) {
	sym := "BTC/USDT:USDT"
	side := "buy"
	typ := "market"
	amount := 1.0
	got := formatFuturesOrder("binance", "order123", ccxt.Order{
		Symbol: &sym,
		Side:   &side,
		Type:   &typ,
		Amount: &amount,
	})
	if !strings.Contains(got, "order123") || !strings.Contains(got, "BTC/USDT") {
		t.Errorf("formatFuturesOrder = %q", got)
	}
}

func TestFormatFuturesPositions_Empty(t *testing.T) {
	got := formatFuturesPositions("binance", nil)
	if !strings.Contains(got, "No active") {
		t.Errorf("formatFuturesPositions(empty) = %q", got)
	}
}

func TestFormatFuturesPositions_WithData(t *testing.T) {
	sym := "BTC/USDT:USDT"
	side := "long"
	contracts := 2.0
	got := formatFuturesPositions("binance", []ccxt.Position{
		{Symbol: &sym, Side: &side, Contracts: &contracts},
	})
	if !strings.Contains(got, "BTC/USDT") {
		t.Errorf("formatFuturesPositions = %q", got)
	}
}

func TestFormatFundingRate(t *testing.T) {
	rate := 0.0001
	mark := 50000.0
	got := formatFundingRate("binance", "BTC/USDT:USDT", ccxt.FundingRate{
		FundingRate: &rate,
		MarkPrice:   &mark,
	})
	if !strings.Contains(got, "BTC/USDT") || !strings.Contains(got, "0.0001") {
		t.Errorf("formatFundingRate = %q", got)
	}
}

func TestFormatFundingHistory_Empty(t *testing.T) {
	got := formatFundingHistory(nil)
	if !strings.Contains(got, "No funding") {
		t.Errorf("formatFundingHistory(nil) = %q", got)
	}
}

func TestFormatFundingHistory_WithData(t *testing.T) {
	amt := 5.0
	cur := "USDT"
	dt := "2026-01-01"
	got := formatFundingHistory([]ccxt.FundingHistory{
		{Amount: &amt, Code: &cur, Datetime: &dt},
	})
	if !strings.Contains(got, "5") {
		t.Errorf("formatFundingHistory = %q", got)
	}
}

func TestFuturesStrPtr(t *testing.T) {
	if got := futuresStrPtr(nil); got != "-" {
		t.Errorf("futuresStrPtr(nil) = %q, want -", got)
	}
	empty := ""
	if got := futuresStrPtr(&empty); got != "-" {
		t.Errorf("futuresStrPtr(empty) = %q, want -", got)
	}
	s := "hello"
	if got := futuresStrPtr(&s); got != "hello" {
		t.Errorf("futuresStrPtr = %q, want hello", got)
	}
}

func TestFuturesFloatPtr(t *testing.T) {
	if got := futuresFloatPtr(nil); got != "-" {
		t.Errorf("futuresFloatPtr(nil) = %q, want -", got)
	}
	f := 1.5
	got := futuresFloatPtr(&f)
	if !strings.Contains(got, "1.5") {
		t.Errorf("futuresFloatPtr = %q", got)
	}
}

// --- tool metadata coverage ---

func TestFuturesToolMetadata(t *testing.T) {
	cfg := config.DefaultConfig()

	tools := []struct {
		name string
		tool interface {
			Name() string
			Description() string
			Parameters() map[string]any
		}
	}{
		{"SetLeverage", NewFuturesSetLeverageTool(cfg)},
		{"OpenPosition", NewFuturesOpenPositionTool(cfg)},
		{"GetOrder", NewFuturesGetOrderTool(cfg)},
		{"GetPositions", NewFuturesGetPositionsTool(cfg)},
		{"GetFunding", NewFuturesGetFundingTool(cfg)},
		{"ClosePosition", NewFuturesClosePositionTool(cfg)},
		{"ReducePosition", NewFuturesReducePositionTool(cfg)},
		{"EmergencyFlatten", NewFuturesEmergencyFlattenTool(cfg)},
		{"ValidateMarket", NewFuturesValidateMarketTool(cfg)},
		{"RiskSummary", NewFuturesRiskSummaryTool(cfg)},
		{"EstimateFundingFee", NewFuturesEstimateFundingFeeTool(cfg)},
		{"ModifyProtection", NewFuturesModifyProtectionTool(cfg)},
		{"CancelOrders", NewFuturesCancelOrdersTool(cfg)},
	}

	for _, tc := range tools {
		t.Run(tc.name, func(t *testing.T) {
			name := tc.tool.Name()
			if name == "" {
				t.Errorf("%s.Name() is empty", tc.name)
			}
			desc := tc.tool.Description()
			if desc == "" {
				t.Errorf("%s.Description() is empty", tc.name)
			}
			params := tc.tool.Parameters()
			if params == nil {
				t.Errorf("%s.Parameters() is nil", tc.name)
			}
		})
	}
}

// --- Execute error path coverage for tools without tests ---

func TestFuturesGetOrder_MissingArgs(t *testing.T) {
	cfg := config.DefaultConfig()
	tool := NewFuturesGetOrderTool(cfg)
	res := tool.Execute(context.Background(), map[string]any{})
	if !res.IsError {
		t.Error("expected error for missing required args")
	}
	if !strings.Contains(res.ForLLM, "required") {
		t.Errorf("expected 'required' in error, got: %s", res.ForLLM)
	}
}

func TestFuturesGetFunding_MissingArgs(t *testing.T) {
	cfg := config.DefaultConfig()
	tool := NewFuturesGetFundingTool(cfg)
	res := tool.Execute(context.Background(), map[string]any{})
	if !res.IsError {
		t.Error("expected error for missing required args")
	}
}

func TestFuturesRiskSummary_MissingProvider(t *testing.T) {
	cfg := config.DefaultConfig()
	tool := NewFuturesRiskSummaryTool(cfg)
	res := tool.Execute(context.Background(), map[string]any{})
	if !res.IsError {
		t.Error("expected error for missing provider")
	}
}

func TestFuturesEstimateFundingFee_MissingProvider(t *testing.T) {
	cfg := config.DefaultConfig()
	tool := NewFuturesEstimateFundingFeeTool(cfg)
	res := tool.Execute(context.Background(), map[string]any{})
	if !res.IsError {
		t.Error("expected error for missing provider")
	}
}

func TestFuturesModifyProtection_MissingArgs(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.TradingRisk.AllowLeverage = true
	tool := NewFuturesModifyProtectionTool(cfg)
	res := tool.Execute(context.Background(), map[string]any{})
	if !res.IsError {
		t.Error("expected error for missing args")
	}
}

func TestFuturesModifyProtection_RequiresLeverage(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.TradingRisk.AllowLeverage = false
	tool := NewFuturesModifyProtectionTool(cfg)
	res := tool.Execute(context.Background(), map[string]any{
		"provider": "binance", "symbol": "BTC/USDT:USDT", "confirm": false,
	})
	if !res.IsError {
		t.Error("expected error when allow_leverage=false")
	}
}

func TestFuturesClosePosition_MissingSymbol(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.TradingRisk.AllowLeverage = true
	tool := NewFuturesClosePositionTool(cfg)
	res := tool.Execute(context.Background(), map[string]any{
		"provider": "binance",
	})
	if !res.IsError {
		t.Error("expected error for missing symbol")
	}
}

func TestFuturesClosePosition_LimitWithoutPrice(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.TradingRisk.AllowLeverage = true
	tool := NewFuturesClosePositionTool(cfg)
	res := tool.Execute(context.Background(), map[string]any{
		"provider":   "binance",
		"symbol":     "BTC/USDT:USDT",
		"order_type": "limit",
		"confirm":    false,
	})
	if !res.IsError {
		t.Error("expected error for limit without limit_price")
	}
}

func TestFuturesValidateMarket_MissingSymbol(t *testing.T) {
	cfg := config.DefaultConfig()
	tool := NewFuturesValidateMarketTool(cfg)
	res := tool.Execute(context.Background(), map[string]any{
		"provider": "binance",
	})
	if !res.IsError {
		t.Error("expected error for missing symbol")
	}
}

func TestFuturesGetPositions_WithSymbol(t *testing.T) {
	// Tests the symbol != "" code path — provider lookup fails without credentials,
	// but we confirm the function runs without panic and returns an error result.
	cfg := config.DefaultConfig()
	tool := NewFuturesGetPositionsTool(cfg)
	res := tool.Execute(context.Background(), map[string]any{
		"provider": "binance",
		"symbol":   "BTC/USDT:USDT",
	})
	if res == nil {
		t.Error("Execute returned nil")
	}
	// Expect an error result (no credentials configured)
	if !res.IsError {
		t.Log("unexpected success (may have credentials configured)")
	}
}

func TestFuturesSetLeverage_DryRun(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.TradingRisk.AllowLeverage = true
	tool := NewFuturesSetLeverageTool(cfg)
	res := tool.Execute(context.Background(), map[string]any{
		"provider": "binance",
		"symbol":   "BTC/USDT:USDT",
		"leverage": 5.0,
		"confirm":  false,
	})
	if res.IsError {
		// Allow error from provider lookup (no config) but not from missing args
		if strings.Contains(res.ForLLM, "required") {
			t.Errorf("unexpected required-arg error: %s", res.ForLLM)
		}
	}
}

func TestFuturesOpenPosition_MissingSymbol(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.TradingRisk.AllowLeverage = true
	tool := NewFuturesOpenPositionTool(cfg)
	res := tool.Execute(context.Background(), map[string]any{
		"provider": "binance",
		"side":     "long",
		"amount":   0.01,
		"leverage": 3.0,
		"confirm":  false,
	})
	if !res.IsError {
		t.Error("expected error for missing symbol")
	}
}

func TestFuturesReducePosition_MissingSymbol(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.TradingRisk.AllowLeverage = true
	tool := NewFuturesReducePositionTool(cfg)
	res := tool.Execute(context.Background(), map[string]any{
		"provider": "binance",
		"amount":   0.5,
		"confirm":  false,
	})
	if !res.IsError {
		t.Error("expected error for missing symbol")
	}
}

// --- tests for 0%-covered futures helper functions ---

func boolPtr(b bool) *bool      { return &b }
func strPtr2(s string) *string  { return &s }
func f64Ptr(f float64) *float64 { return &f }

func TestEstimateFuturesNotional_ExplicitPrice(t *testing.T) {
	fp := &mockFuturesProvider{}
	price := 50000.0
	notional, src, err := estimateFuturesNotional(context.Background(), fp, "BTC/USDT:USDT", 0.1, &price)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if src != "explicit" {
		t.Errorf("source = %q, want explicit", src)
	}
	if notional != 5000.0 {
		t.Errorf("notional = %v, want 5000", notional)
	}
}

func TestEstimateFuturesNotional_MarkPrice(t *testing.T) {
	fp := &mockFuturesProvider{
		markPriceFn: func(_ context.Context, _ string) (float64, error) {
			return 40000.0, nil
		},
	}
	notional, src, err := estimateFuturesNotional(context.Background(), fp, "BTC/USDT:USDT", 0.5, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if src != "mark" {
		t.Errorf("source = %q, want mark", src)
	}
	if notional != 20000.0 {
		t.Errorf("notional = %v, want 20000", notional)
	}
}

func TestEstimateFuturesNotional_FundingMark(t *testing.T) {
	markPrice := 30000.0
	fp := &mockFuturesProvider{
		markPriceFn: func(_ context.Context, _ string) (float64, error) {
			return 0, errors.New("unavailable")
		},
		fundingRateFn: func(_ context.Context, _ string) (ccxt.FundingRate, error) {
			return ccxt.FundingRate{MarkPrice: &markPrice}, nil
		},
	}
	notional, src, err := estimateFuturesNotional(context.Background(), fp, "ETH/USDT:USDT", 1.0, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if src != "funding_mark" {
		t.Errorf("source = %q, want funding_mark", src)
	}
	if notional != 30000.0 {
		t.Errorf("notional = %v, want 30000", notional)
	}
}

func TestEstimateFuturesNotional_NoPriceAvailable(t *testing.T) {
	fp := &mockFuturesProvider{
		markPriceFn: func(_ context.Context, _ string) (float64, error) {
			return 0, errors.New("unavailable")
		},
		fundingRateFn: func(_ context.Context, _ string) (ccxt.FundingRate, error) {
			return ccxt.FundingRate{}, errors.New("unavailable")
		},
	}
	_, _, err := estimateFuturesNotional(context.Background(), fp, "BTC/USDT:USDT", 1.0, nil)
	if err == nil {
		t.Error("expected error when no price source is available")
	}
}

func TestValidateActiveSwapMarket_LoadError(t *testing.T) {
	fp := &mockFuturesProvider{
		loadMarketsFn: func(_ context.Context) (map[string]ccxt.MarketInterface, error) {
			return nil, errors.New("exchange down")
		},
	}
	_, err := validateActiveSwapMarket(context.Background(), fp, "BTC/USDT:USDT", 10)
	if err == nil || !strings.Contains(err.Error(), "cannot load futures markets") {
		t.Errorf("expected load-error, got: %v", err)
	}
}

func TestValidateActiveSwapMarket_SymbolNotFound(t *testing.T) {
	fp := &mockFuturesProvider{
		loadMarketsFn: func(_ context.Context) (map[string]ccxt.MarketInterface, error) {
			return map[string]ccxt.MarketInterface{}, nil
		},
	}
	_, err := validateActiveSwapMarket(context.Background(), fp, "UNKNOWN/USDT:USDT", 0)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected symbol-not-found error, got: %v", err)
	}
}

func TestValidateActiveSwapMarket_Inactive(t *testing.T) {
	inactive := false
	swap := true
	fp := &mockFuturesProvider{
		loadMarketsFn: func(_ context.Context) (map[string]ccxt.MarketInterface, error) {
			m := ccxt.MarketInterface{Active: &inactive, Swap: &swap}
			return map[string]ccxt.MarketInterface{"BTC/USDT:USDT": m}, nil
		},
	}
	_, err := validateActiveSwapMarket(context.Background(), fp, "BTC/USDT:USDT", 0)
	if err == nil || !strings.Contains(err.Error(), "not active") {
		t.Errorf("expected inactive error, got: %v", err)
	}
}

func TestValidateActiveSwapMarket_NotSwap(t *testing.T) {
	active := true
	notSwap := false
	typ := "spot"
	fp := &mockFuturesProvider{
		loadMarketsFn: func(_ context.Context) (map[string]ccxt.MarketInterface, error) {
			m := ccxt.MarketInterface{Active: &active, Swap: &notSwap, Type: &typ}
			return map[string]ccxt.MarketInterface{"BTC/USDT": m}, nil
		},
	}
	_, err := validateActiveSwapMarket(context.Background(), fp, "BTC/USDT", 0)
	if err == nil || !strings.Contains(err.Error(), "not a perpetual swap") {
		t.Errorf("expected non-swap error, got: %v", err)
	}
}

func TestValidateActiveSwapMarket_LeverageExceedsMax(t *testing.T) {
	active := true
	swap := true
	maxLev := float64(20)
	fp := &mockFuturesProvider{
		loadMarketsFn: func(_ context.Context) (map[string]ccxt.MarketInterface, error) {
			m := ccxt.MarketInterface{
				Active: &active,
				Swap:   &swap,
				Limits: ccxt.Limits{
					Leverage: ccxt.MinMax{Max: &maxLev},
				},
			}
			return map[string]ccxt.MarketInterface{"BTC/USDT:USDT": m}, nil
		},
	}
	_, err := validateActiveSwapMarket(context.Background(), fp, "BTC/USDT:USDT", 50)
	if err == nil || !strings.Contains(err.Error(), "exceeds market maximum") {
		t.Errorf("expected leverage-exceeds error, got: %v", err)
	}
}

func TestValidateActiveSwapMarket_ValidMarket(t *testing.T) {
	active := true
	swap := true
	maxLev := float64(100)
	fp := &mockFuturesProvider{
		loadMarketsFn: func(_ context.Context) (map[string]ccxt.MarketInterface, error) {
			m := ccxt.MarketInterface{
				Active: &active,
				Swap:   &swap,
				Limits: ccxt.Limits{
					Leverage: ccxt.MinMax{Max: &maxLev},
				},
			}
			return map[string]ccxt.MarketInterface{"BTC/USDT:USDT": m}, nil
		},
	}
	_, err := validateActiveSwapMarket(context.Background(), fp, "BTC/USDT:USDT", 10)
	if err != nil {
		t.Errorf("unexpected error for valid market: %v", err)
	}
}

func TestVerifyFuturesFill_FetchError(t *testing.T) {
	fp := &mockFuturesProvider{
		fetchFuturesOrderFn: func(_ context.Context, _, _ string) (ccxt.Order, error) {
			return ccxt.Order{}, errors.New("order not found")
		},
	}
	_, _, _, err := verifyFuturesFill(context.Background(), fp, "123", "BTC/USDT:USDT", 0.1)
	if err == nil || !strings.Contains(err.Error(), "fill verification") {
		t.Errorf("expected fill verification error, got: %v", err)
	}
}

func TestVerifyFuturesFill_FullFill(t *testing.T) {
	filled := 0.1
	status := "closed"
	fp := &mockFuturesProvider{
		fetchFuturesOrderFn: func(_ context.Context, _, _ string) (ccxt.Order, error) {
			return ccxt.Order{Filled: &filled, Status: &status}, nil
		},
	}
	gotFilled, gotStatus, isPartial, err := verifyFuturesFill(context.Background(), fp, "123", "BTC/USDT:USDT", 0.1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotFilled != 0.1 {
		t.Errorf("filled = %v, want 0.1", gotFilled)
	}
	if gotStatus != "closed" {
		t.Errorf("status = %q, want closed", gotStatus)
	}
	if isPartial {
		t.Error("expected full fill, got partial")
	}
}

func TestVerifyFuturesFill_PartialFill(t *testing.T) {
	filled := 0.05
	status := "open"
	fp := &mockFuturesProvider{
		fetchFuturesOrderFn: func(_ context.Context, _, _ string) (ccxt.Order, error) {
			return ccxt.Order{Filled: &filled, Status: &status}, nil
		},
	}
	_, _, isPartial, err := verifyFuturesFill(context.Background(), fp, "456", "ETH/USDT:USDT", 0.1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !isPartial {
		t.Error("expected partial fill")
	}
}

func TestVerifyFuturesFill_NilFilledAndStatus(t *testing.T) {
	fp := &mockFuturesProvider{
		fetchFuturesOrderFn: func(_ context.Context, _, _ string) (ccxt.Order, error) {
			return ccxt.Order{}, nil // Filled=nil, Status=nil
		},
	}
	gotFilled, gotStatus, isPartial, err := verifyFuturesFill(context.Background(), fp, "789", "BTC/USDT:USDT", 0.1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotFilled != 0 {
		t.Errorf("filled = %v, want 0", gotFilled)
	}
	if gotStatus != "unknown" {
		t.Errorf("status = %q, want unknown", gotStatus)
	}
	if isPartial {
		t.Error("expected non-partial for zero filled")
	}
}

// --- Execute function tests using mock futuresProviderFn ---

// injectMockFuturesProvider replaces futuresProviderFn for the duration of a test.
// Returns a cleanup function that restores the original.
// unlimitedLimiter always allows orders (for tests that check post-rate-limit paths).
type unlimitedLimiter struct{}

func (unlimitedLimiter) Allow(_ string) bool                       { return true }
func (unlimitedLimiter) Status() map[string]broker.RateLimitStatus { return nil }

func injectMockFuturesProvider(mp *mockFuturesProvider) func() {
	origFn := futuresProviderFn
	origLimiter := broker.DefaultLimiter
	futuresProviderFn = func(_ context.Context, _ *config.Config, _, _ string) (broker.FuturesProvider, error) {
		return mp, nil
	}
	broker.DefaultLimiter = unlimitedLimiter{}
	return func() {
		futuresProviderFn = origFn
		broker.DefaultLimiter = origLimiter
	}
}

// --- FuturesValidateMarketTool ---

func TestFuturesValidateMarket_SuccessPath(t *testing.T) {
	active := true
	swap := true
	linear := true
	settle := "USDT"
	typ := "swap"
	taker := 0.0005
	maker := 0.0002
	contractSize := 0.001
	minAmt := 0.01
	minCost := 5.0
	minLev := 1.0
	maxLev := 100.0

	mp := &mockFuturesProvider{
		loadMarketsFn: func(_ context.Context) (map[string]ccxt.MarketInterface, error) {
			return map[string]ccxt.MarketInterface{
				"BTC/USDT:USDT": {
					Active:       &active,
					Swap:         &swap,
					Linear:       &linear,
					Settle:       &settle,
					Type:         &typ,
					Taker:        &taker,
					Maker:        &maker,
					ContractSize: &contractSize,
					Limits: ccxt.Limits{
						Amount:   ccxt.MinMax{Min: &minAmt},
						Cost:     ccxt.MinMax{Min: &minCost},
						Leverage: ccxt.MinMax{Min: &minLev, Max: &maxLev},
					},
				},
			}, nil
		},
	}
	defer injectMockFuturesProvider(mp)()

	cfg := config.DefaultConfig()
	cfg.TradingRisk.AllowLeverage = true
	tool := NewFuturesValidateMarketTool(cfg)
	res := tool.Execute(context.Background(), map[string]any{
		"provider": "binance",
		"symbol":   "BTC/USDT:USDT",
	})
	if res.IsError {
		t.Errorf("expected success, got error: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "valid for futures trading") {
		t.Errorf("expected 'valid for futures trading' in output, got: %s", res.ForLLM)
	}
}

func TestFuturesValidateMarket_SymbolNotFound(t *testing.T) {
	mp := &mockFuturesProvider{
		loadMarketsFn: func(_ context.Context) (map[string]ccxt.MarketInterface, error) {
			return map[string]ccxt.MarketInterface{}, nil
		},
	}
	defer injectMockFuturesProvider(mp)()

	cfg := config.DefaultConfig()
	tool := NewFuturesValidateMarketTool(cfg)
	res := tool.Execute(context.Background(), map[string]any{
		"provider": "binance",
		"symbol":   "UNKNOWN/USDT:USDT",
	})
	if !res.IsError {
		t.Error("expected error for unknown symbol")
	}
}

// --- FuturesRiskSummaryTool ---

func TestFuturesRiskSummary_NoPositions(t *testing.T) {
	mp := &mockFuturesProvider{}
	mp.fetchFuturesPositionsFn = func(_ context.Context, _ []string) ([]ccxt.Position, error) {
		return []ccxt.Position{}, nil
	}
	defer injectMockFuturesProvider(mp)()

	cfg := config.DefaultConfig()
	tool := NewFuturesRiskSummaryTool(cfg)
	res := tool.Execute(context.Background(), map[string]any{
		"provider": "binance",
	})
	if res.IsError {
		t.Errorf("unexpected error: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "No active futures positions") {
		t.Errorf("expected 'No active futures positions', got: %s", res.ForLLM)
	}
}

func TestFuturesRiskSummary_WithPositions(t *testing.T) {
	contracts := 1.0
	sym := "BTC/USDT:USDT"
	side := "long"
	entry := 45000.0
	mark := 50000.0
	liq := 30000.0
	lev := 10.0
	upnl := 5000.0
	marginMode := "cross"

	mp := &mockFuturesProvider{}
	mp.fetchFuturesPositionsFn = func(_ context.Context, _ []string) ([]ccxt.Position, error) {
		return []ccxt.Position{{
			Symbol:           &sym,
			Side:             &side,
			EntryPrice:       &entry,
			MarkPrice:        &mark,
			LiquidationPrice: &liq,
			Leverage:         &lev,
			Contracts:        &contracts,
			UnrealizedPnl:    &upnl,
			MarginMode:       &marginMode,
		}}, nil
	}
	defer injectMockFuturesProvider(mp)()

	cfg := config.DefaultConfig()
	tool := NewFuturesRiskSummaryTool(cfg)
	res := tool.Execute(context.Background(), map[string]any{
		"provider": "binance",
		"symbol":   "BTC/USDT:USDT",
	})
	if res.IsError {
		t.Errorf("unexpected error: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "BTC/USDT:USDT") {
		t.Errorf("expected symbol in output, got: %s", res.ForLLM)
	}
}

// --- FuturesEstimateFundingFeeTool ---

func TestFuturesEstimateFundingFee_SingleSymbol(t *testing.T) {
	rate := 0.0001
	nextTs := float64(9999999999000)

	mp := &mockFuturesProvider{
		fundingRateFn: func(_ context.Context, _ string) (ccxt.FundingRate, error) {
			return ccxt.FundingRate{FundingRate: &rate, NextFundingTimestamp: &nextTs}, nil
		},
	}
	mp.fetchFuturesPositionsFn = func(_ context.Context, _ []string) ([]ccxt.Position, error) {
		contracts := 0.1
		mark := 50000.0
		sym := "BTC/USDT:USDT"
		side := "long"
		return []ccxt.Position{{
			Symbol:    &sym,
			Side:      &side,
			Contracts: &contracts,
			MarkPrice: &mark,
		}}, nil
	}
	defer injectMockFuturesProvider(mp)()

	cfg := config.DefaultConfig()
	tool := NewFuturesEstimateFundingFeeTool(cfg)
	res := tool.Execute(context.Background(), map[string]any{
		"provider": "binance",
		"symbol":   "BTC/USDT:USDT",
	})
	if res.IsError {
		t.Errorf("unexpected error: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "BTC/USDT:USDT") {
		t.Errorf("expected symbol in output, got: %s", res.ForLLM)
	}
}

func TestFuturesEstimateFundingFee_AllPositions(t *testing.T) {
	rate := 0.0001
	contracts := 0.5
	mark := 2000.0
	sym := "ETH/USDT:USDT"
	side := "short"

	mp := &mockFuturesProvider{
		fundingRateFn: func(_ context.Context, _ string) (ccxt.FundingRate, error) {
			return ccxt.FundingRate{FundingRate: &rate}, nil
		},
	}
	mp.fetchFuturesPositionsFn = func(_ context.Context, _ []string) ([]ccxt.Position, error) {
		return []ccxt.Position{{
			Symbol:    &sym,
			Side:      &side,
			Contracts: &contracts,
			MarkPrice: &mark,
		}}, nil
	}
	defer injectMockFuturesProvider(mp)()

	cfg := config.DefaultConfig()
	tool := NewFuturesEstimateFundingFeeTool(cfg)
	// No symbol — should estimate for all positions
	res := tool.Execute(context.Background(), map[string]any{
		"provider": "binance",
	})
	if res.IsError {
		t.Errorf("unexpected error: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "ETH/USDT:USDT") {
		t.Errorf("expected ETH symbol in output, got: %s", res.ForLLM)
	}
}

// --- FuturesClosePositionTool with mock ---

func TestFuturesClosePosition_DryRun_WithMock(t *testing.T) {
	contracts := 0.5
	sym := "BTC/USDT:USDT"
	side := "long"

	mp := &mockFuturesProvider{
		fetchFuturesPositionsFn: func(_ context.Context, _ []string) ([]ccxt.Position, error) {
			return []ccxt.Position{{
				Symbol:    &sym,
				Side:      &side,
				Contracts: &contracts,
			}}, nil
		},
	}
	defer injectMockFuturesProvider(mp)()

	cfg := config.DefaultConfig()
	cfg.TradingRisk.AllowLeverage = true
	tool := NewFuturesClosePositionTool(cfg)
	res := tool.Execute(context.Background(), map[string]any{
		"provider": "binance",
		"symbol":   "BTC/USDT:USDT",
		"confirm":  false,
	})
	if res.IsError {
		t.Errorf("unexpected error: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "Dry-run") {
		t.Errorf("expected 'Dry-run' in output, got: %s", res.ForLLM)
	}
}

func TestFuturesClosePosition_NoPosition(t *testing.T) {
	mp := &mockFuturesProvider{
		fetchFuturesPositionsFn: func(_ context.Context, _ []string) ([]ccxt.Position, error) {
			return []ccxt.Position{}, nil
		},
	}
	defer injectMockFuturesProvider(mp)()

	cfg := config.DefaultConfig()
	cfg.TradingRisk.AllowLeverage = true
	tool := NewFuturesClosePositionTool(cfg)
	res := tool.Execute(context.Background(), map[string]any{
		"provider": "binance",
		"symbol":   "BTC/USDT:USDT",
		"confirm":  false,
	})
	if !res.IsError {
		t.Error("expected error for no active position")
	}
}

// --- FuturesCancelOrdersTool ---

func TestFuturesCancelOrders_DryRun_WithOrderID(t *testing.T) {
	mp := &mockFuturesProvider{}
	defer injectMockFuturesProvider(mp)()

	cfg := config.DefaultConfig()
	cfg.TradingRisk.AllowLeverage = true
	tool := NewFuturesCancelOrdersTool(cfg)
	res := tool.Execute(context.Background(), map[string]any{
		"provider": "binance",
		"symbol":   "BTC/USDT:USDT",
		"order_id": "ORD123",
		"confirm":  false,
	})
	if res.IsError {
		t.Errorf("unexpected error: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "Dry-run") {
		t.Errorf("expected 'Dry-run' in output, got: %s", res.ForLLM)
	}
}

func TestFuturesCancelOrders_DryRun_ByType(t *testing.T) {
	mp := &mockFuturesProvider{}
	defer injectMockFuturesProvider(mp)()

	cfg := config.DefaultConfig()
	cfg.TradingRisk.AllowLeverage = true
	tool := NewFuturesCancelOrdersTool(cfg)
	res := tool.Execute(context.Background(), map[string]any{
		"provider": "binance",
		"symbol":   "BTC/USDT:USDT",
		"type":     "protection",
		"confirm":  false,
	})
	if res.IsError {
		t.Errorf("unexpected error: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "Dry-run") {
		t.Errorf("expected 'Dry-run' in output, got: %s", res.ForLLM)
	}
}

// --- FuturesModifyProtectionTool dry-run with mock ---

func TestFuturesModifyProtection_DryRun_WithMock(t *testing.T) {
	contracts := 0.1
	sym := "BTC/USDT:USDT"
	side := "long"

	mp := &mockFuturesProvider{
		fetchFuturesPositionsFn: func(_ context.Context, _ []string) ([]ccxt.Position, error) {
			return []ccxt.Position{{
				Symbol:    &sym,
				Side:      &side,
				Contracts: &contracts,
			}}, nil
		},
	}
	defer injectMockFuturesProvider(mp)()

	cfg := config.DefaultConfig()
	cfg.TradingRisk.AllowLeverage = true
	tool := NewFuturesModifyProtectionTool(cfg)
	res := tool.Execute(context.Background(), map[string]any{
		"provider":    "binance",
		"symbol":      "BTC/USDT:USDT",
		"stop_loss":   40000.0,
		"take_profit": 60000.0,
		"confirm":     false,
	})
	if res.IsError {
		t.Errorf("unexpected error: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "Dry-run") {
		t.Errorf("expected 'Dry-run' in output, got: %s", res.ForLLM)
	}
}

// --- FuturesOpenPositionTool dry-run with mock ---

func TestFuturesOpenPosition_DryRun_WithMock(t *testing.T) {
	mp := &mockFuturesProvider{
		markPriceFn: func(_ context.Context, _ string) (float64, error) {
			return 50000.0, nil
		},
		loadMarketsFn: func(_ context.Context) (map[string]ccxt.MarketInterface, error) {
			active := true
			swap := true
			minAmt := 0.001
			maxLev := 100.0
			return map[string]ccxt.MarketInterface{
				"BTC/USDT:USDT": {
					Active: &active,
					Swap:   &swap,
					Limits: ccxt.Limits{
						Amount:   ccxt.MinMax{Min: &minAmt},
						Leverage: ccxt.MinMax{Max: &maxLev},
					},
				},
			}, nil
		},
	}
	defer injectMockFuturesProvider(mp)()

	cfg := config.DefaultConfig()
	cfg.TradingRisk.AllowLeverage = true
	tool := NewFuturesOpenPositionTool(cfg)
	res := tool.Execute(context.Background(), map[string]any{
		"provider": "binance",
		"symbol":   "BTC/USDT:USDT",
		"side":     "long",
		"amount":   0.01,
		"leverage": 5.0,
		"confirm":  false,
	})
	if res.IsError {
		// Accept provider errors but not missing-arg errors
		if strings.Contains(res.ForLLM, "required") && !strings.Contains(res.ForLLM, "provider") {
			t.Errorf("unexpected required-arg error: %s", res.ForLLM)
		}
	}
}

// --- FuturesSetLeverageTool ---

func TestFuturesSetLeverage_DryRunContainsLeverage(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.TradingRisk.AllowLeverage = true
	tool := NewFuturesSetLeverageTool(cfg)
	res := tool.Execute(context.Background(), map[string]any{
		"provider": "binance", "symbol": "BTC/USDT:USDT", "leverage": 10.0, "confirm": false,
	})
	if res.IsError {
		t.Fatalf("dry-run returned error: %s", res.ForUser)
	}
	if !strings.Contains(res.ForUser, "10x") {
		t.Errorf("expected leverage in dry-run output, got: %s", res.ForUser)
	}
}

func TestFuturesSetLeverage_MissingArgs(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.TradingRisk.AllowLeverage = true
	tool := NewFuturesSetLeverageTool(cfg)
	res := tool.Execute(context.Background(), map[string]any{})
	if !res.IsError {
		t.Error("expected error with missing args")
	}
}

func TestFuturesSetLeverage_TooHighLeverage(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.TradingRisk.AllowLeverage = true
	tool := NewFuturesSetLeverageTool(cfg)
	res := tool.Execute(context.Background(), map[string]any{
		"provider": "binance", "symbol": "BTC/USDT:USDT", "leverage": 200.0, "confirm": false,
	})
	if !res.IsError {
		t.Error("expected error for leverage > 125")
	}
}

// --- FuturesGetOrderTool ---

func TestFuturesGetOrder_WithMock(t *testing.T) {
	status := "closed"
	filled := 0.01
	side := "buy"
	price := 50000.0
	mp := &mockFuturesProvider{
		fetchFuturesOrderFn: func(_ context.Context, id, _ string) (ccxt.Order, error) {
			return ccxt.Order{
				Id:     &id,
				Status: &status,
				Filled: &filled,
				Side:   &side,
				Price:  &price,
			}, nil
		},
	}
	defer injectMockFuturesProvider(mp)()

	tool := NewFuturesGetOrderTool(config.DefaultConfig())
	res := tool.Execute(context.Background(), map[string]any{
		"provider": "binance", "symbol": "BTC/USDT:USDT", "order_id": "ord-123",
	})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForUser)
	}
}

// --- FuturesGetPositionsTool ---

func TestFuturesGetPositions_WithMock(t *testing.T) {
	sym := "BTC/USDT:USDT"
	contracts := 0.01
	side := "long"
	mp := &mockFuturesProvider{
		fetchFuturesPositionsFn: func(_ context.Context, _ []string) ([]ccxt.Position, error) {
			return []ccxt.Position{{Symbol: &sym, Contracts: &contracts, Side: &side}}, nil
		},
	}
	defer injectMockFuturesProvider(mp)()

	tool := NewFuturesGetPositionsTool(config.DefaultConfig())
	res := tool.Execute(context.Background(), map[string]any{
		"provider": "binance", "symbol": "BTC/USDT:USDT",
	})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForUser)
	}
	if !strings.Contains(res.ForUser, "BTC") {
		t.Errorf("expected BTC in output, got: %s", res.ForUser)
	}
}

func TestFuturesGetPositions_NoPositions(t *testing.T) {
	mp := &mockFuturesProvider{
		fetchFuturesPositionsFn: func(_ context.Context, _ []string) ([]ccxt.Position, error) {
			return nil, nil
		},
	}
	defer injectMockFuturesProvider(mp)()

	tool := NewFuturesGetPositionsTool(config.DefaultConfig())
	res := tool.Execute(context.Background(), map[string]any{"provider": "binance"})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForUser)
	}
}

// --- FuturesGetFundingTool ---

func TestFuturesGetFunding_WithMock(t *testing.T) {
	rate := 0.0001
	sym := "BTC/USDT:USDT"
	mp := &mockFuturesProvider{
		fundingRateFn: func(_ context.Context, _ string) (ccxt.FundingRate, error) {
			return ccxt.FundingRate{FundingRate: &rate, Symbol: &sym}, nil
		},
	}
	defer injectMockFuturesProvider(mp)()

	tool := NewFuturesGetFundingTool(config.DefaultConfig())
	res := tool.Execute(context.Background(), map[string]any{
		"provider": "binance", "symbol": "BTC/USDT:USDT",
	})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForUser)
	}
}

func TestFuturesGetFunding_WithHistory(t *testing.T) {
	rate := 0.0001
	sym := "BTC/USDT:USDT"
	ts := int64(1700000000000)
	amount := 1.5
	mp := &mockFuturesProvider{
		fundingRateFn: func(_ context.Context, _ string) (ccxt.FundingRate, error) {
			return ccxt.FundingRate{FundingRate: &rate, Symbol: &sym}, nil
		},
		fetchFundingHistoryFn: func(_ context.Context, _ string, _ *int64, _ int) ([]ccxt.FundingHistory, error) {
			return []ccxt.FundingHistory{{Timestamp: &ts, Amount: &amount}}, nil
		},
	}
	defer injectMockFuturesProvider(mp)()

	tool := NewFuturesGetFundingTool(config.DefaultConfig())
	res := tool.Execute(context.Background(), map[string]any{
		"provider": "binance", "symbol": "BTC/USDT:USDT", "include_history": true,
	})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForUser)
	}
}

// --- FuturesEmergencyFlattenTool dry-run paths ---

func TestFuturesEmergencyFlatten_DryRunNoPositions(t *testing.T) {
	mp := &mockFuturesProvider{
		fetchFuturesPositionsFn: func(_ context.Context, _ []string) ([]ccxt.Position, error) {
			return nil, nil
		},
	}
	defer injectMockFuturesProvider(mp)()

	cfg := config.DefaultConfig()
	cfg.TradingRisk.AllowLeverage = true
	tool := NewFuturesEmergencyFlattenTool(cfg)
	res := tool.Execute(context.Background(), map[string]any{
		"provider": "binance", "confirm": false,
	})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForUser)
	}
	if !strings.Contains(res.ForUser, "no active") {
		t.Errorf("expected 'no active' in dry-run output, got: %s", res.ForUser)
	}
}

func TestFuturesEmergencyFlatten_DryRunWithPositions(t *testing.T) {
	sym := "BTC/USDT:USDT"
	contracts := 0.01
	side := "long"
	mp := &mockFuturesProvider{
		fetchFuturesPositionsFn: func(_ context.Context, _ []string) ([]ccxt.Position, error) {
			return []ccxt.Position{{Symbol: &sym, Contracts: &contracts, Side: &side}}, nil
		},
	}
	defer injectMockFuturesProvider(mp)()

	cfg := config.DefaultConfig()
	cfg.TradingRisk.AllowLeverage = true
	tool := NewFuturesEmergencyFlattenTool(cfg)
	res := tool.Execute(context.Background(), map[string]any{
		"provider": "binance", "confirm": false,
	})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForUser)
	}
	if !strings.Contains(res.ForUser, "flatten") {
		t.Errorf("expected 'flatten' in dry-run output, got: %s", res.ForUser)
	}
}

// --- FuturesReducePositionTool validation paths ---

func TestFuturesReducePosition_BothAmountAndPercent(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.TradingRisk.AllowLeverage = true
	tool := NewFuturesReducePositionTool(cfg)
	res := tool.Execute(context.Background(), map[string]any{
		"provider": "binance", "symbol": "BTC/USDT:USDT",
		"amount": 0.01, "percent": 50.0, "confirm": false,
	})
	if !res.IsError {
		t.Error("expected error when both amount and percent provided")
	}
}

func TestFuturesReducePosition_InvalidPercent(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.TradingRisk.AllowLeverage = true
	tool := NewFuturesReducePositionTool(cfg)
	res := tool.Execute(context.Background(), map[string]any{
		"provider": "binance", "symbol": "BTC/USDT:USDT",
		"percent": 150.0, "confirm": false,
	})
	if !res.IsError {
		t.Error("expected error for percent > 100")
	}
}

func TestFuturesReducePosition_LimitWithoutPrice(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.TradingRisk.AllowLeverage = true
	tool := NewFuturesReducePositionTool(cfg)
	res := tool.Execute(context.Background(), map[string]any{
		"provider": "binance", "symbol": "BTC/USDT:USDT",
		"amount": 0.01, "order_type": "limit", "confirm": false,
	})
	if !res.IsError {
		t.Error("expected error for limit without limit_price")
	}
}

// --- marginHealthFromPosition pure-function coverage ---

func TestMarginHealthFromPosition_SafeByDistance(t *testing.T) {
	mark := 50000.0
	liq := 30000.0 // dist = 40% → safe
	_, _, label := marginHealthFromPosition(ccxt.Position{MarkPrice: &mark, LiquidationPrice: &liq})
	if label != "safe" {
		t.Errorf("expected safe, got %q", label)
	}
}

func TestMarginHealthFromPosition_WarnByDistance(t *testing.T) {
	mark := 50000.0
	liq := 47000.0 // dist = 6% → warn
	_, _, label := marginHealthFromPosition(ccxt.Position{MarkPrice: &mark, LiquidationPrice: &liq})
	if label != "warn" {
		t.Errorf("expected warn, got %q", label)
	}
}

func TestMarginHealthFromPosition_CriticalByDistance(t *testing.T) {
	mark := 50000.0
	liq := 48500.0 // dist = 3% → critical
	_, _, label := marginHealthFromPosition(ccxt.Position{MarkPrice: &mark, LiquidationPrice: &liq})
	if label != "critical" {
		t.Errorf("expected critical, got %q", label)
	}
}

func TestMarginHealthFromPosition_SafeByMarginRatio(t *testing.T) {
	ratio := 0.3 // 30% → safe
	_, _, label := marginHealthFromPosition(ccxt.Position{MarginRatio: &ratio})
	if label != "safe" {
		t.Errorf("expected safe by margin ratio, got %q", label)
	}
}

func TestMarginHealthFromPosition_CriticalByMarginRatio(t *testing.T) {
	ratio := 0.9 // 90% → critical
	_, _, label := marginHealthFromPosition(ccxt.Position{MarginRatio: &ratio})
	if label != "critical" {
		t.Errorf("expected critical by margin ratio, got %q", label)
	}
}

func TestMarginHealthFromPosition_BothDistanceAndMarginRatio(t *testing.T) {
	mark := 50000.0
	liq := 30000.0 // dist 40% → safe; marginRatio branch is the "else if" path
	ratio := 0.4
	_, mr, label := marginHealthFromPosition(ccxt.Position{MarkPrice: &mark, LiquidationPrice: &liq, MarginRatio: &ratio})
	if label != "safe" {
		t.Errorf("expected safe, got %q", label)
	}
	if mr == 0 {
		t.Error("expected marginRatioPct populated via else-if branch")
	}
}

func TestMarginHealthFromPosition_Unknown(t *testing.T) {
	_, _, label := marginHealthFromPosition(ccxt.Position{})
	if label != "unknown" {
		t.Errorf("expected unknown, got %q", label)
	}
}

// --- contractsFromNotional pure-function coverage ---

func TestContractsFromNotional_ZeroContractSize(t *testing.T) {
	// contractSize <= 0 triggers fallback to 1
	got, err := contractsFromNotional(100.0, 50000.0, 0, 0.001)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got <= 0 {
		t.Errorf("expected positive contracts, got %v", got)
	}
}

func TestContractsFromNotional_ZeroMinAmount(t *testing.T) {
	// minAmount <= 0 triggers fallback to 1
	got, err := contractsFromNotional(100.0, 50000.0, 0.001, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got <= 0 {
		t.Errorf("expected positive contracts, got %v", got)
	}
}

// --- FuturesReducePositionTool dry-run ---

func TestFuturesReducePosition_DryRunWithMock(t *testing.T) {
	sym := "BTC/USDT:USDT"
	contracts := 0.1
	side := "long"
	mp := &mockFuturesProvider{
		fetchFuturesPositionsFn: func(_ context.Context, _ []string) ([]ccxt.Position, error) {
			return []ccxt.Position{{Symbol: &sym, Contracts: &contracts, Side: &side}}, nil
		},
	}
	defer injectMockFuturesProvider(mp)()

	cfg := config.DefaultConfig()
	cfg.TradingRisk.AllowLeverage = true
	tool := NewFuturesReducePositionTool(cfg)
	res := tool.Execute(context.Background(), map[string]any{
		"provider": "binance", "symbol": "BTC/USDT:USDT",
		"percent": 50.0, "confirm": false,
	})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForUser)
	}
	if !strings.Contains(res.ForUser, "reduce") && !strings.Contains(res.ForUser, "Dry") {
		t.Errorf("expected dry-run output, got: %s", res.ForUser)
	}
}

// --- formatFundingRate pure-function coverage ---

func TestFormatFundingRate_AllFields(t *testing.T) {
	rate := 0.0001
	nextRate := 0.00012
	mark := 50000.0
	index := 49990.0
	ts := float64(1700000000000)
	interval := "8h"
	sym := "BTC/USDT:USDT"
	r := ccxt.FundingRate{
		FundingRate:      &rate,
		NextFundingRate:  &nextRate,
		MarkPrice:        &mark,
		IndexPrice:       &index,
		FundingTimestamp: &ts,
		Interval:         &interval,
		Symbol:           &sym,
	}
	out := formatFundingRate("binance", "BTC/USDT:USDT", r)
	if !strings.Contains(out, "0.0001") {
		t.Errorf("expected rate in output: %s", out)
	}
	if !strings.Contains(out, "8h") {
		t.Errorf("expected interval in output: %s", out)
	}
}

// --- formatFuturesPositions pure-function coverage ---

func TestFormatFuturesPositions_WithAllOptionalFields(t *testing.T) {
	sym := "BTC/USDT:USDT"
	contracts := 0.01
	side := "long"
	realized := 5.0
	liq := 45000.0
	mode := "isolated"
	p := ccxt.Position{
		Symbol:           &sym,
		Contracts:        &contracts,
		Side:             &side,
		RealizedPnl:      &realized,
		LiquidationPrice: &liq,
		MarginMode:       &mode,
	}
	out := formatFuturesPositions("binance", []ccxt.Position{p})
	if !strings.Contains(out, "isolated") {
		t.Errorf("expected 'isolated' in output: %s", out)
	}
	if !strings.Contains(out, "45000") {
		t.Errorf("expected liquidation price in output: %s", out)
	}
}

// --- formatFuturesOrder pure-function coverage ---

func TestFormatFuturesOrder_AllFields(t *testing.T) {
	sym := "BTC/USDT:USDT"
	typ := "market"
	side := "buy"
	amt := 0.01
	price := 50000.0
	filled := 0.01
	remaining := 0.0
	status := "closed"
	dt := "2024-01-01T00:00:00Z"
	o := ccxt.Order{
		Symbol:    &sym,
		Type:      &typ,
		Side:      &side,
		Amount:    &amt,
		Price:     &price,
		Filled:    &filled,
		Remaining: &remaining,
		Status:    &status,
		Datetime:  &dt,
	}
	out := formatFuturesOrder("binance", "ord-001", o)
	if !strings.Contains(out, "BTC/USDT:USDT") {
		t.Errorf("expected symbol in output: %s", out)
	}
	if !strings.Contains(out, "2024-01-01") {
		t.Errorf("expected datetime in output: %s", out)
	}
}
