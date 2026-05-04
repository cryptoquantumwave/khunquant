package tools

import (
	"sort"

	ccxt "github.com/ccxt/ccxt/go/v4"
)

// PnLHeld holds the weighted-average state for currently held qty.
type PnLHeld struct {
	Qty       float64
	AvgCost   float64
	TotalCost float64
}

// PnLResult is the output of ComputeAvgCost.
type PnLResult struct {
	Held           PnLHeld
	BoughtQty      float64
	BoughtCost     float64
	SoldQty        float64
	SoldProceeds   float64
	Realized       float64
	Fees           float64
	TradeCount     int
	TruncatedAt200 bool
	FirstTs        *int64
	LastTs         *int64
}

// ComputeAvgCost replays a trade list using the weighted-average cost-basis
// method. Trades are sorted ascending by timestamp before processing.
// Fees are summed as raw amounts — note that fee currency varies by exchange
// (e.g. Bitkub buy fees are in THB, sell fees are in the base asset).
func ComputeAvgCost(trades []ccxt.Trade) PnLResult {
	sort.Slice(trades, func(i, j int) bool {
		if trades[i].Timestamp == nil {
			return true
		}
		if trades[j].Timestamp == nil {
			return false
		}
		return *trades[i].Timestamp < *trades[j].Timestamp
	})

	r := PnLResult{
		TruncatedAt200: len(trades) >= 200,
		TradeCount:     len(trades),
	}
	if len(trades) > 0 {
		r.FirstTs = trades[0].Timestamp
		r.LastTs = trades[len(trades)-1].Timestamp
	}

	var heldQty, heldCost float64

	for _, t := range trades {
		qty := derefF(t.Amount)
		price := derefF(t.Price)
		cost := derefF(t.Cost)
		if cost == 0 && qty > 0 && price > 0 {
			cost = qty * price
		}
		fee := derefF(t.Fee.Cost)
		side := ""
		if t.Side != nil {
			side = *t.Side
		}

		r.Fees += fee

		if side == "sell" {
			r.SoldQty += qty
			r.SoldProceeds += cost

			avgCost := 0.0
			if heldQty > 0 {
				avgCost = heldCost / heldQty
			}
			r.Realized += (price - avgCost) * qty

			if qty >= heldQty {
				heldQty = 0
				heldCost = 0
			} else {
				heldCost -= avgCost * qty
				heldQty -= qty
			}
		} else {
			r.BoughtQty += qty
			r.BoughtCost += cost
			heldCost += cost
			heldQty += qty
		}
	}

	if heldQty > 0 {
		r.Held.Qty = heldQty
		r.Held.AvgCost = heldCost / heldQty
		r.Held.TotalCost = heldCost
	}

	return r
}

func derefF(f *float64) float64 {
	if f == nil {
		return 0
	}
	return *f
}
