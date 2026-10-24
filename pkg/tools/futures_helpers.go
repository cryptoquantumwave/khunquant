package tools

import (
	"context"
	"fmt"
	"math"
	"strings"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// mergeOrderFillData merges fill and fee data from a CreateOrder response and a
// subsequent FetchOrder response into a single ccxt.Order for leg recording.
//
// Motivation: OKX CreateOrder returns only an order ID; Filled/Average/Cost and
// Fee arrive only in the FetchOrder response. Binance CreateOrder (FULL response)
// carries fill data and commission in fills[]; the subsequent FetchOrder
// (GET /api/v3/order or GET /fapi/v1/order) does NOT return commission — so
// unconditionally overwriting with the fetched order discards the only fee source
// on Binance. This helper merges both objects so neither source is lost.
//
// Rules:
//   - Fill fields (Filled, Average, Cost, Status): prefer fetched when populated.
//   - Fee.Cost: keep whichever is non-nil. When both are populated, prefer fetched
//     (OKX: fetched reflects actual post-fill fee). When only create has fee
//     (Binance FULL response), it is preserved.
func mergeOrderFillData(create, fetched ccxt.Order) ccxt.Order {
	out := create // start from create; preserves order ID, side, and all other fields
	if fetched.Filled != nil && *fetched.Filled > 0 {
		out.Filled = fetched.Filled
	}
	if fetched.Average != nil && *fetched.Average > 0 {
		out.Average = fetched.Average
	}
	if fetched.Cost != nil && *fetched.Cost > 0 {
		out.Cost = fetched.Cost
	}
	if fetched.Status != nil {
		out.Status = fetched.Status
	}
	// Fee: prefer fetched when it carries data (OKX); else keep create's fee (Binance).
	if fetched.Fee.Cost != nil {
		out.Fee = fetched.Fee
	}
	return out
}

// spotFeeCostUSDT returns a positive USDT fee amount from a ccxt.Order for a spot leg.
// The conversion is side-dependent because exchanges charge spot fees in different
// currencies depending on order direction:
//
//	buy  — fee deducted from received base tokens; Fee.Cost is in base currency
//	       → multiply by fillPrice to get USDT
//	sell — fee deducted from received quote (USDT); Fee.Cost is already in USDT
//	       → no conversion needed
//
// Returns 0 when the order carries no fee data or fillPrice is zero.
// Callers should negate the result before storing (fees are costs; stored negative).
func spotFeeCostUSDT(order ccxt.Order, side string, fillPrice float64) float64 {
	if order.Fee.Cost == nil || *order.Fee.Cost <= 0 {
		return 0
	}
	if strings.EqualFold(side, "buy") {
		if fillPrice <= 0 {
			return 0
		}
		return *order.Fee.Cost * fillPrice // base token fee → USDT
	}
	return *order.Fee.Cost // sell: fee already in USDT
}

// futuresContractSize returns the base-currency size of one contract for the
// market, falling back to 1 when the exchange reports none or a non-positive value.
func futuresContractSize(m ccxt.MarketInterface) float64 {
	if m.ContractSize != nil && *m.ContractSize > 0 {
		return *m.ContractSize
	}
	return 1.0
}

// estimateFuturesNotional returns (notional, source, error). amount is a CONTRACT
// count, so notional = amount * contractSize * price. A non-positive contractSize
// is treated as 1.
// Priority: explicit price → FetchFuturesMarkPrice → FetchFuturesFundingRate.MarkPrice.
// source is "explicit", "mark", or "funding_mark".
func estimateFuturesNotional(ctx context.Context, fp broker.FuturesProvider, symbol string, amount, contractSize float64, price *float64) (float64, string, error) {
	if contractSize <= 0 {
		contractSize = 1.0
	}
	baseQty := amount * contractSize
	if price != nil && *price > 0 {
		return baseQty * (*price), "explicit", nil
	}
	if mark, err := fp.FetchFuturesMarkPrice(ctx, symbol); err == nil && mark > 0 {
		return baseQty * mark, "mark", nil
	}
	if rate, err := fp.FetchFuturesFundingRate(ctx, symbol); err == nil && rate.MarkPrice != nil && *rate.MarkPrice > 0 {
		return baseQty * (*rate.MarkPrice), "funding_mark", nil
	}
	return 0, "", fmt.Errorf("cannot estimate notional for %s: mark price and funding rate unavailable", symbol)
}

// validateActiveSwapMarket loads futures markets and validates the symbol is
// an active, linear perpetual swap. Also enforces per-market leverage ceiling.
func validateActiveSwapMarket(ctx context.Context, fp broker.FuturesProvider, symbol string, leverage int64) (ccxt.MarketInterface, error) {
	markets, err := fp.LoadFuturesMarkets(ctx)
	if err != nil {
		return ccxt.MarketInterface{}, fmt.Errorf("cannot load futures markets: %w", err)
	}
	m, ok := markets[symbol]
	if !ok {
		return ccxt.MarketInterface{}, fmt.Errorf("symbol %q not found in futures market catalogue", symbol)
	}
	if m.Active != nil && !*m.Active {
		return ccxt.MarketInterface{}, fmt.Errorf("symbol %q is not active", symbol)
	}
	if m.Swap == nil || !*m.Swap {
		t := "unknown"
		if m.Type != nil {
			t = *m.Type
		}
		return ccxt.MarketInterface{}, fmt.Errorf("symbol %q is not a perpetual swap (type=%s); use a CCXT contract symbol e.g. BTC/USDT:USDT", symbol, t)
	}
	if leverage > 0 && m.Limits.Leverage.Max != nil {
		maxLev := int64(*m.Limits.Leverage.Max)
		if maxLev > 0 && leverage > maxLev {
			return ccxt.MarketInterface{}, fmt.Errorf("leverage %d exceeds market maximum %d for %s", leverage, maxLev, symbol)
		}
	}
	return m, nil
}

// contractsFromNotional converts a USD notional value to the number of contracts
// for a given market. Returns the contract count rounded DOWN to the market's
// minimum amount step. contractSize is the base-currency value per contract
// (e.g. 0.01 BTC/contract for BTC/USDT:USDT on OKX).
//
// Rounding down (under-hedge) is intentional for delta-neutral strategies:
// a small long residual on the spot side is safer than a small short residual,
// because spot losses are bounded at 100% while a naked short is not.
func contractsFromNotional(notionalUSD, markPrice, contractSize, minAmount float64) (float64, error) {
	if markPrice <= 0 {
		return 0, fmt.Errorf("mark price must be positive")
	}
	if contractSize <= 0 {
		contractSize = 1 // fall back: treat 1 unit = 1 contract
	}
	usdPerContract := contractSize * markPrice
	contracts := notionalUSD / usdPerContract
	if minAmount <= 0 {
		minAmount = 1
	}
	// Round DOWN to nearest step of minAmount (under-hedge: prefer long residual)
	steps := math.Floor(contracts / minAmount)
	rounded := steps * minAmount
	return rounded, nil
}

// verifyFuturesFill re-fetches a futures order to detect partial fills.
// Returns (filled, status, isPartial, err).
func verifyFuturesFill(ctx context.Context, fp broker.FuturesProvider, id, symbol string, requestedAmount float64) (float64, string, bool, error) {
	order, err := fp.FetchFuturesOrder(ctx, id, symbol)
	if err != nil {
		return 0, "", false, fmt.Errorf("fill verification: %w", err)
	}
	var filled float64
	if order.Filled != nil {
		filled = *order.Filled
	}
	status := "unknown"
	if order.Status != nil {
		status = *order.Status
	}
	partial := filled > 0 && requestedAmount > 0 && math.Abs(filled-requestedAmount)/requestedAmount > 1e-6
	return filled, status, partial, nil
}

// marginHealthFromPosition computes margin health from a ccxt.Position.
// Returns (liquidationDistancePct, marginRatioPct, label).
// label: "safe" (dist>20%), "warn" (dist>5%), "critical" (dist<=5%), or "unknown".
func marginHealthFromPosition(p ccxt.Position) (distPct, marginRatioPct float64, label string) {
	if p.MarkPrice != nil && p.LiquidationPrice != nil && *p.MarkPrice > 0 {
		dist := math.Abs(*p.MarkPrice-*p.LiquidationPrice) / *p.MarkPrice * 100
		distPct = dist
		switch {
		case dist > 20:
			label = "safe"
		case dist > 5:
			label = "warn"
		default:
			label = "critical"
		}
	}
	if p.MarginRatio != nil && label == "" {
		marginRatioPct = *p.MarginRatio * 100
		switch {
		case marginRatioPct < 50:
			label = "safe"
		case marginRatioPct < 80:
			label = "warn"
		default:
			label = "critical"
		}
	} else if p.MarginRatio != nil {
		marginRatioPct = *p.MarginRatio * 100
	}
	if label == "" {
		label = "unknown"
	}
	return
}
