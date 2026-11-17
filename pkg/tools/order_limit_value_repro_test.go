package tools

import (
	"context"
	"math"
	"os"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// HYP-1: an order's USD value, for the max_order_value_usd cap and for the card,
// is amount x the LIMIT price. A sell limit far below the market is marketable
// (it fills near the bid), so it passes a cap that the same order as a market
// order fails, and its card shows a fraction of the real value.
//
// Fixed: a limit order is valued at amount x max(limit, market reference), for the
// cap and for the card. These were reproducers gated behind KQ_REPRO_HYP1=1; the gate
// went with the fix, so they run with the suite.

const (
	hyp1Market  = 100.0 // BASE/USDT bid, ask and last
	hyp1Amount  = 5.0   // 5 BASE: about 500 USDT at the market
	hyp1Limit   = 1.0   // a sell limit at 1 USDT fills at the bid (about 100)
	hyp1Cap     = 100.0 // trading_risk.max_order_value_usd
	hyp1Holding = 10.0  // BASE held, so the sell is risk-reducing
)

func hyp1Venue(t *testing.T, id string) *pricedVenue {
	t.Helper()
	v := newPricedVenue(t, id)
	v.setTicker("BASE/USDT", hyp1Market, hyp1Market, hyp1Market)
	v.balances = append(v.balances, broker.Balance{Asset: "BASE", Free: hyp1Holding})
	return v
}

func skipUnlessRepro(t *testing.T, env, defect string) {
	t.Helper()
	if os.Getenv(env) != "1" {
		t.Skipf("reproducer of an unfixed defect (%s): set %s=1 to run it", defect, env)
	}
}

// Control (always on): the same sell as a MARKET order is valued at the bid, about
// 500 USD, and the 100 USD cap refuses it.
func TestRepro_HYP1_Control_MarketSellOverCapRefused(t *testing.T) {
	v := hyp1Venue(t, "hyp1-ctl-prop")
	b := NewApprovalOrderBackend(capConfig(hyp1Cap))
	_, err := b.Prepare(context.Background(), approval.OrderRequest{Provider: v.id, Symbol: "BASE/USDT", Side: "sell", Amount: hyp1Amount})
	if err == nil || !strings.Contains(err.Error(), "max_order_value_usd") {
		t.Fatalf("a market sell worth about 500 USD under a 100 USD cap: err = %v", err)
	}
}

// Control (always on): create_order refuses the same market sell on the cap.
func TestRepro_HYP1_Control_CreateOrderMarketSellOverCapRefused(t *testing.T) {
	v := hyp1Venue(t, "hyp1-ctl-create")
	res := placeOrder(capConfig(hyp1Cap), v.id, "BASE/USDT", "market", "sell", hyp1Amount, nil, false)
	if !res.IsError || !strings.Contains(res.ForLLM, "max_order_value_usd") {
		t.Fatalf("a market sell worth about 500 USD under a 100 USD cap: %+v", res)
	}
}

// Reproducer, propose path: a sell limit at 1 for 5 BASE (market value about 500) is
// accepted under a 100 USD cap, and its NotionalUSD is 5.
func TestRepro_HYP1_ProposeSellLimitBelowMarketBypassesCap(t *testing.T) {
	v := hyp1Venue(t, "hyp1-prop")
	b := NewApprovalOrderBackend(capConfig(hyp1Cap))
	limit := hyp1Limit
	got, err := b.Prepare(context.Background(), approval.OrderRequest{
		Provider: v.id, Symbol: "BASE/USDT", Side: "sell", Type: "limit", Amount: hyp1Amount, Price: &limit,
	})
	if err != nil {
		if !strings.Contains(err.Error(), "max_order_value_usd") {
			t.Fatalf("refused, but not on the cap: %v", err)
		}
		return // refused by the cap: what should happen
	}
	// Not refused: the card's USD figure must then be near the real value (about 500).
	if math.Abs(got.Params.NotionalUSD-hyp1Amount*hyp1Market) > 0.2*hyp1Amount*hyp1Market {
		t.Errorf("the sell limit @%v for %v BASE (market %v, worth about %v USD) passed the %v USD cap and was valued at NotionalUSD=%v (ref %v)",
			hyp1Limit, hyp1Amount, hyp1Market, hyp1Amount*hyp1Market, hyp1Cap, got.Params.NotionalUSD, got.Params.RefPrice)
	}
}

// Reproducer, create_order: the owner's own tool values a sell limit the same way,
// twice (Gate 1c uses amount x price with no market read; Gate 3b passes the limit
// to the estimator).
func TestRepro_HYP1_CreateOrderSellLimitBelowMarketBypassesCap(t *testing.T) {
	v := hyp1Venue(t, "hyp1-create")
	res := placeOrder(capConfig(hyp1Cap), v.id, "BASE/USDT", "limit", "sell", hyp1Amount, capPrice(hyp1Limit), false)
	if !res.IsError || !strings.Contains(res.ForLLM, "max_order_value_usd") {
		t.Errorf("a sell limit @%v for %v BASE (market %v, worth about %v USD) was not refused on the %v USD cap: IsError=%v ForLLM=%q",
			hyp1Limit, hyp1Amount, hyp1Market, hyp1Amount*hyp1Market, hyp1Cap, res.IsError, res.ForLLM)
	}
	if v.orderCount() != 0 {
		t.Error("CreateOrder was reached")
	}
}

// A sell limit ABOVE the market rests and fills at its limit: valued at the limit.
func TestHYP1_ProposeSellLimitAboveMarketValuedAtLimit(t *testing.T) {
	v := hyp1Venue(t, "hyp1-sell-above")
	limit := 120.0
	req := approval.OrderRequest{Provider: v.id, Symbol: "BASE/USDT", Side: "sell", Type: "limit", Amount: hyp1Amount, Price: &limit}
	got, err := NewApprovalOrderBackend(capConfig(0)).Prepare(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if want := hyp1Amount * limit; math.Abs(got.Params.NotionalUSD-want) > 1e-9 {
		t.Errorf("NotionalUSD = %v, want %v (amount x limit, the higher of limit and market)", got.Params.NotionalUSD, want)
	}
	if got.Params.RefPrice != hyp1Market {
		t.Errorf("RefPrice = %v, want the market %v", got.Params.RefPrice, hyp1Market)
	}
	// Under a cap between the market value (500) and the limit value (600) it is refused.
	if _, err := NewApprovalOrderBackend(capConfig(550)).Prepare(context.Background(), req); err == nil || !strings.Contains(err.Error(), "max_order_value_usd") {
		t.Errorf("a sell limit worth 600 under a 550 cap: err = %v", err)
	}
}

// A buy limit above the market is valued at its limit (what it can cost at most); one
// below the market at the market, the conservative side the cap is held to.
func TestHYP1_ProposeBuyLimitValuedAtMaxOfLimitAndMarket(t *testing.T) {
	v := hyp1Venue(t, "hyp1-buy")
	for _, tc := range []struct {
		name  string
		limit float64
		want  float64
	}{
		{"above market", 130, hyp1Amount * 130},
		{"below market", 50, hyp1Amount * hyp1Market},
	} {
		limit := tc.limit
		got, err := NewApprovalOrderBackend(capConfig(0)).Prepare(context.Background(), approval.OrderRequest{
			Provider: v.id, Symbol: "BASE/USDT", Side: "buy", Type: "limit", Amount: hyp1Amount, Price: &limit,
		})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if math.Abs(got.Params.NotionalUSD-tc.want) > 1e-9 {
			t.Errorf("%s: NotionalUSD = %v, want %v", tc.name, got.Params.NotionalUSD, tc.want)
		}
	}
	limit := 130.0
	if _, err := NewApprovalOrderBackend(capConfig(600)).Prepare(context.Background(), approval.OrderRequest{
		Provider: v.id, Symbol: "BASE/USDT", Side: "buy", Type: "limit", Amount: hyp1Amount, Price: &limit,
	}); err == nil || !strings.Contains(err.Error(), "max_order_value_usd") {
		t.Errorf("a buy limit worth 650 under a 600 cap: err = %v", err)
	}
}

// The approval.order recheck re-values a stored limit order at the market, so an order
// stored under one cap is refused at execution if the cap is now lower, and its
// NotionalUSD is the market-based figure.
func TestHYP1_RecheckValuesLimitAtMarket(t *testing.T) {
	v := hyp1Venue(t, "hyp1-recheck")
	limit := hyp1Limit
	prep, err := NewApprovalOrderBackend(capConfig(0)).Prepare(context.Background(), approval.OrderRequest{
		Provider: v.id, Symbol: "BASE/USDT", Side: "sell", Type: "limit", Amount: hyp1Amount, Price: &limit,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewApprovalOrderBackend(capConfig(hyp1Cap)).Recheck(context.Background(), prep.Params); err == nil || !strings.Contains(err.Error(), "max_order_value_usd") {
		t.Fatalf("a stored sell limit @1 for 5 BASE (market 100) must be refused on the 100 USD cap at recheck: %v", err)
	}
	rc, err := NewApprovalOrderBackend(capConfig(1000)).Recheck(context.Background(), prep.Params)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(rc.NotionalUSD-hyp1Amount*hyp1Market) > 1e-9 {
		t.Errorf("recheck NotionalUSD = %v, want %v", rc.NotionalUSD, hyp1Amount*hyp1Market)
	}
}

// create_order: the cap is measured at max(limit, market) for a buy limit above the market.
func TestHYP1_CreateOrderBuyLimitAboveMarketOverCapRefused(t *testing.T) {
	v := hyp1Venue(t, "hyp1-create-buy")
	res := placeOrder(capConfig(600), v.id, "BASE/USDT", "limit", "buy", hyp1Amount, capPrice(130), false)
	if !res.IsError || !strings.Contains(res.ForLLM, "max_order_value_usd") {
		t.Fatalf("a buy limit worth 650 under a 600 cap: %+v", res)
	}
}

// A limit order that cannot be valued against the market is refused while a cap is set.
func TestHYP1_UnknownMarketReferenceRefusesLimit(t *testing.T) {
	v := newPricedVenue(t, "hyp1-noref")
	res := placeOrder(capConfig(hyp1Cap), v.id, "BASE/USDT", "limit", "sell", 1, capPrice(50), false)
	if !res.IsError || res.ForLLM == "" {
		t.Fatalf("no market price, a cap set: %+v", res)
	}
	if v.orderCount() != 0 {
		t.Error("CreateOrder was reached")
	}
}

// Futures: a limit order is valued at the higher of its limit and the mark.
func TestHYP1_FuturesLimitValuedAtMaxOfLimitAndMark(t *testing.T) {
	installFutures(t) // mark 100, contract size 1
	cfg := capConfig(0)
	cfg.TradingRisk.AllowLeverage = true
	for _, tc := range []struct {
		name  string
		limit float64
		want  float64
	}{{"limit above mark", 150, 10 * 150}, {"limit below mark", 40, 10 * 100}} {
		limit := tc.limit
		req := futReq("buy", 1000, false)
		req.Type, req.Price = "limit", &limit
		got, err := NewApprovalOrderBackend(cfg).Prepare(context.Background(), req)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if math.Abs(got.Params.NotionalUSD-tc.want) > 1e-9 {
			t.Errorf("%s: NotionalUSD = %v, want %v", tc.name, got.Params.NotionalUSD, tc.want)
		}
	}
	cfg.TradingRisk.MaxOrderValueUSD = 1200
	limit := 150.0
	req := futReq("buy", 1000, false)
	req.Type, req.Price = "limit", &limit
	if _, err := NewApprovalOrderBackend(cfg).Prepare(context.Background(), req); err == nil || !strings.Contains(err.Error(), "gate risk") {
		t.Errorf("a futures limit worth 1500 under a 1200 cap: err = %v", err)
	}
}

// futures_open_position: the estimator values a limit at max(limit, mark), so a limit far
// below the mark cannot slip under max_order_value_usd; with no mark it is an error.
func TestHYP1_EstimateFuturesNotionalLimitValuedAtMaxOfLimitAndMark(t *testing.T) {
	fp := &mockFuturesProvider{markPriceFn: func(context.Context, string) (float64, error) { return 100, nil }}
	for _, tc := range []struct {
		name  string
		limit float64
		want  float64
		src   string
	}{
		{"limit far below the mark", 1, 10 * 2 * 100, "mark"}, // 10 contracts x 2 base per contract x mark
		{"limit above the mark", 150, 10 * 2 * 150, "explicit"},
	} {
		limit := tc.limit
		n, src, err := estimateFuturesNotional(context.Background(), fp, "BASE/USDT:USDT", 10, 2, &limit)
		if err != nil || math.Abs(n-tc.want) > 1e-9 || src != tc.src {
			t.Errorf("%s: notional=%v src=%q err=%v, want %v %q", tc.name, n, src, err, tc.want, tc.src)
		}
	}
	limit := 1.0
	if _, _, err := estimateFuturesNotional(context.Background(), &mockFuturesProvider{}, "BASE/USDT:USDT", 10, 2, &limit); err == nil {
		t.Error("a limit with no mark price to compare against must be an error")
	}
}
