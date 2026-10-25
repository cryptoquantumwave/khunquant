package tools

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

func approvalVenue(t *testing.T, id string) *pricedVenue {
	t.Helper()
	v := newPricedVenue(t, id)
	v.setTicker("BTC/USDT", 60000, 59990, 59995)
	v.balances = append(v.balances, broker.Balance{Asset: "BTC", Free: 1})
	return v
}

func spotReq(id, side string, amount float64) approval.OrderRequest {
	return approval.OrderRequest{Provider: id, Symbol: "BTC/USDT", Side: side, Amount: amount}
}

func TestApprovalBackend_PrepareSpotDirectionAndPricing(t *testing.T) {
	v := approvalVenue(t, "apb-spot")
	b := NewApprovalOrderBackend(capConfig(0))
	ctx := context.Background()

	got, err := b.Prepare(ctx, spotReq(v.id, "sell", 0.01))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Reducing || got.Mode != approval.ModeLive {
		t.Errorf("a sell within holdings: reducing=%v mode=%s", got.Reducing, got.Mode)
	}
	p := got.Params
	if p.Side != "sell" || p.Amount != 0.01 || p.Market != approval.MarketSpot || p.Type != "market" || p.PriceCollarPct != approval.DefaultCollarPct {
		t.Errorf("params = %+v", p)
	}
	if p.RefPrice != 59990 || math.Abs(p.NotionalUSD-599.9) > 1e-6 {
		t.Errorf("ref %v, notional %v: a market sell is priced at the bid, valued in USD", p.RefPrice, p.NotionalUSD)
	}

	if got, _ := b.Prepare(ctx, spotReq(v.id, "sell", 5)); got.Reducing {
		t.Error("a sell of more than the account holds is not risk-reducing")
	}
	if got, _ := b.Prepare(ctx, spotReq(v.id, "buy", 0.01)); got.Reducing || got.Params.RefPrice != 60000 {
		t.Errorf("a buy: reducing=%v ref=%v (the ask)", got.Reducing, got.Params.RefPrice)
	}

	// A limit order is valued at its limit and referenced to the market.
	limit := 59000.0
	req := spotReq(v.id, "sell", 0.01)
	req.Type, req.Price = "limit", &limit
	got, err = b.Prepare(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if got.Params.RefPrice != 59990 || math.Abs(got.Params.NotionalUSD-590) > 1e-6 || got.Params.Price == nil || *got.Params.Price != limit {
		t.Errorf("limit params = %+v", got.Params)
	}
}

// The classifier create_order uses is the one the approval path uses.
func TestOrderAddsRisk_OneDefinition(t *testing.T) {
	for _, tc := range []struct {
		symbol, side string
		params       map[string]any
		reduceOnly   bool
		want         bool
	}{
		{"BTC/USDT", "sell", nil, false, false},
		{"BTC/USDT", "buy", nil, false, true},
		{"BTC/USDT:USDT", "sell", nil, false, true}, // a futures short
		{"BTC/USDT:USDT", "sell", nil, true, false}, // reduce-only
		{"BTC/USDT:USDT", "buy", nil, true, false},
		{"BTC/USDT:USDT", "buy", nil, false, true},
		{"BTC/USDT", "sell", map[string]any{"tdMode": "cross"}, false, true},
		{"BTC/USDT", "sell", map[string]any{"borrow": true}, false, true},
		{"BTC/USDT", "sell", map[string]any{"leverage": 3}, false, true},
		{"BTC/USDT", "SELL", nil, false, false},
		{"BTC/USDT", "buy", nil, true, true}, // reduce_only means nothing on spot
	} {
		if got := orderAddsRisk(tc.symbol, tc.side, tc.params, tc.reduceOnly); got != tc.want {
			t.Errorf("orderAddsRisk(%s, %s, %v, %v) = %v, want %v", tc.symbol, tc.side, tc.params, tc.reduceOnly, got, tc.want)
		}
	}
}

// Caps are in USD: a THB order is valued with the rate, an unknown value refuses
// whether or not a cap is set.
func TestApprovalBackend_USDCapAndUnknownUSD(t *testing.T) {
	ctx := context.Background()
	v := newPricedVenue(t, "apb-thb")
	v.setTicker("USDT/THB", 0, 0, 36)
	v.setTicker("BTC/THB", 100_000, 99_900, 99_950)
	b := NewApprovalOrderBackend(capConfig(100))

	_, err := b.Prepare(ctx, approval.OrderRequest{Provider: v.id, Symbol: "BTC/THB", Side: "buy", Amount: 0.05})
	if err == nil || !strings.Contains(err.Error(), "gate risk") || !strings.Contains(err.Error(), "max_order_value_usd") {
		t.Fatalf("5,000 THB (about 139 USD) over a 100 USD cap: err = %v", err)
	}
	got, err := b.Prepare(ctx, approval.OrderRequest{Provider: v.id, Symbol: "BTC/THB", Side: "buy", Amount: 0.03})
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(got.Params.NotionalUSD-83.33) > 0.05 {
		t.Errorf("NotionalUSD = %v, want about 83.33 (3,000 THB at 36)", got.Params.NotionalUSD)
	}
	if got.Params.RefPrice != 100_000 {
		t.Errorf("RefPrice = %v (the quote currency's price)", got.Params.RefPrice)
	}

	noRate := newPricedVenue(t, "apb-norate")
	noRate.setTicker("BTC/THB", 100_000, 99_900, 99_950)
	for name, cfg := range map[string]*config.Config{"cap set": capConfig(100), "no cap": capConfig(0)} {
		_, err := NewApprovalOrderBackend(cfg).Prepare(ctx, approval.OrderRequest{Provider: noRate.id, Symbol: "BTC/THB", Side: "buy", Amount: 0.001})
		if err == nil || !strings.Contains(err.Error(), "USD conversion rate") {
			t.Errorf("%s: an order whose USD value is unknown was not refused for that cause: %v", name, err)
		}
	}
}

func TestApprovalBackend_RefusesUnusableRequests(t *testing.T) {
	v := approvalVenue(t, "apb-bad")
	b := NewApprovalOrderBackend(capConfig(0))
	px := 59000.0
	nan, inf := math.NaN(), math.Inf(1)
	for name, mutate := range map[string]func(*approval.OrderRequest){
		"amount zero":           func(r *approval.OrderRequest) { r.Amount = 0 },
		"amount negative":       func(r *approval.OrderRequest) { r.Amount = -1 },
		"amount NaN":            func(r *approval.OrderRequest) { r.Amount = nan },
		"amount Inf":            func(r *approval.OrderRequest) { r.Amount = inf },
		"side unknown":          func(r *approval.OrderRequest) { r.Side = "short" },
		"type stop_loss":        func(r *approval.OrderRequest) { r.Type = "stop_loss" },
		"limit without a price": func(r *approval.OrderRequest) { r.Type = "limit" },
		"limit price NaN":       func(r *approval.OrderRequest) { r.Type = "limit"; r.Price = &nan },
		"market with a price":   func(r *approval.OrderRequest) { r.Price = &px },
		"collar NaN":            func(r *approval.OrderRequest) { r.CollarPct = nan },
		"collar over 50":        func(r *approval.OrderRequest) { r.CollarPct = 60 },
		"collar negative":       func(r *approval.OrderRequest) { r.CollarPct = -1 },
		"reduce_only on spot":   func(r *approval.OrderRequest) { r.ReduceOnly = true },
		"no provider":           func(r *approval.OrderRequest) { r.Provider = "" },
		"no symbol":             func(r *approval.OrderRequest) { r.Symbol = "" },
		"unknown market":        func(r *approval.OrderRequest) { r.Market = "margin" },
		"unknown provider":      func(r *approval.OrderRequest) { r.Provider = "no-such-provider-xyz" },
		"futures without a size": func(r *approval.OrderRequest) {
			r.Market = approval.MarketFutures
			r.Symbol = "BTC/USDT:USDT"
			r.Amount = 1
		},
	} {
		req := spotReq(v.id, "sell", 0.01)
		mutate(&req)
		if _, err := b.Prepare(context.Background(), req); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if v.orderCount() != 0 {
		t.Error("an order reached the venue")
	}
}

// The collar is adverse-only: a buy is abandoned when the price rose, a sell when
// it fell; a move in the order's favour never aborts; one percent exactly is inside.
func TestApprovalBackend_RecheckCollarIsAdverseOnly(t *testing.T) {
	for _, tc := range []struct {
		name      string
		side      string
		ref, now  float64
		wantAbort bool
	}{
		{"buy, price up 1.5%", "buy", 100, 101.5, true},
		{"buy, price up exactly 1%", "buy", 100, 101, false},
		{"buy, price down 5% (in its favour)", "buy", 100, 95, false},
		{"sell, price down 1.5%", "sell", 100, 98.5, true},
		{"sell, price down exactly 1%", "sell", 100, 99, false},
		{"sell, price up 5% (in its favour)", "sell", 100, 105, false},
		{"no usable price", "buy", 100, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := adverseMovePct(tc.side, tc.ref, tc.now)
			if abort := got > approval.DefaultCollarPct; abort != tc.wantAbort {
				t.Errorf("adverse move %.4f%%: abort=%v, want %v", got, abort, tc.wantAbort)
			}
		})
	}

	// And through the gate, with a venue whose price moves after the proposal.
	v := approvalVenue(t, "apb-collar")
	b := NewApprovalOrderBackend(capConfig(0))
	ctx := context.Background()
	prep, err := b.Prepare(ctx, spotReq(v.id, "buy", 0.01))
	if err != nil {
		t.Fatal(err)
	}
	if prep.Reducing {
		t.Fatal("a buy is not risk-reducing")
	}
	if _, err := b.Recheck(ctx, prep.Params); err != nil {
		t.Fatalf("an unchanged price aborted: %v", err)
	}
	v.setTicker("BTC/USDT", 60000*0.95, 59990*0.95, 59995*0.95) // down 5%: in a buyer's favour
	if _, err := b.Recheck(ctx, prep.Params); err != nil {
		t.Fatalf("a favourable move aborted: %v", err)
	}
	v.setTicker("BTC/USDT", 60000*1.05, 59990*1.05, 59995*1.05) // up 5%: against a buyer
	_, err = b.Recheck(ctx, prep.Params)
	if err == nil || !strings.Contains(err.Error(), "price collar") || !strings.Contains(err.Error(), "adverse direction only") ||
		!strings.Contains(err.Error(), "nothing was placed") {
		t.Fatalf("a buy after a 5%% rise: err = %v", err)
	}
}

// An order that only reduces risk is never abandoned for a price move: a safety
// brake must not block getting out of a position.
func TestApprovalBackend_RecheckNeverBlocksAnExitOnPrice(t *testing.T) {
	ctx := context.Background()

	t.Run("a reducing market sell after a 5% drop executes", func(t *testing.T) {
		v := approvalVenue(t, "apb-exit-drop")
		b := NewApprovalOrderBackend(capConfig(0))
		prep, err := b.Prepare(ctx, spotReq(v.id, "sell", 0.01))
		if err != nil || !prep.Reducing {
			t.Fatalf("prepare: %v reducing=%v", err, prep.Reducing)
		}
		v.setTicker("BTC/USDT", 60000*0.95, 59990*0.95, 59995*0.95)
		rc, err := b.Recheck(ctx, prep.Params)
		if err != nil {
			t.Fatalf("an exit was abandoned for a price move: %v", err)
		}
		if !rc.Reducing || math.Abs(rc.LastPrice-59990*0.95) > 1e-6 {
			t.Errorf("recheck = %+v: it must report the price it saw", rc)
		}
	})

	t.Run("a reducing limit exit keeps its limit price", func(t *testing.T) {
		v := approvalVenue(t, "apb-exit-limit")
		b := NewApprovalOrderBackend(capConfig(0))
		limit := 59000.0
		req := spotReq(v.id, "sell", 0.01)
		req.Type, req.Price = "limit", &limit
		prep, err := b.Prepare(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		v.setTicker("BTC/USDT", 60000*0.9, 59990*0.9, 59995*0.9)
		if _, err := b.Recheck(ctx, prep.Params); err != nil {
			t.Fatalf("a limit exit was abandoned: %v", err)
		}
		if prep.Params.Price == nil || *prep.Params.Price != limit {
			t.Errorf("the stored limit is %v", prep.Params.Price)
		}
	})

	t.Run("a reducing order with no usable price still refuses", func(t *testing.T) {
		v := approvalVenue(t, "apb-exit-noprice")
		b := NewApprovalOrderBackend(capConfig(0))
		prep, err := b.Prepare(ctx, spotReq(v.id, "sell", 0.01))
		if err != nil {
			t.Fatal(err)
		}
		v.mu.Lock()
		delete(v.tickers, "BTC/USDT")
		v.mu.Unlock()
		if _, err := b.Recheck(ctx, prep.Params); err == nil || !strings.Contains(err.Error(), "gate risk") {
			t.Fatalf("an exit with an unknown USD value: err = %v", err)
		}
	})

	t.Run("the gates that apply to an exit still run: the USD cap", func(t *testing.T) {
		v := approvalVenue(t, "apb-exit-cap")
		prep, err := NewApprovalOrderBackend(capConfig(0)).Prepare(ctx, spotReq(v.id, "sell", 0.01))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := NewApprovalOrderBackend(capConfig(100)).Recheck(ctx, prep.Params); err == nil {
			t.Error("a cap lowered after the proposal did not apply to the exit")
		}
	})

	t.Run("a futures reduce-only exit after a 10% drop executes", func(t *testing.T) {
		installFutures(t)
		b := NewApprovalOrderBackend(noLeverage())
		prep, err := b.Prepare(ctx, futReq("sell", 1000, true))
		if err != nil {
			t.Fatal(err)
		}
		prep.Params.RefPrice = 200 // the mark is 100 now: down 50% against a seller
		if _, err := b.Recheck(ctx, prep.Params); err != nil {
			t.Fatalf("a futures exit was abandoned for a price move: %v", err)
		}
	})
}

func TestApprovalBackend_RecheckRunsTheGatesAgain(t *testing.T) {
	v := approvalVenue(t, "apb-recheck")
	ctx := context.Background()
	prep, err := NewApprovalOrderBackend(capConfig(0)).Prepare(ctx, spotReq(v.id, "sell", 0.01))
	if err != nil {
		t.Fatal(err)
	}
	// The cap is lowered after the proposal: 600 USD no longer fits.
	_, err = NewApprovalOrderBackend(capConfig(100)).Recheck(ctx, prep.Params)
	if err == nil || !strings.Contains(err.Error(), "gate risk") {
		t.Fatalf("err = %v, want the risk gate", err)
	}
	// The holding is sold meanwhile: the order is no longer an exit.
	v.balances = []broker.Balance{{Asset: "USDT", Free: 1e9}}
	rc, err := NewApprovalOrderBackend(capConfig(0)).Recheck(ctx, prep.Params)
	if err != nil {
		t.Fatal(err)
	}
	if rc.Reducing {
		t.Error("a sell the account can no longer cover was rechecked as reducing")
	}
}

// Paper mode: the order goes through the simulation (nothing reaches the venue),
// takes no limiter token, and a simulated order is not fetched back.
func TestApprovalBackend_PaperModeSpot(t *testing.T) {
	v := approvalVenue(t, "apb-paper")
	cfg := capConfig(0)
	cfg.TradingRisk.PaperTradingMode = true
	b := NewApprovalOrderBackend(cfg)
	ctx := context.Background()

	prep, err := b.Prepare(ctx, spotReq(v.id, "sell", 0.01))
	if err != nil {
		t.Fatal(err)
	}
	if prep.Mode != approval.ModePaper {
		t.Fatalf("mode = %s", prep.Mode)
	}
	orig := broker.DefaultLimiter
	broker.DefaultLimiter = broker.NewTokenBucketLimiter(1, time.Minute)
	t.Cleanup(func() { broker.DefaultLimiter = orig })
	for i := 0; i < 3; i++ {
		if ok, _ := b.Throttle(prep.Params); !ok {
			t.Fatal("a simulated order spent the venue's request budget")
		}
	}
	placed, err := b.PlaceApprovedOrder(ctx, prep.Params, "wf1:place", 1, time.Now(), approval.Rechecked{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(placed.OrderID, broker.PaperOrderIDPrefix) || v.orderCount() != 0 {
		t.Errorf("order %q, %d orders on the venue: the simulation must be what was placed with", placed.OrderID, v.orderCount())
	}
	if !placed.Fill.Known || placed.Fill.Filled != 0.01 || placed.Fill.AvgPrice != 59990 {
		t.Errorf("fill from the answer = %+v", placed.Fill)
	}
	if _, err := b.Fetch(ctx, prep.Params, placed.OrderID); err == nil {
		t.Error("a simulated order was fetched back as if it were real")
	}
}

func TestApprovalBackend_ThrottleLive(t *testing.T) {
	orig := broker.DefaultLimiter
	broker.DefaultLimiter = broker.NewTokenBucketLimiter(1, time.Minute)
	t.Cleanup(func() { broker.DefaultLimiter = orig })
	b := NewApprovalOrderBackend(capConfig(0))
	p := approval.OrderParams{Provider: "venue-x", Market: approval.MarketSpot}
	if ok, _ := b.Throttle(p); !ok {
		t.Fatal("the first token was refused")
	}
	ok, after := b.Throttle(p)
	if ok || after <= 0 {
		t.Errorf("second call = %v, %v; want refused with a wait", ok, after)
	}
}

func installFutures(t *testing.T) *dnFakeFutures {
	t.Helper()
	fut := &dnFakeFutures{mockFuturesProvider: &mockFuturesProvider{}}
	orig := futuresProviderFn
	futuresProviderFn = func(context.Context, *config.Config, string, string) (broker.FuturesProvider, error) { return fut, nil }
	t.Cleanup(func() { futuresProviderFn = orig })
	return fut
}

// noLeverage is a config that has not opted in to leverage.
func noLeverage() *config.Config {
	cfg := capConfig(0)
	cfg.TradingRisk.AllowLeverage = false
	return cfg
}

func futReq(side string, notional float64, reduceOnly bool) approval.OrderRequest {
	return approval.OrderRequest{Provider: "okx", Market: approval.MarketFutures, Symbol: "BTC/USDT:USDT", Side: side, NotionalUSD: notional, ReduceOnly: reduceOnly}
}

func TestApprovalBackend_Futures(t *testing.T) {
	ctx := context.Background()
	installFutures(t)

	t.Run("a futures amount is a contract count computed from the notional", func(t *testing.T) {
		cfg := capConfig(0)
		cfg.TradingRisk.AllowLeverage = true
		got, err := NewApprovalOrderBackend(cfg).Prepare(ctx, futReq("buy", 1000, false))
		if err != nil {
			t.Fatal(err)
		}
		// mark 100, contract size 1, step 1: 1000 USD is 10 contracts, not 1000 / 100 base units re-read as USD.
		if got.Params.Amount != 10 || got.Params.NotionalUSD != 1000 || got.Params.RefPrice != 100 {
			t.Errorf("params = %+v", got.Params)
		}
		if got.Reducing || got.Params.PositionSide != "long" {
			t.Errorf("a futures entry: reducing=%v side=%s", got.Reducing, got.Params.PositionSide)
		}
	})

	t.Run("opening needs the leverage opt-in", func(t *testing.T) {
		cfg := capConfig(0)
		cfg.TradingRisk.AllowLeverage = false
		_, err := NewApprovalOrderBackend(cfg).Prepare(ctx, futReq("buy", 1000, false))
		if err == nil || !strings.Contains(err.Error(), "gate leverage") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("a reduce-only exit is not stopped by the leverage switch and is reducing", func(t *testing.T) {
		got, err := NewApprovalOrderBackend(noLeverage()).Prepare(ctx, futReq("sell", 1000, true))
		if err != nil {
			t.Fatal(err)
		}
		if !got.Reducing || got.Params.PositionSide != "long" || !got.Params.ReduceOnly || got.Params.Amount != 10 {
			t.Errorf("params = %+v reducing=%v", got.Params, got.Reducing)
		}
	})

	t.Run("a reduce-only order must close a position that exists, on its closing side, by no more than it holds", func(t *testing.T) {
		b := NewApprovalOrderBackend(noLeverage())
		if _, err := b.Prepare(ctx, futReq("buy", 1000, true)); err == nil { // the fake position is long: a buy would not close it
			t.Error("a reduce-only buy against a long position was accepted")
		}
		if _, err := b.Prepare(ctx, futReq("sell", 6000, true)); err == nil || !strings.Contains(err.Error(), "larger than the open position") {
			t.Errorf("60 contracts against 50 held: err = %v", err)
		}
	})

	t.Run("the USD cap holds for futures", func(t *testing.T) {
		cfg := capConfig(500)
		cfg.TradingRisk.AllowLeverage = true
		if _, err := NewApprovalOrderBackend(cfg).Prepare(ctx, futReq("buy", 1000, false)); err == nil || !strings.Contains(err.Error(), "gate risk") {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("a size below one step is refused", func(t *testing.T) {
		cfg := capConfig(0)
		cfg.TradingRisk.AllowLeverage = true
		if _, err := NewApprovalOrderBackend(cfg).Prepare(ctx, futReq("buy", 50, false)); err == nil {
			t.Error("0.5 contracts accepted")
		}
	})

	t.Run("paper mode: an entry is refused, an exit is a REAL exit", func(t *testing.T) {
		cfg := capConfig(0)
		cfg.TradingRisk.PaperTradingMode = true
		cfg.TradingRisk.AllowLeverage = true
		b := NewApprovalOrderBackend(cfg)
		if _, err := b.Prepare(ctx, futReq("buy", 1000, false)); err == nil || !strings.Contains(err.Error(), "no simulation") {
			t.Errorf("a futures entry under paper mode: err = %v", err)
		}
		got, err := b.Prepare(ctx, futReq("sell", 1000, true))
		if err != nil {
			t.Fatal(err)
		}
		if got.Mode != approval.ModeRealExit {
			t.Errorf("mode = %s, want a real exit", got.Mode)
		}
	})
}

func TestApprovalBackend_PlaceFuturesExitAndRecordLoss(t *testing.T) {
	ctx := context.Background()
	fut := installFutures(t)
	resetRateLimiter(t)
	pnl := -10.0
	fut.pnl = &pnl
	b := NewApprovalOrderBackend(noLeverage())

	prep, err := b.Prepare(ctx, futReq("sell", 1000, true))
	if err != nil {
		t.Fatal(err)
	}
	rc, err := b.Recheck(ctx, prep.Params)
	if err != nil {
		t.Fatal(err)
	}
	if rc.PositionContracts != 50 || rc.PnLBefore == nil || *rc.PnLBefore != -10 {
		t.Fatalf("recheck baseline = %+v", rc)
	}
	placed, err := b.PlaceApprovedOrder(ctx, prep.Params, "wf1:place", 1, time.Now(), rc)
	if err != nil {
		t.Fatal(err)
	}
	if len(fut.orders) != 1 {
		t.Fatalf("%d futures orders", len(fut.orders))
	}
	o := fut.orders[0]
	if !o.ReduceOnly || o.Side != "sell" || o.Amount != 10 || o.PositionSide != "long" || o.OrderType != "market" {
		t.Errorf("order = %+v", o)
	}
	before := broker.GlobalLossTracker.TodayLossUSD()
	b.Record(ctx, prep.Params, rc, placed, approval.Fill{Known: true, Filled: 10, Status: "closed"})
	// 10 of 50 contracts closed: a fifth of the 10 USD loss.
	if got := broker.GlobalLossTracker.TodayLossUSD() - before; math.Abs(got-2) > 1e-9 {
		t.Errorf("recorded loss = %v, want 2", got)
	}
}

// A spot proposal on a contract symbol would be resolved to the perpetual by the
// exchange library, skipping the leverage opt-in and valuing the order without
// its contract size. Prepare refuses it, the spot gate refuses it on its own
// (defence in depth, and it is what a stored proposal meets again at Recheck),
// and the same symbol as a futures proposal still works.
func TestApprovalBackend_SpotRefusesContractSymbol(t *testing.T) {
	ctx := context.Background()
	installFutures(t)
	b := NewApprovalOrderBackend(noLeverage())

	t.Run("Prepare refuses, with a message the model can recover from", func(t *testing.T) {
		for _, market := range []string{"", approval.MarketSpot} { // spot is the default
			req := approval.OrderRequest{Provider: "okx", Market: market, Symbol: "BTC/USDT:USDT", Side: "buy", Amount: 1}
			_, err := b.Prepare(ctx, req)
			if err == nil || !strings.Contains(err.Error(), "use market=futures") {
				t.Errorf("market %q: err = %v, want a refusal that names market=futures", market, err)
			}
		}
	})

	t.Run("the spot gate refuses on its own", func(t *testing.T) {
		_, err := b.gateSpot(ctx, orderSpec{provider: "okx", market: approval.MarketSpot, symbol: "BTC/USDT:USDT", side: "buy", orderType: "market", amount: 1})
		if err == nil || !strings.Contains(err.Error(), "use market=futures") {
			t.Fatalf("err = %v, want the gate to refuse a contract symbol", err)
		}
	})

	t.Run("a stored spot proposal on a contract symbol is refused at Recheck", func(t *testing.T) {
		_, err := b.Recheck(ctx, approval.OrderParams{
			Provider: "okx", Market: approval.MarketSpot, Symbol: "BTC/USDT:USDT", Side: "buy", Type: "market",
			Amount: 1, RefPrice: 100, NotionalUSD: 100, PriceCollarPct: 1,
		})
		if err == nil || !strings.Contains(err.Error(), "use market=futures") {
			t.Fatalf("err = %v, want Recheck to refuse", err)
		}
	})

	t.Run("the same symbol as a futures proposal still works", func(t *testing.T) {
		fcfg := capConfig(0)
		fcfg.TradingRisk.AllowLeverage = true
		got, err := NewApprovalOrderBackend(fcfg).Prepare(ctx, futReq("buy", 1000, false))
		if err != nil {
			t.Fatal(err)
		}
		if got.Params.Market != approval.MarketFutures || got.Params.Symbol != "BTC/USDT:USDT" {
			t.Errorf("params = %+v", got.Params)
		}
	})
}
