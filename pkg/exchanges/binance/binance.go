package binance

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/khunquant/khunquant/pkg/config"
	"github.com/khunquant/khunquant/pkg/exchanges"
)

// Name is the canonical identifier for this exchange.
const Name = "binance"

// BinanceExchange implements exchanges.WalletExchange using the CCXT Go library.
type BinanceExchange struct {
	spot      *ccxt.Binance     // spot / funding / cross-margin
	usdm      *ccxt.Binanceusdm // USDT-M perpetual futures
	coinm     *ccxt.Binancecoinm // Coin-M futures
	isTestnet bool
}

// NewBinanceExchange creates a new BinanceExchange using resolved credentials.
func NewBinanceExchange(creds config.ExchangeAccount, testnet bool) (*BinanceExchange, error) {
	if creds.APIKey == "" || creds.Secret == "" {
		return nil, fmt.Errorf("binance: api_key and secret are required")
	}

	ccxtCreds := map[string]interface{}{
		"apiKey": creds.APIKey,
		"secret": creds.Secret,
	}

	spot := ccxt.NewBinance(ccxtCreds)
	usdm := ccxt.NewBinanceusdm(ccxtCreds)
	coinm := ccxt.NewBinancecoinm(ccxtCreds)

	if testnet {
		spot.SetSandboxMode(true)
		usdm.SetSandboxMode(true)
		coinm.SetSandboxMode(true)
	}

	return &BinanceExchange{
		spot:      spot,
		usdm:      usdm,
		coinm:     coinm,
		isTestnet: testnet,
	}, nil
}

// Name returns the exchange identifier.
func (b *BinanceExchange) Name() string { return Name }

// SupportedWalletTypes returns all wallet types this exchange supports.
func (b *BinanceExchange) SupportedWalletTypes() []string {
	return []string{"spot", "funding", "futures_usdt", "futures_coin", "margin", "earn_flexible", "earn_locked", "earn", "all"}
}

// GetBalances implements the basic Exchange interface (spot only, for backward compat).
func (b *BinanceExchange) GetBalances(ctx context.Context) ([]exchanges.Balance, error) {
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
	if ticker, err := b.spot.FetchTicker(base + "/" + upperQuote); err == nil && ticker.Last != nil {
		return *ticker.Last, nil
	}

	// Fallback: try base/USDT then convert if quote != USDT
	if upperQuote != "USDT" {
		if ticker, err := b.spot.FetchTicker(base + "/USDT"); err == nil && ticker.Last != nil {
			// We have USDT price; if quote is another stablecoin treat as 1:1
			if usdLike[upperQuote] {
				return *ticker.Last, nil
			}
		}
	}

	return 0, fmt.Errorf("binance: cannot determine price for %s in %s", asset, quote)
}

// getAllBalances aggregates balances across all wallet types.
// Individual wallet errors are silently skipped (e.g. futures not enabled).
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
	var all []exchanges.WalletBalance
	for _, wt := range walletTypes {
		wb, err := b.GetWalletBalances(ctx, wt)
		if err != nil {
			continue // wallet not enabled or no access — skip
		}
		all = append(all, filterOutLDTokens(wb)...)
	}
	return all, nil
}

// filterOutLDTokens removes LD-prefixed Simple Earn tokens (e.g. LDBTC) from
// a balance slice. The underlying base asset balance already includes the staked
// amount, so LD tokens must be excluded to avoid double-counting.
func filterOutLDTokens(balances []exchanges.WalletBalance) []exchanges.WalletBalance {
	out := make([]exchanges.WalletBalance, 0, len(balances))
	for _, b := range balances {
		upper := strings.ToUpper(b.Asset)
		if strings.HasPrefix(upper, "LD") && len(upper) > 2 {
			continue
		}
		out = append(out, b)
	}
	return out
}

// getSpotBalances fetches spot wallet balances via CCXT FetchBalance(type=spot).
func (b *BinanceExchange) getSpotBalances(_ context.Context) ([]exchanges.WalletBalance, error) {
	bal, err := b.spot.FetchBalance(map[string]interface{}{"type": "spot"})
	if err != nil {
		return nil, fmt.Errorf("spot: %w", err)
	}
	return walletBalancesFromCCXT(bal, "spot"), nil
}

// getFundingBalances fetches funding wallet balances via CCXT FetchBalance(type=funding).
func (b *BinanceExchange) getFundingBalances(_ context.Context) ([]exchanges.WalletBalance, error) {
	bal, err := b.spot.FetchBalance(map[string]interface{}{"type": "funding"})
	if err != nil {
		return nil, fmt.Errorf("funding: %w", err)
	}
	return walletBalancesFromCCXT(bal, "funding"), nil
}

// getFuturesUSDTBalances fetches USDT-M futures balances via CCXT BinanceUSDM.
func (b *BinanceExchange) getFuturesUSDTBalances(_ context.Context) ([]exchanges.WalletBalance, error) {
	bal, err := b.usdm.FetchBalance()
	if err != nil {
		return nil, fmt.Errorf("futures_usdt: %w", err)
	}
	return walletBalancesFromCCXT(bal, "futures_usdt"), nil
}

// getFuturesCoinBalances fetches Coin-M futures balances via CCXT BinanceCoinM.
func (b *BinanceExchange) getFuturesCoinBalances(_ context.Context) ([]exchanges.WalletBalance, error) {
	bal, err := b.coinm.FetchBalance()
	if err != nil {
		return nil, fmt.Errorf("futures_coin: %w", err)
	}
	return walletBalancesFromCCXT(bal, "futures_coin"), nil
}

// getMarginBalances fetches cross-margin account balances via CCXT FetchBalance(type=margin).
func (b *BinanceExchange) getMarginBalances(_ context.Context) ([]exchanges.WalletBalance, error) {
	bal, err := b.spot.FetchBalance(map[string]interface{}{"type": "margin"})
	if err != nil {
		return nil, fmt.Errorf("margin: %w", err)
	}
	return walletBalancesFromCCXT(bal, "margin"), nil
}

// getEarnFlexibleBalances fetches Simple Earn flexible positions via the raw CCXT Sapi endpoint.
// Paginates automatically (100/page).
func (b *BinanceExchange) getEarnFlexibleBalances(_ context.Context) ([]exchanges.WalletBalance, error) {
	var out []exchanges.WalletBalance
	page := int64(1)

	for {
		params := map[string]interface{}{
			"current": page,
			"size":    100,
		}
		res := <-b.spot.SapiGetSimpleEarnFlexiblePosition(params)
		if ccxt.IsError(res) {
			return nil, fmt.Errorf("earn_flexible: %w", ccxt.CreateReturnError(res))
		}

		resp, ok := res.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("earn_flexible: unexpected response type %T", res)
		}

		rows, _ := resp["rows"].([]interface{})
		total := safeInt64(resp, "total")

		for _, r := range rows {
			row, ok := r.(map[string]interface{})
			if !ok {
				continue
			}
			totalAmount := safeFloat(row, "totalAmount")
			if totalAmount == 0 {
				continue
			}
			asset := safeString(row, "asset")
			extra := map[string]string{
				"apr": safeString(row, "latestAnnualPercentageRate"),
			}
			if v := safeString(row, "cumulativeTotalRewards"); v != "" && v != "0" {
				extra["cumulative_rewards"] = v
			}
			if v := safeString(row, "collateralAmount"); v != "" && v != "0" {
				extra["collateral"] = v
			}
			out = append(out, exchanges.WalletBalance{
				Balance:    exchanges.Balance{Asset: asset, Free: totalAmount},
				WalletType: "earn_flexible",
				Extra:      extra,
			})
		}

		if int64(len(out)) >= total || int64(len(rows)) < 100 {
			break
		}
		page++
	}

	return out, nil
}

// getEarnLockedBalances fetches Simple Earn locked positions via the raw CCXT Sapi endpoint.
// Paginates automatically (100/page).
func (b *BinanceExchange) getEarnLockedBalances(_ context.Context) ([]exchanges.WalletBalance, error) {
	var out []exchanges.WalletBalance
	page := int64(1)

	for {
		params := map[string]interface{}{
			"current": page,
			"size":    100,
		}
		res := <-b.spot.SapiGetSimpleEarnLockedPosition(params)
		if ccxt.IsError(res) {
			return nil, fmt.Errorf("earn_locked: %w", ccxt.CreateReturnError(res))
		}

		resp, ok := res.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("earn_locked: unexpected response type %T", res)
		}

		rows, _ := resp["rows"].([]interface{})
		total := safeInt64(resp, "total")

		for _, r := range rows {
			row, ok := r.(map[string]interface{})
			if !ok {
				continue
			}
			amount := safeFloat(row, "amount")
			if amount == 0 {
				continue
			}
			asset := safeString(row, "asset")
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
			out = append(out, exchanges.WalletBalance{
				Balance:    exchanges.Balance{Asset: asset, Locked: amount},
				WalletType: "earn_locked",
				Extra:      extra,
			})
		}

		if int64(len(out)) >= total || int64(len(rows)) < 100 {
			break
		}
		page++
	}

	return out, nil
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
