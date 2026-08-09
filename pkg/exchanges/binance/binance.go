package binance

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/exchanges"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

// Name is the canonical identifier for this exchange.
const Name = "binance"

// BinanceExchange implements exchanges.WalletExchange using the CCXT Go library.
type BinanceExchange struct {
	spot       *ccxt.Binance      // spot / funding / cross-margin (authenticated)
	usdm       *ccxt.Binanceusdm  // USDT-M perpetual futures (authenticated)
	coinm      *ccxt.Binancecoinm // Coin-M futures (authenticated)
	publicSpot *ccxt.Binance      // credential-free instance for public endpoints
	isTestnet  bool
	hasAuth    bool
}

// NewBinanceExchange creates a new BinanceExchange using resolved credentials.
// If both APIKey and Secret are empty, a public-only instance is created.
// Public endpoints (OHLCV, tickers, order book) always use a credential-free
// CCXT instance so that IP-restricted API keys do not interfere.
func NewBinanceExchange(creds config.ExchangeAccount, testnet bool) (*BinanceExchange, error) {
	hasAuth := creds.APIKey.String() != "" && creds.Secret.String() != ""

	var ccxtCreds map[string]interface{}
	if hasAuth {
		ccxtCreds = map[string]interface{}{
			"apiKey": creds.APIKey.String(),
			"secret": creds.Secret.String(),
		}
	}

	spot := ccxt.NewBinance(ccxtCreds)
	usdm := ccxt.NewBinanceusdm(ccxtCreds)
	coinm := ccxt.NewBinancecoinm(ccxtCreds)
	publicSpot := ccxt.NewBinance(nil) // no credentials — for OHLCV / tickers / order book

	noSymbolWarn := map[string]interface{}{"warnOnFetchOpenOrdersWithoutSymbol": false}
	spot.ExtendExchangeOptions(noSymbolWarn)
	usdm.ExtendExchangeOptions(noSymbolWarn)
	coinm.ExtendExchangeOptions(noSymbolWarn)

	if creds.Proxy != "" {
		isHTTPS := strings.HasPrefix(strings.ToLower(creds.Proxy), "https")
		for _, ex := range []*ccxt.Binance{spot, publicSpot} {
			if isHTTPS {
				ex.HttpsProxy = creds.Proxy
			} else {
				ex.HttpProxy = creds.Proxy
			}
			ex.UpdateProxySettings()
		}
		if isHTTPS {
			usdm.HttpsProxy = creds.Proxy
			coinm.HttpsProxy = creds.Proxy
		} else {
			usdm.HttpProxy = creds.Proxy
			coinm.HttpProxy = creds.Proxy
		}
		usdm.UpdateProxySettings()
		coinm.UpdateProxySettings()
	}

	if testnet {
		spot.SetSandboxMode(true)
		usdm.SetSandboxMode(true)
		coinm.SetSandboxMode(true)
		publicSpot.SetSandboxMode(true)
	}

	// If sandbox mode is enabled, rewrite all URLs to point to the sandbox server.
	isSandboxed, baseURL := sandbox.GlobalState()
	if isSandboxed && baseURL != "" {
		// Rewrite URLs for all exchange instances.
		for _, ex := range []interface{}{spot, usdm, coinm, publicSpot} {
			if err := sandbox.RewriteExchangeURLs(Name, ex, baseURL); err != nil {
				return nil, fmt.Errorf("rewrite URLs for sandbox: %w", err)
			}
			if err := sandbox.VerifyExchangeURLsSandboxed(ex); err != nil {
				return nil, fmt.Errorf("verify sandbox URLs: %w", err)
			}
		}
	}

	return &BinanceExchange{
		spot:       spot,
		usdm:       usdm,
		coinm:      coinm,
		publicSpot: publicSpot,
		isTestnet:  testnet,
		hasAuth:    hasAuth,
	}, nil
}

func (b *BinanceExchange) requireAuth() error {
	if !b.hasAuth {
		return fmt.Errorf("binance: api_key and secret are required for this operation")
	}
	return nil
}

// Name returns the exchange identifier.
func (b *BinanceExchange) Name() string { return Name }

// SupportedWalletTypes returns all wallet types this exchange supports.
func (b *BinanceExchange) SupportedWalletTypes() []string {
	return []string{"spot", "funding", "futures_usdt", "futures_coin", "margin", "earn_flexible", "earn_locked", "earn", "all"}
}

// SupportedQuotes implements exchanges.QuoteLister.
func (b *BinanceExchange) SupportedQuotes() []string {
	return []string{"USDT", "USDC", "BUSD", "FDUSD", "BTC", "ETH", "BNB"}
}

// GetBalances implements the basic Exchange interface (spot only, for backward compat).
func (b *BinanceExchange) GetBalances(ctx context.Context) ([]exchanges.Balance, error) {
	if err := b.requireAuth(); err != nil {
		return nil, err
	}
	wb, err := b.getSpotBalances(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]exchanges.Balance, len(wb))
	for i, w := range wb {
		out[i] = w.Balance
	}
	return out, nil
}

// GetWalletBalances implements WalletExchange.
func (b *BinanceExchange) GetWalletBalances(ctx context.Context, walletType string) ([]exchanges.WalletBalance, error) {
	if err := b.requireAuth(); err != nil {
		return nil, err
	}
	switch walletType {
	case "spot":
		return b.getSpotBalances(ctx)
	case "funding":
		return b.getFundingBalances(ctx)
	case "futures_usdt", "futures":
		return b.getFuturesUSDTBalances(ctx)
	case "futures_coin":
		return b.getFuturesCoinBalances(ctx)
	case "margin":
		return b.getMarginBalances(ctx)
	case "earn_flexible":
		return b.getEarnFlexibleBalances(ctx)
	case "earn_locked":
		return b.getEarnLockedBalances(ctx)
	case "earn":
		return b.getEarnBalances(ctx)
	case "all":
		return b.getAllBalances(ctx)
	default:
		return nil, fmt.Errorf("binance: unsupported wallet type %q (supported: %v)", walletType, b.SupportedWalletTypes())
	}
}

// GetWalletBalancesPartial implements PartialWalletExchange for aggregate ("all") requests,
// returning both successful balances and a list of failed wallet types. For single-wallet
// requests, it delegates to GetWalletBalances.
func (b *BinanceExchange) GetWalletBalancesPartial(ctx context.Context, walletType string) ([]exchanges.WalletBalance, []exchanges.WalletFailure, error) {
	if err := b.requireAuth(); err != nil {
		return nil, nil, err
	}
	if walletType != "all" {
		// For non-aggregate requests, use the standard method and return no failures.
		balances, err := b.GetWalletBalances(ctx, walletType)
		return balances, nil, err
	}
	walletTypes := []string{"spot", "funding", "futures_usdt", "futures_coin", "margin", "earn_flexible", "earn_locked"}
	return collectAllWalletBalancesPartial(ctx, walletTypes, func(ctx context.Context, wt string) ([]exchanges.WalletBalance, error) {
		wb, err := b.GetWalletBalances(ctx, wt)
		if err != nil {
			return nil, err
		}
		return filterOutLDTokens(wb), nil
	})
}

// usdLike is the set of stablecoins treated as 1:1 with USD/USDT for valuation.
var usdLike = map[string]bool{
	"USDT": true, "USDC": true, "BUSD": true, "FDUSD": true,
	"TUSD": true, "DAI": true, "USD": true, "USDP": true, "GUSD": true,
}

// FetchPrice implements PricedExchange.
// It resolves the last-traded price of asset denominated in quote (e.g. "USDT").
// Handles LD-prefixed Binance earn tokens (e.g. LDBTC → BTC).
// Returns (0, nil) when the asset itself is the quote or a USD-equivalent stablecoin.
func (b *BinanceExchange) FetchPrice(_ context.Context, asset, quote string) (float64, error) {
	upper := strings.ToUpper(asset)
	upperQuote := strings.ToUpper(quote)

	// asset == quote or asset is a stablecoin equivalent to quote
	if upper == upperQuote || (usdLike[upperQuote] && usdLike[upper]) {
		return 0, nil // 1:1, caller should treat amount as face value
	}

	// Binance earn LD-prefixed tokens (e.g. LDBTC, LDETH, LDADA) → strip prefix
	base := upper
	if strings.HasPrefix(upper, "LD") && len(upper) > 2 {
		base = upper[2:]
	}

	// Try base/quote (e.g. BTC/USDT)
	if ticker, err := b.publicSpot.FetchTicker(base + "/" + upperQuote); err == nil && ticker.Last != nil {
		return *ticker.Last, nil
	}

	// Fallback: try base/USDT then convert if quote != USDT
	if upperQuote != "USDT" {
		if ticker, err := b.publicSpot.FetchTicker(base + "/USDT"); err == nil && ticker.Last != nil {
			// We have USDT price; if quote is another stablecoin treat as 1:1
			if usdLike[upperQuote] {
				return *ticker.Last, nil
			}
		}
	}

	return 0, fmt.Errorf("binance: cannot determine price for %s in %s", asset, quote)
}

// getAllBalances aggregates balances across all wallet types.
// Individual wallet errors are skipped when at least one wallet returns
// balances (e.g. futures not enabled), but failures are returned when no
// balances are found so API/credential issues do not look like an empty
// portfolio.
//
// LD-prefixed earn tokens (e.g. LDBTC, LDBNB) are stripped from every wallet
// type in this aggregated view. Binance includes Simple Earn Flexible positions
// in the spot balance as both the base asset (BTC) and the LD-wrapped token
// (LDBTC), where the base asset total already embeds the staked amount. Counting
// LDBTC separately would therefore double-count the staked portion.
//
// Requesting earn_flexible or spot directly still returns the raw LD token list.
func (b *BinanceExchange) getAllBalances(ctx context.Context) ([]exchanges.WalletBalance, error) {
	walletTypes := []string{"spot", "funding", "futures_usdt", "futures_coin", "margin", "earn_flexible", "earn_locked"}
	return collectAllWalletBalances(ctx, walletTypes, func(ctx context.Context, wt string) ([]exchanges.WalletBalance, error) {
		wb, err := b.GetWalletBalances(ctx, wt)
		if err != nil {
			return nil, err
		}
		return filterOutLDTokens(wb), nil
	})
}

func collectAllWalletBalances(ctx context.Context, walletTypes []string, fetch func(context.Context, string) ([]exchanges.WalletBalance, error)) ([]exchanges.WalletBalance, error) {
	var all []exchanges.WalletBalance
	var errs []error
	successes := 0
	for _, wt := range walletTypes {
		wb, err := fetch(ctx, wt)
		if err != nil {
			errs = append(errs, compactCCXTError(err))
			continue
		}
		successes++
		all = append(all, wb...)
	}
	if successes == 0 && len(errs) > 0 {
		return nil, fmt.Errorf("binance: all wallet balance fetches failed: %w", errors.Join(errs...))
	}
	if len(all) == 0 && len(errs) > 0 {
		return nil, fmt.Errorf("binance: no wallet balances found and some wallet fetches failed: %w", errors.Join(errs...))
	}
	return all, nil
}

func collectAllWalletBalancesPartial(ctx context.Context, walletTypes []string, fetch func(context.Context, string) ([]exchanges.WalletBalance, error)) ([]exchanges.WalletBalance, []exchanges.WalletFailure, error) {
	var all []exchanges.WalletBalance
	var failures []exchanges.WalletFailure
	var rawErrs []error // for the aggregate error message
	successes := 0
	for _, wt := range walletTypes {
		wb, err := fetch(ctx, wt)
		if err != nil {
			// Store the bare, compacted message in the failure for display
			bareMsg := bareCompactCCXTMessage(err, wt)
			failures = append(failures, exchanges.WalletFailure{
				WalletType: wt,
				Err:        errors.New(bareMsg),
			})
			rawErrs = append(rawErrs, err) // keep raw for aggregate
			continue
		}
		successes++
		all = append(all, wb...)
	}
	if successes == 0 && len(failures) > 0 {
		return nil, failures, fmt.Errorf("binance: all wallet balance fetches failed: %w", errors.Join(rawErrs...))
	}
	if len(all) == 0 && len(failures) > 0 {
		return nil, failures, fmt.Errorf("binance: no wallet balances found and some wallet fetches failed: %w", errors.Join(rawErrs...))
	}
	return all, failures, nil
}

func compactCCXTError(err error) error {
	if err == nil {
		return nil
	}
	return errors.New(compactCCXTMessage(err.Error()))
}

func compactCCXTMessage(msg string) string {
	for _, marker := range []string{"\nStack trace:", "Stack trace:"} {
		if idx := strings.Index(msg, marker); idx >= 0 {
			msg = msg[:idx]
			break
		}
	}
	return strings.TrimSpace(msg)
}

// bareCompactCCXTMessage extracts and humanizes the CCXT error message from a
// potentially wrapped error chain. It unwraps once (if the error is a simple wrap),
// strips the wallet-type prefix (e.g. "funding: "), and humanizes CCXT error class
// names (e.g. "ExchangeNotAvailable" → "exchange not available").
func bareCompactCCXTMessage(err error, walletType string) string {
	if err == nil {
		return ""
	}

	msg := err.Error()

	// Unwrap once to get the bare cause if this is a wrapped error
	if unwrapped := errors.Unwrap(err); unwrapped != nil {
		msg = unwrapped.Error()
	}

	// Remove wallet-type prefix if present (e.g. "funding: " or "spot: ")
	walletPrefix := walletType + ": "
	if strings.HasPrefix(msg, walletPrefix) {
		msg = strings.TrimPrefix(msg, walletPrefix)
	}

	// Humanize CCXT error format: [ccxtError]::[ErrorClass]::[details...]
	// Extract the ErrorClass and convert to human-readable form
	if strings.Contains(msg, "::[") {
		parts := strings.Split(msg, "::")
		if len(parts) >= 2 {
			// parts[0] might be [ccxtError]
			// parts[1] is the error class like [ExchangeNotAvailable]
			errorClass := strings.Trim(parts[1], "[]")
			msg = humanizeErrorClass(errorClass)
		}
	}

	return msg
}

// humanizeErrorClass converts a CamelCase error class name to human-readable form.
// e.g. "ExchangeNotAvailable" → "exchange not available"
func humanizeErrorClass(class string) string {
	if class == "" {
		return "unknown error"
	}

	// Insert spaces before capital letters (except the first one)
	var result strings.Builder
	for i, c := range class {
		if i > 0 && c >= 'A' && c <= 'Z' {
			result.WriteRune(' ')
		}
		result.WriteRune(c)
	}

	// Convert to lowercase
	return strings.ToLower(result.String())
}

// filterOutLDTokens removes LD-prefixed Simple Earn tokens (e.g. LDBTC) from
// a balance slice. The underlying base asset balance already includes the staked
// amount, so LD tokens must be excluded to avoid double-counting.
//
// IMPORTANT: Only filter LD-wrapped tokens when the base asset also exists in
// the balance set. The naive check (string prefix "LD") incorrectly drops real
// tokens like LDO (Lido DAO, a top-50 Binance-listed token). The only reliable
// signal is whether stripping the "LD" prefix leaves an asset that is also
// present in balances.
func filterOutLDTokens(balances []exchanges.WalletBalance) []exchanges.WalletBalance {
	// Build a set of all asset symbols (uppercase) present in balances
	assetSet := make(map[string]bool)
	for _, b := range balances {
		assetSet[strings.ToUpper(b.Asset)] = true
	}

	out := make([]exchanges.WalletBalance, 0, len(balances))
	for _, b := range balances {
		upper := strings.ToUpper(b.Asset)
		// Only filter if:
		// 1. It starts with "LD" and has at least 3 characters (LD + base symbol)
		// 2. The base asset (without "LD" prefix) is also present in balances
		if strings.HasPrefix(upper, "LD") && len(upper) > 2 {
			baseSymbol := upper[2:]
			if assetSet[baseSymbol] {
				// This is an LD-wrapper for a base asset that also appears in balances
				continue
			}
		}
		out = append(out, b)
	}
	return out
}

// getSpotBalances fetches spot wallet balances via CCXT FetchBalance(type=spot).
func (b *BinanceExchange) getSpotBalances(_ context.Context) ([]exchanges.WalletBalance, error) {
	bal, err := b.spot.FetchBalance(map[string]interface{}{"type": "spot"})
	if err != nil {
		return nil, fmt.Errorf("spot: %w", compactCCXTError(err))
	}
	return walletBalancesFromCCXT(bal, "spot"), nil
}

// getFundingBalances fetches funding wallet balances via CCXT FetchBalance(type=funding).
func (b *BinanceExchange) getFundingBalances(_ context.Context) ([]exchanges.WalletBalance, error) {
	bal, err := b.spot.FetchBalance(map[string]interface{}{"type": "funding"})
	if err != nil {
		return nil, fmt.Errorf("funding: %w", compactCCXTError(err))
	}
	return walletBalancesFromCCXT(bal, "funding"), nil
}

// getFuturesUSDTBalances fetches USDT-M futures balances via CCXT BinanceUSDM.
func (b *BinanceExchange) getFuturesUSDTBalances(_ context.Context) ([]exchanges.WalletBalance, error) {
	bal, err := b.usdm.FetchBalance()
	if err != nil {
		return nil, fmt.Errorf("futures_usdt: %w", compactCCXTError(err))
	}
	return walletBalancesFromCCXT(bal, "futures_usdt"), nil
}

// getFuturesCoinBalances fetches Coin-M futures balances via CCXT BinanceCoinM.
func (b *BinanceExchange) getFuturesCoinBalances(_ context.Context) ([]exchanges.WalletBalance, error) {
	bal, err := b.coinm.FetchBalance()
	if err != nil {
		return nil, fmt.Errorf("futures_coin: %w", compactCCXTError(err))
	}
	return walletBalancesFromCCXT(bal, "futures_coin"), nil
}

// getMarginBalances fetches cross-margin account balances via CCXT FetchBalance(type=margin).
func (b *BinanceExchange) getMarginBalances(_ context.Context) ([]exchanges.WalletBalance, error) {
	bal, err := b.spot.FetchBalance(map[string]interface{}{"type": "margin"})
	if err != nil {
		return nil, fmt.Errorf("margin: %w", compactCCXTError(err))
	}
	return walletBalancesFromCCXT(bal, "margin"), nil
}

// paginateEarnPositions is a shared helper for paginating Simple Earn endpoints.
// It correctly terminates pagination based on rows consumed from the server,
// not filtered rows, to avoid infinite loops when pages contain rows that are
// filtered out by the caller.
//
// CRITICAL: Every row that passes the filter is added to the output, even if it
// shares an asset symbol with another row. Users legitimately hold multiple
// positions in the same asset — locked positions with different durations (30d/60d/90d),
// or flexible positions across different products. Deduplicating by asset symbol
// silently loses these real positions.
//
// Duplication of identical pages is detected by comparing page identities (based on
// positionId, productId, or asset+amount). If the current page is identical to the
// previous page, the endpoint is repeating itself and pagination stops.
//
// fetchPage: function to fetch a page of results (params include current, size)
// filterAndExtractAmount: function that extracts the amount and returns it + whether to keep the row
// buildExtra: function to build the Extra fields map from a row
// walletType: wallet type for the result
// errorContext: context for error messages
func (b *BinanceExchange) paginateEarnPositions(
	fetchPage func(params map[string]interface{}) interface{},
	filterAndExtractAmount func(row map[string]interface{}) (float64, bool),
	buildExtra func(row map[string]interface{}) map[string]string,
	walletType, errorContext string,
) ([]exchanges.WalletBalance, error) {
	var out []exchanges.WalletBalance
	page := int64(1)
	rowsConsumed := int64(0) // Tracks actual rows received from server
	const maxPages = 500     // Safety cap to prevent infinite loops on broken endpoints
	const pageSize = int64(100)

	var prevPageIdentities []string // Track previous page's row identities to detect repeats

	for page <= maxPages {
		params := map[string]interface{}{
			"current": page,
			"size":    pageSize,
		}
		res := fetchPage(params)
		if ccxt.IsError(res) {
			return nil, fmt.Errorf("%s: %w", errorContext, compactCCXTError(ccxt.CreateReturnError(res)))
		}

		resp, ok := res.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("%s: unexpected response type %T", errorContext, res)
		}

		rows, _ := resp["rows"].([]interface{})
		total := safeInt64(resp, "total")

		// Track how many rows the server returned (before filtering)
		pageRowCount := int64(len(rows))
		rowsConsumed += pageRowCount

		// Build this page's identity and collect its rows for later addition
		var pageIdentities []string
		var pageRows []exchanges.WalletBalance

		// Process rows and apply caller's filter
		for _, r := range rows {
			row, ok := r.(map[string]interface{})
			if !ok {
				continue
			}
			amount, keep := filterAndExtractAmount(row)
			if !keep {
				continue
			}

			// Build row identity: use positionId (locked) or productId (flexible),
			// fall back to asset+amount to identify unique positions
			rowID := getRowIdentity(row)
			pageIdentities = append(pageIdentities, rowID)

			asset := safeString(row, "asset")
			// Caller determines which field to use via wallet type convention
			var balance exchanges.Balance
			if walletType == "earn_locked" {
				balance = exchanges.Balance{Asset: asset, Locked: amount}
			} else {
				balance = exchanges.Balance{Asset: asset, Free: amount}
			}
			pageRows = append(pageRows, exchanges.WalletBalance{
				Balance:    balance,
				WalletType: walletType,
				Extra:      buildExtra(row),
			})
		}

		// Detect if this page is identical to the previous page (stale endpoint).
		// If so, discard this page's rows and stop — the endpoint is repeating.
		if len(pageIdentities) > 0 && identitiesEqual(prevPageIdentities, pageIdentities) {
			// Page is a duplicate; discard and stop pagination
			break
		}

		// Page is new; add all its rows to output
		out = append(out, pageRows...)
		prevPageIdentities = pageIdentities

		// Termination conditions based on actual rows consumed from server:
		// 1. We've seen as many rows as the server said exist
		// 2. This page returned fewer than page size (end of pagination)
		// 3. This page returned zero rows (shouldn't happen, but safety)
		if rowsConsumed >= total || pageRowCount < pageSize || pageRowCount == 0 {
			break
		}
		page++
	}

	return out, nil
}

// getRowIdentity builds a unique identifier for a row based on position/product IDs
// or asset+amount. Used to detect when pagination endpoints repeat the same page.
func getRowIdentity(row map[string]interface{}) string {
	// Prefer positionId (locked positions)
	if posID, ok := row["positionId"]; ok && posID != nil {
		return fmt.Sprintf("positionId:%v", posID)
	}
	// Fall back to productId (flexible positions)
	if prodID, ok := row["productId"]; ok && prodID != nil {
		return fmt.Sprintf("productId:%v", prodID)
	}
	// Last resort: asset + amount (neither ID present)
	asset := safeString(row, "asset")
	amount := safeFloat(row, "amount")
	if amount == 0 {
		amount = safeFloat(row, "totalAmount")
	}
	return fmt.Sprintf("%s:%v", asset, amount)
}

// identitiesEqual checks if two row identity lists are equal.
func identitiesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// getEarnFlexibleBalances fetches Simple Earn flexible positions via the raw CCXT Sapi endpoint.
// Paginates automatically (100/page).
func (b *BinanceExchange) getEarnFlexibleBalances(_ context.Context) ([]exchanges.WalletBalance, error) {
	return b.paginateEarnPositions(
		func(params map[string]interface{}) interface{} {
			return <-b.spot.SapiGetSimpleEarnFlexiblePosition(params)
		},
		func(row map[string]interface{}) (float64, bool) {
			amount := safeFloat(row, "totalAmount")
			return amount, amount != 0
		},
		func(row map[string]interface{}) map[string]string {
			extra := map[string]string{
				"apr": safeString(row, "latestAnnualPercentageRate"),
			}
			if v := safeString(row, "cumulativeTotalRewards"); v != "" && v != "0" {
				extra["cumulative_rewards"] = v
			}
			if v := safeString(row, "collateralAmount"); v != "" && v != "0" {
				extra["collateral"] = v
			}
			return extra
		},
		"earn_flexible",
		"earn_flexible",
	)
}

// getEarnLockedBalances fetches Simple Earn locked positions via the raw CCXT Sapi endpoint.
// Paginates automatically (100/page).
func (b *BinanceExchange) getEarnLockedBalances(_ context.Context) ([]exchanges.WalletBalance, error) {
	return b.paginateEarnPositions(
		func(params map[string]interface{}) interface{} {
			return <-b.spot.SapiGetSimpleEarnLockedPosition(params)
		},
		func(row map[string]interface{}) (float64, bool) {
			amount := safeFloat(row, "amount")
			return amount, amount != 0
		},
		func(row map[string]interface{}) map[string]string {
			extra := map[string]string{
				"apy":      safeString(row, "APY"),
				"status":   safeString(row, "status"),
				"duration": safeString(row, "duration") + "d",
			}
			if rewardAmt := safeString(row, "rewardAmt"); rewardAmt != "" && rewardAmt != "0" {
				extra["reward"] = rewardAmt + " " + safeString(row, "rewardAsset")
			}
			if canEarly, _ := row["canRedeemEarly"].(bool); canEarly {
				if v := safeString(row, "redeemAmountEarly"); v != "" {
					extra["early_redeem"] = v
				}
			}
			return extra
		},
		"earn_locked",
		"earn_locked",
	)
}

// getEarnBalances returns flexible + locked Simple Earn positions combined.
func (b *BinanceExchange) getEarnBalances(ctx context.Context) ([]exchanges.WalletBalance, error) {
	flex, err := b.getEarnFlexibleBalances(ctx)
	if err != nil {
		return nil, err
	}
	locked, err := b.getEarnLockedBalances(ctx)
	if err != nil {
		return nil, err
	}
	return append(flex, locked...), nil
}

// walletBalancesFromCCXT converts a CCXT Balances result to []exchanges.WalletBalance,
// skipping any currency with zero free and zero used.
func walletBalancesFromCCXT(bal ccxt.Balances, walletType string) []exchanges.WalletBalance {
	var out []exchanges.WalletBalance
	for currency, b := range bal.Balances {
		// skip aggregate/metadata keys
		if strings.ToLower(currency) == currency && !isUpperAsset(currency) {
			continue
		}
		free := derefFloat(b.Free)
		used := derefFloat(b.Used)
		if free == 0 && used == 0 {
			continue
		}
		out = append(out, exchanges.WalletBalance{
			Balance:    exchanges.Balance{Asset: currency, Free: free, Locked: used},
			WalletType: walletType,
		})
	}
	return out
}

// isUpperAsset returns true if the string looks like a currency symbol (all uppercase or alphanumeric).
func isUpperAsset(s string) bool {
	if len(s) == 0 {
		return false
	}
	for _, c := range s {
		if !(c >= 'A' && c <= 'Z') && !(c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

func derefFloat(f *float64) float64 {
	if f == nil {
		return 0
	}
	return *f
}

func safeString(m map[string]interface{}, key string) string {
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	default:
		return fmt.Sprintf("%v", v)
	}
}

func safeFloat(m map[string]interface{}, key string) float64 {
	v, ok := m[key]
	if !ok || v == nil {
		return 0
	}
	switch t := v.(type) {
	case float64:
		return t
	case string:
		f, _ := strconv.ParseFloat(t, 64)
		return f
	}
	return 0
}

func safeInt64(m map[string]interface{}, key string) int64 {
	v, ok := m[key]
	if !ok || v == nil {
		return 0
	}
	switch t := v.(type) {
	case float64:
		return int64(t)
	case int64:
		return t
	case int:
		return int64(t)
	case string:
		n, _ := strconv.ParseInt(t, 10, 64)
		return n
	}
	return 0
}
