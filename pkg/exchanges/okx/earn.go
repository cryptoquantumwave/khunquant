package okx

// OKX Simple Earn (Flexible savings) support for the broker.EarnProvider interface.
// CCXT has no unified Earn methods, so these call CCXT implicit (raw) endpoints on
// the OKX client. OKX savings are keyed by currency (ccy), not a product id.
// Responses follow OKX's {"code":"0","data":[...]} envelope.

import (
	"context"
	"fmt"
	"strconv"

	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// Compile-time guarantee that the adapter satisfies broker.EarnProvider.
var _ broker.EarnProvider = (*OKXBrokerAdapter)(nil)

// okxFloat coerces a CCXT raw value (float64 or numeric string) to float64.
func okxFloat(v interface{}) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case int:
		return float64(t)
	case int64:
		return float64(t)
	case string:
		f, _ := strconv.ParseFloat(t, 64)
		return f
	}
	return 0
}

// okxString coerces a CCXT raw value to string.
func okxString(v interface{}) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case nil:
		return ""
	default:
		return fmt.Sprint(t)
	}
}

// okxData extracts the "data" array from an OKX raw response envelope.
func okxData(res interface{}) []map[string]interface{} {
	m, ok := res.(map[string]interface{})
	if !ok {
		return nil
	}
	raw, ok := m["data"].([]interface{})
	if !ok {
		return nil
	}
	out := make([]map[string]interface{}, 0, len(raw))
	for _, r := range raw {
		if rm, ok := r.(map[string]interface{}); ok {
			out = append(out, rm)
		}
	}
	return out
}

// --- broker.EarnProvider ---

// FetchFlexibleEarnProducts returns OKX earn products from two sources:
//  1. Simple Earn Flexible / Savings (public): /api/v5/finance/savings/lending-rate-history
//  2. On-chain Earn / DeFi (requires auth): /api/v5/finance/staking-defi/offers
//
// asset == "" returns all currencies. APY is already a fraction (0.05 == 5%).
// We use lending-rate-history (lendingRate field) rather than lending-rate-summary
// (estRate) because estRate is the borrowing-demand threshold (~2x actual), while
// lendingRate is the rate actually settled and paid to lenders — matching what
// the OKX app displays as "past hour APR".
func (a *OKXBrokerAdapter) FetchFlexibleEarnProducts(_ context.Context, asset string) ([]broker.EarnProduct, error) {
	var products []broker.EarnProduct

	// ── Source 1: Savings / Simple Earn Flexible (public endpoint) ────────
	// Use lending-rate-history with limit=1 to get the most recent settled
	// lendingRate for each currency. When asset=="", OKX returns all currencies
	// for the latest settlement period regardless of the limit parameter.
	_ = catchPanic(func() error {
		params := map[string]interface{}{
			"limit": "1",
		}
		if asset != "" {
			params["ccy"] = asset
		}
		res := <-a.publicClient.PublicGetFinanceSavingsLendingRateHistory(params)
		if res.Err != nil {
			return res.Err
		}
		for _, row := range okxData(res.Boxed()) {
			products = append(products, broker.EarnProduct{
				Exchange:     Name,
				Asset:        okxString(row["ccy"]),
				ProductID:    okxString(row["ccy"]),
				APY:          okxFloat(row["lendingRate"]),
				CanSubscribe: true,
				Type:         "savings",
			})
		}
		return nil
	})

	// ── Source 2: On-chain Earn / DeFi offers (requires auth) ─────────────
	// Gracefully skipped when API keys are absent. Only purchasable offers included.
	if a.hasAuth {
		_ = catchPanic(func() error {
			params := map[string]interface{}{}
			if asset != "" {
				params["ccy"] = asset
			}
			res := <-a.client.PrivateGetFinanceStakingDefiOffers(params)
			if res.Err != nil {
				return nil // supplemental: ignore auth/permission errors
			}
			for _, row := range okxData(res.Boxed()) {
				if okxString(row["state"]) != "purchasable" {
					continue
				}
				ccy := okxString(row["ccy"])
				minAmt := 0.0
				if ivArr, ok := row["investData"].([]interface{}); ok {
					for _, iv := range ivArr {
						if ivm, ok := iv.(map[string]interface{}); ok {
							if okxString(ivm["ccy"]) == ccy {
								minAmt = okxFloat(ivm["minAmt"])
							}
						}
					}
				}
				products = append(products, broker.EarnProduct{
					Exchange:     Name,
					Asset:        ccy,
					ProductID:    okxString(row["productId"]),
					APY:          okxFloat(row["apy"]),
					MinSubscribe: minAmt,
					CanSubscribe: true,
					Type:         "staking-defi",
					Protocol:     okxString(row["protocol"]),
				})
			}
			return nil
		})
	}

	return products, nil
}

// FetchFlexibleEarnPositions returns currently held OKX earn balances across all products.
//
// Four sources are queried and merged; duplicates (same asset) are skipped:
//  1. /api/v5/finance/savings/balance        — Simple Earn Flexible / Savings lending pool
//  2. /api/v5/account/balance details.frozenBal — UTA trading account frozen amounts
//  3. /api/v5/asset/balances frozenBal        — Funding account frozen amounts
//  4. /api/v5/finance/staking-defi/orders-active — On-chain Earn / DeFi positions
func (a *OKXBrokerAdapter) FetchFlexibleEarnPositions(_ context.Context) ([]broker.EarnPosition, error) {
	if err := a.requireAuth(); err != nil {
		return nil, err
	}
	var positions []broker.EarnPosition

	// Helper: check if an asset is already in positions.
	has := func(asset string) bool {
		for _, p := range positions {
			if p.Asset == asset {
				return true
			}
		}
		return false
	}

	// ── Source 1: old Savings / Simple Earn lending pool ─────────────────
	_ = catchPanic(func() error {
		res := <-a.client.PrivateGetFinanceSavingsBalance(map[string]interface{}{})
		if res.Err != nil {
			return res.Err
		}
		for _, row := range okxData(res.Boxed()) {
			asset := okxString(row["ccy"])
			if !has(asset) {
				positions = append(positions, broker.EarnPosition{
					Exchange:  Name,
					Asset:     asset,
					ProductID: asset,
					Amount:    okxFloat(row["amt"]),
					APY:       okxFloat(row["earningRate"]),
				})
			}
		}
		return nil
	})

	// ── Source 2: trading account frozenBal (UTA — Simple Earn locks assets here) ──
	// OKX Unified Trade Account shows earn-locked assets as frozenBal in account/balance.
	// cashBal = freely tradable; frozenBal = locked in earn/orders.
	// We add frozenBal only when the asset isn't already counted from savings.
	_ = catchPanic(func() error {
		res := <-a.client.PrivateGetAccountBalance(map[string]interface{}{})
		if res.Err != nil {
			return nil // supplemental: ignore error
		}
		m, ok := res.Boxed().(map[string]interface{})
		if !ok {
			return nil
		}
		dataArr, _ := m["data"].([]interface{})
		if len(dataArr) == 0 {
			return nil
		}
		acct, _ := dataArr[0].(map[string]interface{})
		details, _ := acct["details"].([]interface{})
		for _, d := range details {
			dm, _ := d.(map[string]interface{})
			asset := okxString(dm["ccy"])
			frozen := okxFloat(dm["frozenBal"])
			if frozen > 0 && !has(asset) {
				positions = append(positions, broker.EarnPosition{
					Exchange:  Name,
					Asset:     asset,
					ProductID: asset + ":trading:frozen",
					Amount:    frozen,
				})
			}
		}
		return nil
	})

	// ── Source 3: funding account frozenBal ───────────────────────────────
	// OKX Simple Earn Flexible draws from the funding account; the subscribed
	// amount appears as frozenBal in /api/v5/asset/balances.
	_ = catchPanic(func() error {
		res := <-a.client.PrivateGetAssetBalances(map[string]interface{}{})
		if res.Err != nil {
			return nil // supplemental: ignore error
		}
		for _, row := range okxData(res.Boxed()) {
			asset := okxString(row["ccy"])
			frozen := okxFloat(row["frozenBal"])
			if frozen > 0 && !has(asset) {
				positions = append(positions, broker.EarnPosition{
					Exchange:  Name,
					Asset:     asset,
					ProductID: asset + ":funding:frozen",
					Amount:    frozen,
				})
			}
		}
		return nil
	})

	// ── Source 4: On-chain Earn / DeFi (staking-defi) ────────────────────
	// Assets subscribed to OKX "On-chain Earn" (e.g. CHZ) appear here.
	// The subscribed amount is nested: investData[i].amt (not a top-level field).
	// State values: 8=Pending, 13=Cancelling, 9=Onchain, 1=Earning, 2=Redeeming.
	_ = catchPanic(func() error {
		res := <-a.client.PrivateGetFinanceStakingDefiOrdersActive(map[string]interface{}{})
		if res.Err != nil {
			return nil // supplemental: ignore error
		}
		for _, row := range okxData(res.Boxed()) {
			asset := okxString(row["ccy"])
			state := okxString(row["state"])
			// Skip terminal/exit states (2=Redeeming, 13=Cancelling); count all others.
			// Active states: 8=Pending, 9=Onchain, 1=Earning.
			if state == "2" || state == "13" {
				continue
			}
			// Sum investData entries whose ccy matches the order currency
			var amt float64
			if ivArr, ok := row["investData"].([]interface{}); ok {
				for _, iv := range ivArr {
					if ivm, ok := iv.(map[string]interface{}); ok {
						if okxString(ivm["ccy"]) == asset {
							amt += okxFloat(ivm["amt"])
						}
					}
				}
			}
			apy := okxFloat(row["apy"])
			ordID := okxString(row["ordId"])
			if amt > 0 && !has(asset) {
				positions = append(positions, broker.EarnPosition{
					Exchange:  Name,
					Asset:     asset,
					ProductID: ordID + ":staking-defi",
					Amount:    amt,
					APY:       apy,
				})
			}
		}
		return nil
	})

	return positions, nil
}

// purchaseRedempt issues an OKX savings purchase or redemption for a currency.
// side is "purchase" or "redempt".
func (a *OKXBrokerAdapter) purchaseRedempt(asset string, amount float64, side string) (string, error) {
	var txID string
	err := catchPanic(func() error {
		params := map[string]interface{}{
			"ccy":  asset,
			"amt":  strconv.FormatFloat(amount, 'f', -1, 64),
			"side": side,
		}
		res := <-a.client.PrivatePostFinanceSavingsPurchaseRedempt(params)
		if res.Err != nil {
			return res.Err
		}
		for _, row := range okxData(res.Boxed()) {
			txID = okxString(row["ccy"]) + ":" + okxString(row["side"])
		}
		return nil
	})
	return txID, err
}

// SubscribeFlexibleEarn purchases amount of asset into OKX flexible savings.
// OKX savings draw from the Funding account, so this first transfers from the
// trading account if needed (best-effort; ignored if already in funding).
func (a *OKXBrokerAdapter) SubscribeFlexibleEarn(_ context.Context, _ /*productID*/ string, asset string, amount float64, _ bool) (string, error) {
	if err := a.requireAuth(); err != nil {
		return "", err
	}
	// Best-effort move from trading -> funding; OKX savings subscribe from funding.
	_ = catchPanic(func() error {
		_, e := a.client.Transfer(asset, amount, "trading", "funding")
		return e
	})
	txID, err := a.purchaseRedempt(asset, amount, "purchase")
	if err != nil {
		return "", fmt.Errorf("okx earn: subscribe: %w", err)
	}
	return txID, nil
}

// RedeemFlexibleEarn redeems amount of asset from OKX flexible savings. OKX has no
// "redeem all" flag, so redeemAll requires the caller to pass the full amount.
func (a *OKXBrokerAdapter) RedeemFlexibleEarn(_ context.Context, _ /*productID*/ string, asset string, amount float64, redeemAll bool) (string, error) {
	if err := a.requireAuth(); err != nil {
		return "", err
	}
	if redeemAll && amount <= 0 {
		return "", fmt.Errorf("okx earn: redeem: OKX requires an explicit amount (no redeem-all flag); pass the held amount")
	}
	txID, err := a.purchaseRedempt(asset, amount, "redempt")
	if err != nil {
		return "", fmt.Errorf("okx earn: redeem: %w", err)
	}
	return txID, nil
}

// SetFlexibleAutoSubscribe is not exposed by the OKX API. OKX "Default Subscribe"
// (auto-earn) is configured in the OKX app, not via a per-currency API toggle.
func (a *OKXBrokerAdapter) SetFlexibleAutoSubscribe(_ context.Context, _ /*productID*/ string, _ string, _ bool) error {
	return fmt.Errorf("okx earn: auto-subscribe is not configurable via API — enable 'Default Subscribe' in the OKX app")
}

// FetchFlexibleEarnRateHistory returns historical rate data for a flexible savings currency.
// Calls /api/v5/finance/savings/lending-rate-history (PUBLIC endpoint).
// Uses lendingRate (actual settled rate paid to lenders), not rate (borrowing threshold).
// Already a fraction (0.05 == 5% APY). productID is ignored for OKX (keyed by currency).
//
// The endpoint returns HOURLY points, 100 per page (max), newest-first. To cover
// multi-month windows it is paged backward via `after` (returns records earlier
// than the cursor ts). When `since` is set we page until reaching it; otherwise
// we return the most-recent `limit` points (single page when limit<=100). Results
// are cached (broker.EarnRateHistoryTTL) since a full-year pull is ~88 requests.
func (a *OKXBrokerAdapter) FetchFlexibleEarnRateHistory(_ context.Context, _ /*productID*/ string, asset string, since *int64, limit int) ([]broker.EarnRatePoint, error) {
	if cached, ok := broker.EarnRateHistoryCacheGet("okx", asset, since); ok {
		return cached, nil
	}
	var points []broker.EarnRatePoint
	err := catchPanic(func() error {
		// Safety cap: 400 pages * 100 = 40k hourly points (~4.5y) — far past any
		// window or OKX's retention, so the since/short-page conditions end first.
		const pageCap = 400
		var cursor string // OKX `after` cursor (ms); empty => latest page
		for page := 0; page < pageCap; page++ {
			params := map[string]interface{}{"ccy": asset, "limit": "100"}
			if cursor != "" {
				params["after"] = cursor
			}
			res := <-a.publicClient.PublicGetFinanceSavingsLendingRateHistory(params)
			if res.Err != nil {
				return res.Err
			}
			rows := okxData(res.Boxed())
			if len(rows) == 0 {
				break
			}
			oldestTs := int64(1) << 62
			for _, row := range rows {
				ts := int64(okxFloat(row["ts"]))
				if ts < oldestTs {
					oldestTs = ts
				}
				points = append(points, broker.EarnRatePoint{
					Rate:      okxFloat(row["lendingRate"]),
					Timestamp: ts,
				})
			}
			// Stop once we've paged past the requested lookback, on the final
			// (short) page, or — for latest-N requests — once we have enough.
			if since != nil && oldestTs <= *since {
				break
			}
			if len(rows) < 100 {
				break
			}
			if since == nil && limit > 0 && len(points) >= limit {
				break
			}
			cursor = strconv.FormatInt(oldestTs, 10)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("okx earn: fetch rate history: %w", err)
	}
	// Trim only latest-N requests; windowed (since) callers filter by timestamp.
	if since == nil && limit > 0 && len(points) > limit {
		points = points[:limit]
	}
	broker.EarnRateHistoryCacheSet("okx", asset, since, points)
	return points, nil
}
