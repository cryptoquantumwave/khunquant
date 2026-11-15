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
// The reproducers are of an unfixed defect and are gated behind KQ_REPRO_HYP1=1,
// so the default suite stays green. The change that fixes the defect deletes the
// gate. The controls run always: they pin what is already right.

const (
	hyp1Market  = 100.0 // BASE/USDT bid, ask and last
	hyp1Amount  = 5.0   // 5 BASE: about 500 USDT at the market
	hyp1Limit   = 1.0   // a sell limit at 1 USDT fills at the bid (about 100)
	hyp1Cap     = 100.0 // trading_risk.max_order_value_usd
	hyp1Holding = 10.0  // BASE held, so the sell is risk-reducing
	hyp1Defect  = "HYP-1: a limit order is valued at amount x limit price, not at what it would fill at"
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
	skipUnlessRepro(t, "KQ_REPRO_HYP1", hyp1Defect)
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
	skipUnlessRepro(t, "KQ_REPRO_HYP1", hyp1Defect)
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
