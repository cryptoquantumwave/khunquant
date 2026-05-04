package tools

import (
	"math"
	"testing"

	ccxt "github.com/ccxt/ccxt/go/v4"
)

func floatPtr(f float64) *float64 { return &f }
func strPtr(s string) *string     { return &s }
func int64Ptr(i int64) *int64     { return &i }

func near(a, b float64) bool { return math.Abs(a-b) < 0.0001 }

func TestComputeAvgCost_BuyOnly(t *testing.T) {
	trades := []ccxt.Trade{
		{Side: strPtr("buy"), Amount: floatPtr(1), Price: floatPtr(100), Cost: floatPtr(100), Timestamp: int64Ptr(1000)},
		{Side: strPtr("buy"), Amount: floatPtr(2), Price: floatPtr(110), Cost: floatPtr(220), Timestamp: int64Ptr(2000)},
	}
	r := ComputeAvgCost(trades)

	if !near(r.Held.Qty, 3) {
		t.Errorf("Held.Qty = %v, want 3", r.Held.Qty)
	}
	wantAvg := (100.0 + 220.0) / 3
	if !near(r.Held.AvgCost, wantAvg) {
		t.Errorf("Held.AvgCost = %v, want %.4f", r.Held.AvgCost, wantAvg)
	}
	if r.Realized != 0 {
		t.Errorf("Realized = %v, want 0 (no sells)", r.Realized)
	}
	if r.BoughtQty != 3 {
		t.Errorf("BoughtQty = %v, want 3", r.BoughtQty)
	}
}

func TestComputeAvgCost_BuyPartialSellBuy(t *testing.T) {
	// Buy 2 @ 100 → avg 100. Sell 1 @ 150 → realized = (150-100)*1 = 50.
	// Buy 1 @ 120 → new avg = (100*1 + 120*1)/2 = 110.
	trades := []ccxt.Trade{
		{Side: strPtr("buy"), Amount: floatPtr(2), Price: floatPtr(100), Cost: floatPtr(200), Timestamp: int64Ptr(1000)},
		{Side: strPtr("sell"), Amount: floatPtr(1), Price: floatPtr(150), Cost: floatPtr(150), Timestamp: int64Ptr(2000)},
		{Side: strPtr("buy"), Amount: floatPtr(1), Price: floatPtr(120), Cost: floatPtr(120), Timestamp: int64Ptr(3000)},
	}
	r := ComputeAvgCost(trades)

	if !near(r.Realized, 50) {
		t.Errorf("Realized = %v, want 50", r.Realized)
	}
	if !near(r.Held.Qty, 2) {
		t.Errorf("Held.Qty = %v, want 2", r.Held.Qty)
	}
	wantAvg := (100.0 + 120.0) / 2
	if !near(r.Held.AvgCost, wantAvg) {
		t.Errorf("Held.AvgCost = %v, want %.4f", r.Held.AvgCost, wantAvg)
	}
}

func TestComputeAvgCost_SellDownToZeroThenRebuy(t *testing.T) {
	// Buy 1 @ 100, sell 1 @ 200 (realized = 100), buy 1 @ 300.
	// After rebuy, avg_cost should reset to 300.
	trades := []ccxt.Trade{
		{Side: strPtr("buy"), Amount: floatPtr(1), Price: floatPtr(100), Cost: floatPtr(100), Timestamp: int64Ptr(1000)},
		{Side: strPtr("sell"), Amount: floatPtr(1), Price: floatPtr(200), Cost: floatPtr(200), Timestamp: int64Ptr(2000)},
		{Side: strPtr("buy"), Amount: floatPtr(1), Price: floatPtr(300), Cost: floatPtr(300), Timestamp: int64Ptr(3000)},
	}
	r := ComputeAvgCost(trades)

	if !near(r.Realized, 100) {
		t.Errorf("Realized = %v, want 100", r.Realized)
	}
	if !near(r.Held.Qty, 1) {
		t.Errorf("Held.Qty = %v, want 1", r.Held.Qty)
	}
	if !near(r.Held.AvgCost, 300) {
		t.Errorf("Held.AvgCost = %v, want 300", r.Held.AvgCost)
	}
}

func TestComputeAvgCost_FeesAccumulate(t *testing.T) {
	trades := []ccxt.Trade{
		{Side: strPtr("buy"), Amount: floatPtr(1), Price: floatPtr(100), Cost: floatPtr(100),
			Fee: ccxt.Fee{Cost: floatPtr(0.5)}, Timestamp: int64Ptr(1000)},
		{Side: strPtr("sell"), Amount: floatPtr(1), Price: floatPtr(110), Cost: floatPtr(110),
			Fee: ccxt.Fee{Cost: floatPtr(0.3)}, Timestamp: int64Ptr(2000)},
	}
	r := ComputeAvgCost(trades)

	if !near(r.Fees, 0.8) {
		t.Errorf("Fees = %v, want 0.8", r.Fees)
	}
}

func TestComputeAvgCost_Empty(t *testing.T) {
	r := ComputeAvgCost(nil)
	if r.Held.Qty != 0 || r.Realized != 0 || r.Fees != 0 {
		t.Errorf("non-zero result on empty trades: %+v", r)
	}
	if r.TruncatedAt200 {
		t.Error("TruncatedAt200 should be false for empty input")
	}
}

func TestComputeAvgCost_TruncatedFlag(t *testing.T) {
	trades := make([]ccxt.Trade, 200)
	for i := range trades {
		ts := int64(i * 1000)
		trades[i] = ccxt.Trade{
			Side: strPtr("buy"), Amount: floatPtr(1), Price: floatPtr(100),
			Cost: floatPtr(100), Timestamp: &ts,
		}
	}
	r := ComputeAvgCost(trades)
	if !r.TruncatedAt200 {
		t.Error("TruncatedAt200 should be true when len(trades) == 200")
	}
}

func TestComputeAvgCost_NilCostDerivation(t *testing.T) {
	// When Cost is nil, it should be derived from qty * price.
	trades := []ccxt.Trade{
		{Side: strPtr("buy"), Amount: floatPtr(2), Price: floatPtr(50), Timestamp: int64Ptr(1000)},
	}
	r := ComputeAvgCost(trades)
	if !near(r.BoughtCost, 100) {
		t.Errorf("BoughtCost = %v, want 100 (derived from qty*price)", r.BoughtCost)
	}
	if !near(r.Held.AvgCost, 50) {
		t.Errorf("Held.AvgCost = %v, want 50", r.Held.AvgCost)
	}
}
