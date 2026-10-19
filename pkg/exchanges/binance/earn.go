package binance

// Binance Simple Earn (Flexible) support for the broker.EarnProvider interface.
// These call CCXT implicit (raw) endpoints on the authenticated spot client, since
// CCXT has no unified Earn methods. Responses come back as raw maps.

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// Compile-time guarantee that the adapter satisfies broker.EarnProvider.
var _ broker.EarnProvider = (*BinanceBrokerAdapter)(nil)

// earnAsFloat coerces a CCXT raw value (float64 or numeric string) to float64.
func earnAsFloat(v interface{}) float64 {
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

// earnAsString coerces a CCXT raw value to string.
func earnAsString(v interface{}) string {
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

// earnAsBool coerces a CCXT raw value to bool.
func earnAsBool(v interface{}) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return t == "true" || t == "1"
	}
	return false
}

// earnRows extracts the "rows" array (Binance Simple Earn list/position shape)
// from a raw response map.
func earnRows(res interface{}) []map[string]interface{} {
	m, ok := res.(map[string]interface{})
	if !ok {
		return nil
	}
	raw, ok := m["rows"].([]interface{})
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

// FetchFlexibleEarnProducts returns Binance Simple Earn flexible products. When
// asset == "" all products are returned (paged). APY is normalized to a fraction.
func (a *BinanceBrokerAdapter) FetchFlexibleEarnProducts(_ context.Context, asset string) ([]broker.EarnProduct, error) {
	if err := a.requireAuth(); err != nil {
		return nil, err
	}
	var products []broker.EarnProduct
	err := catchPanic(func() error {
		const size = 100
		for current := 1; ; current++ {
			params := map[string]interface{}{"current": current, "size": size}
			if asset != "" {
				params["asset"] = asset
			}
			res := <-a.spot.SapiGetSimpleEarnFlexibleList(params)
			if res.Err != nil {
				return res.Err
			}
			rows := earnRows(res.Boxed())
			for _, row := range rows {
				products = append(products, broker.EarnProduct{
					Exchange:      Name,
					Asset:         earnAsString(row["asset"]),
					ProductID:     earnAsString(row["productId"]),
					APY:           earnAsFloat(row["latestAnnualPercentageRate"]),
					CanSubscribe:  earnAsBool(row["canPurchase"]),
					AutoSubscribe: earnAsBool(row["autoSubscribe"]),
					MinSubscribe:  earnAsFloat(row["minPurchaseAmount"]),
				})
			}
			if len(rows) < size || asset != "" {
				break
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("binance earn: list products: %w", err)
	}
	return products, nil
}

// FetchFlexibleEarnPositions returns currently held Binance flexible earn positions.
func (a *BinanceBrokerAdapter) FetchFlexibleEarnPositions(_ context.Context) ([]broker.EarnPosition, error) {
	if err := a.requireAuth(); err != nil {
		return nil, err
	}
	var positions []broker.EarnPosition
	err := catchPanic(func() error {
		const size = 100
		for current := 1; ; current++ {
			res := <-a.spot.SapiGetSimpleEarnFlexiblePosition(map[string]interface{}{"current": current, "size": size})
			if res.Err != nil {
				return res.Err
			}
			rows := earnRows(res.Boxed())
			for _, row := range rows {
				positions = append(positions, broker.EarnPosition{
					Exchange:      Name,
					Asset:         earnAsString(row["asset"]),
					ProductID:     earnAsString(row["productId"]),
					Amount:        earnAsFloat(row["totalAmount"]),
					APY:           earnAsFloat(row["latestAnnualPercentageRate"]),
					AutoSubscribe: earnAsBool(row["autoSubscribe"]),
				})
			}
			if len(rows) < size {
				break
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("binance earn: list positions: %w", err)
	}
	return positions, nil
}

// resolveProductID resolves a Binance flexible productId from an asset when the
// caller did not supply one.
func (a *BinanceBrokerAdapter) resolveProductID(ctx context.Context, productID, asset string) (string, error) {
	if productID != "" {
		return productID, nil
	}
	products, err := a.FetchFlexibleEarnProducts(ctx, asset)
	if err != nil {
		return "", err
	}
	for _, p := range products {
		if p.Asset == asset && p.ProductID != "" {
			return p.ProductID, nil
		}
	}
	return "", fmt.Errorf("binance earn: no flexible product found for %s", asset)
}

// SubscribeFlexibleEarn subscribes amount of asset into the flexible product.
func (a *BinanceBrokerAdapter) SubscribeFlexibleEarn(ctx context.Context, productID, asset string, amount float64, autoSubscribe bool) (string, error) {
	if err := a.requireAuth(); err != nil {
		return "", err
	}
	pid, err := a.resolveProductID(ctx, productID, asset)
	if err != nil {
		return "", err
	}
	var txID string
	err = catchPanic(func() error {
		params := map[string]interface{}{
			"productId":     pid,
			"amount":        strconv.FormatFloat(amount, 'f', -1, 64),
			"autoSubscribe": autoSubscribe,
		}
		res := <-a.spot.SapiPostSimpleEarnFlexibleSubscribe(params)
		if res.Err != nil {
			return res.Err
		}
		if m, ok := res.Boxed().(map[string]interface{}); ok {
			txID = earnAsString(m["purchaseId"])
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("binance earn: subscribe: %w", err)
	}
	return txID, nil
}

// RedeemFlexibleEarn redeems amount (or all) of asset from the flexible product.
func (a *BinanceBrokerAdapter) RedeemFlexibleEarn(ctx context.Context, productID, asset string, amount float64, redeemAll bool) (string, error) {
	if err := a.requireAuth(); err != nil {
		return "", err
	}
	pid, err := a.resolveProductID(ctx, productID, asset)
	if err != nil {
		return "", err
	}
	var txID string
	err = catchPanic(func() error {
		params := map[string]interface{}{"productId": pid}
		if redeemAll {
			params["redeemAll"] = true
		} else {
			params["amount"] = strconv.FormatFloat(amount, 'f', -1, 64)
		}
		res := <-a.spot.SapiPostSimpleEarnFlexibleRedeem(params)
		if res.Err != nil {
			return res.Err
		}
		if m, ok := res.Boxed().(map[string]interface{}); ok {
			txID = earnAsString(m["redeemId"])
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("binance earn: redeem: %w", err)
	}
	return txID, nil
}

// SetFlexibleAutoSubscribe enables/disables auto-subscribe for the product/asset.
func (a *BinanceBrokerAdapter) SetFlexibleAutoSubscribe(ctx context.Context, productID, asset string, enable bool) error {
	if err := a.requireAuth(); err != nil {
		return err
	}
	pid, err := a.resolveProductID(ctx, productID, asset)
	if err != nil {
		return err
	}
	err = catchPanic(func() error {
		params := map[string]interface{}{"productId": pid, "autoSubscribe": enable}
		res := <-a.spot.SapiPostSimpleEarnFlexibleSetAutoSubscribe(params)
		if res.Err != nil {
			return res.Err
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("binance earn: set auto-subscribe: %w", err)
	}
	return nil
}

// FetchFlexibleEarnRateHistory returns historical rate data for a flexible earn product.
// Calls /sapi/v1/simple-earn/flexible/history/rateHistory. Requires authentication.
// The response annualPercentageRate is already a fraction (0.05 == 5% APY).
// Points are DAILY (aprPeriod=DAY). The endpoint serves up to a 1-year span and
// 100 rows per page, so multi-month windows are paged via `current`. When `since`
// is set it bounds startTime (clamped to <=1y ago per Binance's max span); else
// the latest `limit` points are returned. Results are cached (EarnRateHistoryTTL).
func (a *BinanceBrokerAdapter) FetchFlexibleEarnRateHistory(ctx context.Context, productID, asset string, since *int64, limit int) ([]broker.EarnRatePoint, error) {
	if err := a.requireAuth(); err != nil {
		return nil, err
	}
	if cached, ok := broker.EarnRateHistoryCacheGet("binance", asset, since); ok {
		return cached, nil
	}
	if productID == "" {
		var err error
		productID, err = a.resolveProductID(ctx, productID, asset)
		if err != nil {
			return nil, err
		}
	}
	var points []broker.EarnRatePoint
	const size = 100
	appendPage := func(params map[string]interface{}) (int, error) {
		res := <-a.spot.SapiGetSimpleEarnFlexibleHistoryRateHistory(params)
		if res.Err != nil {
			return 0, res.Err
		}
		rows := earnRows(res.Boxed())
		for _, row := range rows {
			points = append(points, broker.EarnRatePoint{
				Rate:      earnAsFloat(row["annualPercentageRate"]),
				Timestamp: int64(earnAsFloat(row["time"])),
			})
		}
		return len(rows), nil
	}
	err := catchPanic(func() error {
		if since == nil {
			// Latest-N request: Binance returns the recent ~30d window by default;
			// page via `current` until a short page or the requested limit.
			for page := 1; page <= 20; page++ {
				n, e := appendPage(map[string]interface{}{
					"productId": productID, "aprPeriod": "DAY", "size": size, "current": page,
				})
				if e != nil {
					return e
				}
				if n < size || (limit > 0 && len(points) >= limit) {
					break
				}
			}
			return nil
		}
		// Windowed request: Binance caps the startTime..endTime span (documented
		// variously as 3 months / 1 year), so walk the lookback in ≤89-day chunks —
		// safe under either reading — paging each chunk via `current`.
		const subWindowMs = int64(89) * 24 * 60 * 60 * 1000
		nowMs := time.Now().UnixMilli()
		startBound := nowMs - int64(364)*24*60*60*1000
		if *since > startBound {
			startBound = *since
		}
		for end := nowMs; end > startBound; {
			start := end - subWindowMs
			if start < startBound {
				start = startBound
			}
			for page := 1; page <= 20; page++ {
				n, e := appendPage(map[string]interface{}{
					"productId": productID, "aprPeriod": "DAY", "size": size, "current": page,
					"startTime": start, "endTime": end,
				})
				if e != nil {
					return e
				}
				if n < size {
					break
				}
			}
			end = start - 1
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("binance earn: fetch rate history: %w", err)
	}
	// Trim only latest-N requests; windowed (since) callers filter by timestamp.
	if since == nil && limit > 0 && len(points) > limit {
		points = points[:limit]
	}
	broker.EarnRateHistoryCacheSet("binance", asset, since, points)
	return points, nil
}
