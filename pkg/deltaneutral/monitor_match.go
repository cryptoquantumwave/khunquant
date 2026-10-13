package deltaneutral

import (
	"context"
	"math"
	"sort"
	"strings"

	ccxt "github.com/ccxt/ccxt/go/v4"
)

// normalSide reads a plan's or a position's side as "long" or "short"; anything else
// (including an empty side) is "".
func normalSide(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "short", "sell":
		return "short"
	case "long", "buy":
		return "long"
	}
	return ""
}

// matchPosition picks the position of the plan's symbol and side out of what the
// exchange returned. A position that names a different symbol or the opposite side
// is not the plan's, however first it comes; one that names neither (an adapter that
// leaves the field out of a response already filtered by symbol) is accepted, but an
// explicit match beats it. With several explicit matches the first wins.
func matchPosition(positions []ccxt.Position, plan Plan) (ccxt.Position, bool) {
	wantSide := normalSide(plan.FuturesSide)
	best, bestScore := ccxt.Position{}, -1
	for _, p := range positions {
		score := 0
		if p.Symbol != nil && strings.TrimSpace(*p.Symbol) != "" {
			if !strings.EqualFold(strings.TrimSpace(*p.Symbol), plan.FuturesSymbol) {
				continue
			}
			score++
		}
		if p.Side != nil {
			if side := normalSide(*p.Side); side != "" && wantSide != "" {
				if side != wantSide {
					continue
				}
				score++
			}
		}
		if score > bestScore {
			best, bestScore = p, score
		}
	}
	return best, bestScore >= 0
}

// RecentFundingRates returns the rates of the last n funding settlements in history,
// oldest first, as classifyFunding reads them. Records without a timestamp or a
// finite rate are left out; the order of history does not matter. n <= 0 gives nil.
func RecentFundingRates(history []ccxt.FundingRateHistory, n int) []float64 {
	if n <= 0 {
		return nil
	}
	type point struct {
		ts   int64
		rate float64
	}
	pts := make([]point, 0, len(history))
	for _, h := range history {
		if h.Timestamp == nil || h.FundingRate == nil || math.IsNaN(*h.FundingRate) || math.IsInf(*h.FundingRate, 0) {
			continue
		}
		pts = append(pts, point{*h.Timestamp, *h.FundingRate})
	}
	sort.SliceStable(pts, func(i, j int) bool { return pts[i].ts < pts[j].ts })
	if len(pts) > n {
		pts = pts[len(pts)-n:]
	}
	out := make([]float64, len(pts))
	for i, p := range pts {
		out[i] = p.rate
	}
	return out
}

// planPnL is the plan's profit and loss so far: the net unrealized result of both legs
// (the futures unrealized PnL plus the spot value against the spot notional the plan
// was opened with, which should be about zero for a hedge) plus what the plan has
// booked in funding and fees. It is unknown when no source of the booked amount is
// set or has none, or a leg could not be read.
func (m *Monitor) planPnL(ctx context.Context, plan Plan, spot SpotState, fut FuturesState) (float64, bool) {
	if m.Booked == nil || !spot.Available || !fut.Available || plan.SpotNotionalUSDT <= 0 {
		return 0, false
	}
	booked, ok := m.Booked(ctx, plan.ID)
	if !ok || !finiteNum(booked) {
		return 0, false
	}
	pnl := fut.UnrealizedPnLUSDT + (spot.ValueUSDT - plan.SpotNotionalUSDT) + booked
	if !finiteNum(pnl) {
		return 0, false
	}
	return pnl, true
}
