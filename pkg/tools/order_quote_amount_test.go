package tools

import (
	"context"
	"math"
	"strings"
	"sync"
	"testing"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// qaVenue is a pricedVenue with a market catalogue, which records what reaches
// CreateOrder.
type qaVenue struct {
	*pricedVenue
	markets map[string]ccxt.MarketInterface

	omu    sync.Mutex
	amount []float64
}

func newQAVenue(t *testing.T, id string) *qaVenue {
	t.Helper()
	v := &qaVenue{pricedVenue: newPricedVenue(t, id), markets: map[string]ccxt.MarketInterface{}}
	// Replaces the factory newPricedVenue registered; its cleanup still unregisters.
	broker.RegisterFactory(id, func(*config.Config) (broker.Provider, error) { return v, nil })
	return v
}

func (v *qaVenue) LoadMarkets(context.Context) (map[string]ccxt.MarketInterface, error) {
	return v.markets, nil
}

func (v *qaVenue) CreateOrder(ctx context.Context, symbol, typ, side string, amount float64, price *float64, params map[string]interface{}) (ccxt.Order, error) {
	v.omu.Lock()
	v.amount = append(v.amount, amount)
	v.omu.Unlock()
	return v.pricedVenue.CreateOrder(ctx, symbol, typ, side, amount, price, params)
}

func (v *qaVenue) placed() []float64 {
	v.omu.Lock()
	defer v.omu.Unlock()
	return append([]float64(nil), v.amount...)
}

// setMarket scripts a catalogue entry; a zero step or minimum is left unset.
func (v *qaVenue) setMarket(symbol string, step, minAmount, minCost float64) {
	m := ccxt.MarketInterface{Symbol: &symbol}
	if step > 0 {
		m.Precision.Amount = &step
	}
	if minAmount > 0 {
		m.Limits.Amount.Min = &minAmount
	}
	if minCost > 0 {
		m.Limits.Cost.Min = &minCost
	}
	v.markets[symbol] = m
}

func quoteOrder(provider, symbol, orderType, side string, quote float64, price *float64, confirm bool) map[string]any {
	args := map[string]any{
		"provider": provider, "symbol": symbol, "type": orderType, "side": side,
		"quote_amount": quote, "confirm": confirm,
	}
	if price != nil {
		args["price"] = *price
	}
	return args
}

func btcUSDTVenue(t *testing.T, id string, step float64) *qaVenue {
	t.Helper()
	v := newQAVenue(t, id)
	v.setTicker("BTC/USDT", 98000, 97900, 97950)
	v.setMarket("BTC/USDT", step, 0, 0)
	return v
}

func TestQuoteAmount_MarketBuy_UsesAskAndRoundsDown(t *testing.T) {
	v := btcUSDTVenue(t, "qa-buy", 0.000001)
	res := NewCreateOrderTool(capConfig(0)).Execute(context.Background(), quoteOrder(v.id, "BTC/USDT", "market", "buy", 20, nil, true))
	if res.IsError {
		t.Fatalf("refused: %s", res.ForLLM)
	}
	// 20 / 98000 = 0.00020408163...: rounded DOWN to the 1e-6 step, never up to ...205.
	if got := v.placed(); len(got) != 1 || got[0] != 0.000204 {
		t.Fatalf("placed amounts = %v, want [0.000204]", got)
	}
	if !strings.Contains(res.ForLLM, "quote 20 USDT ≈ 0.000204 BTC at ask 98000") {
		t.Errorf("result lacks the sizing line: %s", res.ForLLM)
	}
}

func TestQuoteAmount_Sell_UsesBid(t *testing.T) {
	v := btcUSDTVenue(t, "qa-sell", 0.0000001)
	res := NewCreateOrderTool(capConfig(0)).Execute(context.Background(), quoteOrder(v.id, "BTC/USDT", "market", "sell", 20, nil, true))
	if res.IsError {
		t.Fatalf("refused: %s", res.ForLLM)
	}
	// 20 / 97900 = 0.000204290...; at the ask (98000) it would be 0.0002040.
	if got := v.placed(); len(got) != 1 || got[0] != 0.0002042 {
		t.Fatalf("placed amounts = %v, want [0.0002042]", got)
	}
	if !strings.Contains(res.ForLLM, "at bid 97900") {
		t.Errorf("result does not name the bid: %s", res.ForLLM)
	}
}

func TestQuoteAmount_LastIsTheFallbackEdge(t *testing.T) {
	v := newQAVenue(t, "qa-last")
	v.setTicker("BTC/USDT", 0, 0, 100000) // only a last, as Binance TH reports
	v.setMarket("BTC/USDT", 0.00001, 0, 0)
	res := NewCreateOrderTool(capConfig(0)).Execute(context.Background(), quoteOrder(v.id, "BTC/USDT", "market", "buy", 25, nil, true))
	if res.IsError || !strings.Contains(res.ForLLM, "at last 100000") {
		t.Fatalf("a ticker with only a last must size at the last: %+v", res)
	}
	if got := v.placed(); len(got) != 1 || got[0] != 0.00025 {
		t.Fatalf("placed amounts = %v, want [0.00025]", got)
	}
}

func TestQuoteAmount_LimitOrder_UsesTheLimitPrice(t *testing.T) {
	v := btcUSDTVenue(t, "qa-limit", 0.000001)
	px := 90000.0
	res := NewCreateOrderTool(capConfig(0)).Execute(context.Background(), quoteOrder(v.id, "BTC/USDT", "limit", "buy", 20, &px, true))
	if res.IsError {
		t.Fatalf("refused: %s", res.ForLLM)
	}
	// 20 / 90000 = 0.000222222...; the ticker (98000) must not be used.
	if got := v.placed(); len(got) != 1 || got[0] != 0.000222 {
		t.Fatalf("placed amounts = %v, want [0.000222]", got)
	}
	if !strings.Contains(res.ForLLM, "at limit price 90000") {
		t.Errorf("result does not name the limit price: %s", res.ForLLM)
	}
	if n := len(v.fetched()); n != 0 {
		t.Errorf("a limit order sized by its own price fetched %d tickers before sizing", n)
	}
}

func TestQuoteAmount_BelowMinimums_Refused(t *testing.T) {
	v := newQAVenue(t, "qa-min")
	v.setTicker("BTC/USDT", 98000, 97900, 97950)

	// Minimum amount: 20 USDT is 0.000204 BTC, under 0.001.
	v.setMarket("BTC/USDT", 0.000001, 0.001, 0)
	res := NewCreateOrderTool(capConfig(0)).Execute(context.Background(), quoteOrder(v.id, "BTC/USDT", "market", "buy", 20, nil, true))
	if !res.IsError || !strings.Contains(res.ForLLM, "minimum order amount") {
		t.Fatalf("below the minimum amount was not refused: %+v", res)
	}

	// Minimum cost: 5 USDT against a 10 USDT minimum, with an amount that clears its own minimum.
	v.setMarket("BTC/USDT", 0.000001, 0.00001, 10)
	res = NewCreateOrderTool(capConfig(0)).Execute(context.Background(), quoteOrder(v.id, "BTC/USDT", "market", "buy", 5, nil, true))
	if !res.IsError || !strings.Contains(res.ForLLM, "minimum order cost") {
		t.Fatalf("below the minimum cost was not refused: %+v", res)
	}

	// Less than one step.
	v.setMarket("BTC/USDT", 0.001, 0, 0)
	res = NewCreateOrderTool(capConfig(0)).Execute(context.Background(), quoteOrder(v.id, "BTC/USDT", "market", "buy", 20, nil, true))
	if !res.IsError || !strings.Contains(res.ForLLM, "less than one tradable step") {
		t.Fatalf("a size that rounds to zero was not refused: %+v", res)
	}
	if len(v.placed()) != 0 {
		t.Fatalf("an order was placed: %v", v.placed())
	}

	// At the minimum exactly: allowed.
	v.setMarket("BTC/USDT", 0.000001, 0.000204, 19)
	res = NewCreateOrderTool(capConfig(0)).Execute(context.Background(), quoteOrder(v.id, "BTC/USDT", "market", "buy", 20, nil, true))
	if res.IsError {
		t.Fatalf("an order at the minimum was refused: %s", res.ForLLM)
	}
}

func TestQuoteAmount_NoPrice_Refused(t *testing.T) {
	v := newQAVenue(t, "qa-noprice")
	v.setMarket("BTC/USDT", 0.000001, 0, 0) // listed, but the ticker fetch fails
	res := NewCreateOrderTool(capConfig(0)).Execute(context.Background(), quoteOrder(v.id, "BTC/USDT", "market", "buy", 20, nil, true))
	if !res.IsError || !strings.Contains(res.ForLLM, "cannot size the order: no price for BTC/USDT") {
		t.Fatalf("an unpriceable market order was not refused: %+v", res)
	}

	// A ticker with no usable field at all.
	v.setTicker("BTC/USDT", 0, 0, 0)
	res = NewCreateOrderTool(capConfig(0)).Execute(context.Background(), quoteOrder(v.id, "BTC/USDT", "market", "buy", 20, nil, true))
	if !res.IsError || !strings.Contains(res.ForLLM, "cannot size the order: no price for") {
		t.Fatalf("a ticker with no price was not refused: %+v", res)
	}

	// Priced but not in the catalogue: the step and minimums are unknown, so refuse.
	v.setTicker("ETH/USDT", 3000, 2999, 2999.5)
	res = NewCreateOrderTool(capConfig(0)).Execute(context.Background(), quoteOrder(v.id, "ETH/USDT", "market", "buy", 20, nil, true))
	if !res.IsError || !strings.Contains(res.ForLLM, "not in the market catalogue") {
		t.Fatalf("a symbol missing from the catalogue was not refused: %+v", res)
	}
	if len(v.placed()) != 0 {
		t.Fatalf("an order was placed: %v", v.placed())
	}
}

func TestQuoteAmount_ExactlyOneOfAmountAndQuoteAmount(t *testing.T) {
	v := btcUSDTVenue(t, "qa-xor", 0.000001)
	tool := NewCreateOrderTool(capConfig(0))

	both := quoteOrder(v.id, "BTC/USDT", "market", "buy", 20, nil, true)
	both["amount"] = 0.001
	if res := tool.Execute(context.Background(), both); !res.IsError || !strings.Contains(res.ForLLM, "not both") {
		t.Errorf("both amount and quote_amount were accepted: %+v", res)
	}

	neither := map[string]any{"provider": v.id, "symbol": "BTC/USDT", "type": "market", "side": "buy", "confirm": true}
	if res := tool.Execute(context.Background(), neither); !res.IsError || !strings.Contains(res.ForLLM, "is required") {
		t.Errorf("neither amount nor quote_amount was accepted: %+v", res)
	}
	if len(v.placed()) != 0 {
		t.Fatalf("an order was placed: %v", v.placed())
	}

	// amount alone is unchanged.
	if res := placeOrder(capConfig(0), v.id, "BTC/USDT", "market", "buy", 0.001, nil, true); res.IsError {
		t.Errorf("a plain amount order was refused: %s", res.ForLLM)
	}
}

func TestQuoteAmount_RejectsNonFiniteAndNonPositive(t *testing.T) {
	v := btcUSDTVenue(t, "qa-bad", 0.000001)
	tool := NewCreateOrderTool(capConfig(0))
	for name, q := range map[string]any{
		"NaN": math.NaN(), "+Inf": math.Inf(1), "-Inf": math.Inf(-1), "zero": 0.0, "negative": -5.0, "string": "20",
	} {
		args := quoteOrder(v.id, "BTC/USDT", "market", "buy", 1, nil, true)
		args["quote_amount"] = q
		res := tool.Execute(context.Background(), args)
		if !res.IsError || !strings.Contains(res.ForLLM, "quote_amount must be a positive, finite number") {
			t.Errorf("%s: not refused as expected: %+v", name, res)
		}
	}
	if len(v.placed()) != 0 {
		t.Fatalf("an order was placed: %v", v.placed())
	}
}

func TestQuoteAmount_ContractSymbol_Refused(t *testing.T) {
	v := btcUSDTVenue(t, "qa-contract", 0.000001)
	res := NewCreateOrderTool(capConfig(0)).Execute(context.Background(), quoteOrder(v.id, "BTC/USDT:USDT", "market", "buy", 20, nil, true))
	if !res.IsError || !strings.Contains(res.ForLLM, "spot orders only") || !strings.Contains(res.ForLLM, "notional_usd") {
		t.Fatalf("quote_amount on a contract symbol was not refused with the futures pointer: %+v", res)
	}
	if len(v.fetched()) != 0 || len(v.placed()) != 0 {
		t.Fatal("a contract symbol reached the venue")
	}
}

// The USD cap is enforced on the converted value, in USD, for a THB-quoted symbol too.
func TestQuoteAmount_USDCapIsEnforcedOnTheConvertedValue(t *testing.T) {
	v := newQAVenue(t, "qa-thb-cap")
	v.setTicker("USDT/THB", 0, 0, 36)
	v.setTicker("BTC/THB", 100_000, 99_900, 99_950)
	v.setMarket("BTC/THB", 0.000001, 0, 0)
	cfg := capConfig(100)
	tool := NewCreateOrderTool(cfg)

	// 5,000 THB is 0.05 BTC, about 139 USD: over the 100 USD cap.
	res := tool.Execute(context.Background(), quoteOrder(v.id, "BTC/THB", "market", "buy", 5000, nil, true))
	if !res.IsError || !strings.Contains(res.ForLLM, "max_order_value_usd") {
		t.Fatalf("5,000 THB by quote_amount was not refused on the USD cap: %+v", res)
	}
	if len(v.placed()) != 0 {
		t.Fatal("an order over the cap reached the venue")
	}

	// 3,000 THB is about 83 USD: placed, at 0.03 BTC.
	res = tool.Execute(context.Background(), quoteOrder(v.id, "BTC/THB", "market", "buy", 3000, nil, true))
	if res.IsError {
		t.Fatalf("3,000 THB was refused: %s", res.ForLLM)
	}
	if got := v.placed(); len(got) != 1 || got[0] != 0.03 {
		t.Fatalf("placed amounts = %v, want [0.03]", got)
	}

	// A USDT symbol: 600 USDT over the 100 USD cap, with a limit price (the early USD-like check).
	u := btcUSDTVenue(t, "qa-usdt-cap", 0.000001)
	px := 60000.0
	res = tool.Execute(context.Background(), quoteOrder(u.id, "BTC/USDT", "limit", "buy", 600, &px, true))
	if !res.IsError || !strings.Contains(res.ForLLM, "exceeds configured max_order_value_usd") {
		t.Fatalf("600 USDT by quote_amount was not refused on the USD cap: %+v", res)
	}
	if len(u.placed()) != 0 {
		t.Fatal("an order over the cap reached the venue")
	}
}

// With a cap set, an order whose USD value cannot be determined is refused, not placed unchecked.
func TestQuoteAmount_UnknownUSDWithCapSet_Refused(t *testing.T) {
	v := newQAVenue(t, "qa-norate")
	v.setTicker("BTC/THB", 100_000, 99_900, 99_950) // no USDT/THB rate anywhere
	v.setMarket("BTC/THB", 0.000001, 0, 0)
	res := NewCreateOrderTool(capConfig(100)).Execute(context.Background(), quoteOrder(v.id, "BTC/THB", "market", "buy", 1000, nil, true))
	if !res.IsError || !strings.Contains(res.ForLLM, "USD conversion rate") {
		t.Fatalf("an order with no USD value under a cap was not refused: %+v", res)
	}
	if len(v.placed()) != 0 {
		t.Fatal("an unmeasurable order reached the venue")
	}
}

// A tripped daily-loss limit stops a quote_amount buy exactly as it stops an amount buy.
func TestQuoteAmount_DailyLossStillBlocksABuy(t *testing.T) {
	tripLossTracker(t, 100)
	spot, fut := newDNFakes()
	installDNFakes(t, spot, fut)
	tool := NewCreateOrderTool(riskCfg(true, 100, 0))
	args := quoteOrder("dnfake", "BTC/USDT", "market", "buy", 20, nil, false)
	res := tool.Execute(context.Background(), args)
	if !res.IsError || !strings.Contains(res.ForLLM, "daily loss") {
		t.Fatalf("a quote_amount buy must be refused by a tripped limit: %+v", res)
	}
	noOrders(t, spot, fut)
}

// Paper mode simulates with the converted amount; nothing reaches the venue.
func TestQuoteAmount_PaperMode_SimulatesWithTheConvertedAmount(t *testing.T) {
	v := btcUSDTVenue(t, "qa-paper", 0.000001)
	cfg := capConfig(0)
	cfg.TradingRisk.PaperTradingMode = true
	res := NewCreateOrderTool(cfg).Execute(context.Background(), quoteOrder(v.id, "BTC/USDT", "market", "buy", 20, nil, true))
	if res.IsError || !strings.Contains(res.ForLLM, "[PAPER]") {
		t.Fatalf("paper mode must simulate the order: %+v", res)
	}
	if !strings.Contains(res.ForLLM, "Amount: 0.000204") || !strings.Contains(res.ForLLM, "quote 20 USDT ≈ 0.000204 BTC at ask 98000") {
		t.Errorf("the simulation did not use the converted amount: %s", res.ForLLM)
	}
	if got := v.placed(); len(got) != 0 {
		t.Fatalf("paper mode reached the venue: %v", got)
	}
}

func TestQuoteAmount_DryRunShowsQuoteBaseAndPrice(t *testing.T) {
	v := btcUSDTVenue(t, "qa-dry", 0.000001)
	res := NewCreateOrderTool(capConfig(0)).Execute(context.Background(), quoteOrder(v.id, "BTC/USDT", "market", "buy", 20, nil, false))
	if res.IsError {
		t.Fatalf("dry-run refused: %s", res.ForLLM)
	}
	for _, want := range []string{"Dry-run", "x0.000204", "quote 20 USDT ≈ 0.000204 BTC at ask 98000"} {
		if !strings.Contains(res.ForLLM, want) {
			t.Errorf("dry-run lacks %q: %s", want, res.ForLLM)
		}
	}
	if len(v.placed()) != 0 {
		t.Fatal("a dry-run placed an order")
	}
}

func TestQuoteAmount_NoStepPublished_FloorsToEightDecimals(t *testing.T) {
	v := newQAVenue(t, "qa-nostep")
	v.setTicker("BTC/THB", 3_000_000, 2_999_000, 2_999_500)
	v.setMarket("BTC/THB", 0, 0, 0) // as the Bitkub and Binance TH catalogues are
	res := NewCreateOrderTool(capConfig(0)).Execute(context.Background(), quoteOrder(v.id, "BTC/THB", "market", "buy", 1000, nil, true))
	if res.IsError {
		t.Fatalf("refused: %s", res.ForLLM)
	}
	// 1000 / 3,000,000 = 0.000333333...
	if got := v.placed(); len(got) != 1 || got[0] != 0.00033333 {
		t.Fatalf("placed amounts = %v, want [0.00033333]", got)
	}
}

func TestFloorToStep(t *testing.T) {
	for _, c := range []struct{ x, step, want float64 }{
		{0.00020408163, 0.000001, 0.000204},
		{0.3, 0.1, 0.3}, // 0.3/0.1 = 2.9999999999999996 in binary
		{0.7, 0.1, 0.7},
		{1.999999, 1, 1},
		{12.7, 2.5, 12.5},
		{9.9, 2.5, 7.5},
		{0.00033333333, 1e-8, 0.00033333},
		{5, 10, 0},
	} {
		if got := floorToStep(c.x, c.step); got != c.want {
			t.Errorf("floorToStep(%v, %v) = %v, want %v", c.x, c.step, got, c.want)
		}
	}
}

func TestQuoteAmount_OrderParamsStillRefuseExchangeFlags(t *testing.T) {
	for _, key := range []string{"tgtCcy", "quoteOrderQty", "TGTCCY"} {
		_, err := sanitizeOrderParams(map[string]any{key: "quote_ccy"}, config.TradingRiskConfig{})
		if err == nil || !strings.Contains(err.Error(), "quote_amount") || !strings.Contains(err.Error(), key) {
			t.Errorf("%s: want a refusal naming quote_amount, got %v", key, err)
		}
	}
	// Still refused when margin and leverage are allowed: it is not a risk flag.
	if _, err := sanitizeOrderParams(map[string]any{"tgtCcy": "quote_ccy"}, config.TradingRiskConfig{AllowMargin: true, AllowLeverage: true}); err == nil {
		t.Error("tgtCcy passed with margin and leverage allowed")
	}
	// Through the tool itself, so the whole path refuses it before anything is sent.
	v := btcUSDTVenue(t, "qa-flag", 0.000001)
	args := quoteOrder(v.id, "BTC/USDT", "market", "buy", 20, nil, true)
	args["params"] = map[string]any{"tgtCcy": "quote_ccy"}
	res := NewCreateOrderTool(capConfig(0)).Execute(context.Background(), args)
	if !res.IsError || !strings.Contains(res.ForLLM, "quote_amount") || len(v.placed()) != 0 {
		t.Fatalf("tgtCcy through create_order: %+v", res)
	}
}

func TestQuoteAmount_SchemaAdvertisesIt(t *testing.T) {
	cfg := capConfig(0)
	for _, tool := range []interface{ Parameters() map[string]any }{NewCreateOrderTool(cfg), NewProposeOrderTool(cfg)} {
		props := tool.Parameters()["properties"].(map[string]any)
		if _, ok := props["quote_amount"]; !ok {
			t.Errorf("%T does not declare quote_amount", tool)
		}
		for _, req := range tool.Parameters()["required"].([]string) {
			if req == "amount" || req == "quote_amount" {
				t.Errorf("%T requires %s: exactly one of them is given", tool, req)
			}
		}
	}
	if d := NewCreateOrderTool(cfg).Description(); !strings.Contains(d, "quote_amount") {
		t.Errorf("create_order's description does not mention quote_amount: %s", d)
	}
	if d := NewProposeOrderTool(cfg).Description(); !strings.Contains(d, "quote_amount") {
		t.Errorf("propose_order's description does not mention quote_amount: %s", d)
	}
}

// ---- propose_order ----

func TestQuoteAmount_Propose_StoresTheConvertedBaseAmount(t *testing.T) {
	v := btcUSDTVenue(t, "qa-prop", 0.000001)
	got, err := NewApprovalOrderBackend(capConfig(0)).Prepare(context.Background(),
		approval.OrderRequest{Provider: v.id, Symbol: "BTC/USDT", Side: "buy", QuoteAmount: 20})
	if err != nil {
		t.Fatal(err)
	}
	p := got.Params
	if p.Amount != 0.000204 || p.QuoteAmount != 20 || p.Market != approval.MarketSpot {
		t.Fatalf("params = %+v, want amount 0.000204 and quote_amount 20", p)
	}
	if p.RefPrice != 98000 || math.Abs(p.NotionalUSD-0.000204*98000) > 1e-9 {
		t.Errorf("ref %v, notional %v: the gates must value the converted amount", p.RefPrice, p.NotionalUSD)
	}
	if len(v.placed()) != 0 {
		t.Fatal("a proposal placed an order")
	}
}

func TestQuoteAmount_Propose_Refusals(t *testing.T) {
	v := btcUSDTVenue(t, "qa-prop-bad", 0.000001)
	b := NewApprovalOrderBackend(capConfig(0))
	ctx := context.Background()
	base := approval.OrderRequest{Provider: v.id, Symbol: "BTC/USDT", Side: "buy"}

	both := base
	both.Amount, both.QuoteAmount = 0.001, 20
	if _, err := b.Prepare(ctx, both); err == nil || !strings.Contains(err.Error(), "not both") {
		t.Errorf("both: %v", err)
	}
	if _, err := b.Prepare(ctx, base); err == nil || !strings.Contains(err.Error(), "quote_amount") {
		t.Errorf("neither: %v", err)
	}
	for name, q := range map[string]float64{"negative": -1, "NaN": math.NaN(), "Inf": math.Inf(1)} {
		r := base
		r.QuoteAmount = q
		if _, err := b.Prepare(ctx, r); err == nil {
			t.Errorf("%s quote_amount was accepted", name)
		}
	}
	fut := approval.OrderRequest{Provider: v.id, Symbol: "BTC/USDT:USDT", Market: approval.MarketFutures, Side: "buy", QuoteAmount: 20}
	if _, err := b.Prepare(ctx, fut); err == nil || !strings.Contains(err.Error(), "notional_usd") {
		t.Errorf("futures: %v", err)
	}
	spotContract := approval.OrderRequest{Provider: v.id, Symbol: "BTC/USDT:USDT", Side: "buy", QuoteAmount: 20}
	if _, err := b.Prepare(ctx, spotContract); err == nil || !strings.Contains(err.Error(), "notional_usd") {
		t.Errorf("a contract symbol on the spot market: %v", err)
	}
	small := base
	small.QuoteAmount = 0.01 // 0.0000001 BTC, under one 1e-6 step
	if _, err := b.Prepare(ctx, small); err == nil || !strings.Contains(err.Error(), "less than one tradable step") {
		t.Errorf("below one step: %v", err)
	}
	// The USD cap holds on the converted value.
	if _, err := NewApprovalOrderBackend(capConfig(10)).Prepare(ctx, approval.OrderRequest{Provider: v.id, Symbol: "BTC/USDT", Side: "buy", QuoteAmount: 20}); err == nil ||
		!strings.Contains(err.Error(), "max_order_value_usd") {
		t.Errorf("20 USDT under a 10 USD cap: %v", err)
	}
}

func TestQuoteAmount_ProposeTool_PassesItAndRefusesBadArguments(t *testing.T) {
	cfg := approvalsCfg(true)
	reg, fp := armPropose(t, cfg)
	ctx := WithToolSender(WithToolOrigin(context.Background(), OriginRemote), proposeOwner)
	ctx = WithToolContext(ctx, "telegram", "1001")

	res := reg.ExecuteWithContext(ctx, NameProposeOrder, map[string]any{"provider": "okx", "symbol": "BTC/USDT", "side": "buy", "quote_amount": 20.0}, "telegram", "1001", nil)
	if res.IsError {
		t.Fatalf("refused: %s", res.ForLLM)
	}
	if fp.count() != 1 || fp.reqs[0].Order.QuoteAmount != 20 || fp.reqs[0].Order.Amount != 0 {
		t.Fatalf("the request = %+v", fp.reqs)
	}

	for name, args := range map[string]map[string]any{
		"both":  {"provider": "okx", "symbol": "BTC/USDT", "side": "buy", "quote_amount": 20.0, "amount": 0.001},
		"NaN":   {"provider": "okx", "symbol": "BTC/USDT", "side": "buy", "quote_amount": math.NaN()},
		"zero":  {"provider": "okx", "symbol": "BTC/USDT", "side": "buy", "quote_amount": 0.0},
		"minus": {"provider": "okx", "symbol": "BTC/USDT", "side": "buy", "quote_amount": -3.0},
	} {
		before := fp.count()
		res := reg.ExecuteWithContext(ctx, NameProposeOrder, args, "telegram", "1001", nil)
		if !res.IsError || fp.count() != before {
			t.Errorf("%s: not refused before the approval service: %+v", name, res)
		}
	}
}
