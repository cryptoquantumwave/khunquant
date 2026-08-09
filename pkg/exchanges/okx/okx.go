package okx

import (
	"context"
	"errors"
	"fmt"
	"strings"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/exchanges"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

// Name is the canonical identifier for this exchange.
const Name = "okx"

// OKXExchange implements exchanges.WalletExchange using the CCXT Go library.
type OKXExchange struct {
	client       *ccxt.Okx
	publicClient *ccxt.Okx // credential-free instance for public endpoints
	isTestnet    bool
	hasAuth      bool
}

// NewOKXExchange creates a new OKXExchange using resolved credentials.
// If credentials are empty, a public-only instance is created for market data endpoints.
// Public endpoints (OHLCV, tickers, order book) always use a credential-free CCXT
// instance so that IP-restricted API keys do not cause authentication errors.
func NewOKXExchange(creds config.OKXExchangeAccount, testnet bool) (*OKXExchange, error) {
	hasAuth := creds.APIKey.String() != "" && creds.Secret.String() != "" && creds.Passphrase.String() != ""

	var ccxtCreds map[string]interface{}
	if hasAuth {
		ccxtCreds = map[string]interface{}{
			"apiKey":   creds.APIKey.String(),
			"secret":   creds.Secret.String(),
			"password": creds.Passphrase.String(),
		}
	}

	client := ccxt.NewOkx(ccxtCreds)
	publicClient := ccxt.NewOkx(nil)

	if creds.Proxy != "" {
		isHTTPS := strings.HasPrefix(strings.ToLower(creds.Proxy), "https")
		for _, ex := range []*ccxt.Okx{client, publicClient} {
			if isHTTPS {
				ex.HttpsProxy = creds.Proxy
			} else {
				ex.HttpProxy = creds.Proxy
			}
			ex.UpdateProxySettings()
		}
	}

	if testnet {
		client.SetSandboxMode(true)
		publicClient.SetSandboxMode(true)
	}

	// If sandbox mode is enabled, rewrite all URLs to point to the sandbox server.
	// This must happen after SetSandboxMode but before LoadMarkets.
	isSandboxed, baseURL := sandbox.GlobalState()
	if isSandboxed && baseURL != "" {
		// Rewrite URLs for both exchange instances.
		for _, ex := range []interface{}{client, publicClient} {
			if err := sandbox.RewriteExchangeURLs(Name, ex, baseURL); err != nil {
				return nil, fmt.Errorf("rewrite URLs for sandbox: %w", err)
			}
			if err := sandbox.VerifyExchangeURLsSandboxed(ex); err != nil {
				return nil, fmt.Errorf("verify sandbox URLs: %w", err)
			}
		}
	}

	if hasAuth {
		if _, err := client.LoadMarkets(); err != nil {
			return nil, fmt.Errorf("okx: load markets: %w", err)
		}
	}

	return &OKXExchange{
		client:       client,
		publicClient: publicClient,
		isTestnet:    testnet,
		hasAuth:      hasAuth,
	}, nil
}

func (o *OKXExchange) requireAuth() error {
	if !o.hasAuth {
		return fmt.Errorf("okx: api_key, secret, and passphrase are required for this operation")
	}
	return nil
}

// Name returns the exchange identifier.
func (o *OKXExchange) Name() string { return Name }

// SupportedWalletTypes returns all wallet types this exchange supports.
func (o *OKXExchange) SupportedWalletTypes() []string {
	return []string{"trading", "funding", "all"}
}

// CcxtClient returns the underlying authenticated CCXT Okx client. Intended for diagnostic tools only.
func (o *OKXExchange) CcxtClient() *ccxt.Okx { return o.client }

// GetBalances implements the basic Exchange interface (trading account only).
func (o *OKXExchange) GetBalances(ctx context.Context) ([]exchanges.Balance, error) {
	if err := o.requireAuth(); err != nil {
		return nil, err
	}
	wb, err := o.getTradingBalances(ctx)
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
func (o *OKXExchange) GetWalletBalances(ctx context.Context, walletType string) ([]exchanges.WalletBalance, error) {
	if err := o.requireAuth(); err != nil {
		return nil, err
	}
	switch walletType {
	case "trading":
		return o.getTradingBalances(ctx)
	case "funding":
		return o.getFundingBalances(ctx)
	case "all":
		return o.getAllBalances(ctx)
	default:
		return nil, fmt.Errorf("okx: unsupported wallet type %q (supported: %v)", walletType, o.SupportedWalletTypes())
	}
}

// GetWalletBalancesPartial implements PartialWalletExchange for aggregate ("all") requests,
// returning both successful balances and a list of failed wallet types. For single-wallet
// requests, it delegates to GetWalletBalances.
func (o *OKXExchange) GetWalletBalancesPartial(ctx context.Context, walletType string) ([]exchanges.WalletBalance, []exchanges.WalletFailure, error) {
	if err := o.requireAuth(); err != nil {
		return nil, nil, err
	}
	if walletType != "all" {
		// For non-aggregate requests, use the standard method and return no failures.
		balances, err := o.GetWalletBalances(ctx, walletType)
		return balances, nil, err
	}
	return o.getAllBalancesPartial(ctx)
}

// usdLike is the set of stablecoins treated as 1:1 with USD/USDT for valuation.
var usdLike = map[string]bool{
	"USDT": true, "USDC": true, "BUSD": true, "FDUSD": true,
	"TUSD": true, "DAI": true, "USD": true, "USDP": true, "GUSD": true,
}

// SupportedQuotes implements exchanges.QuoteLister.
func (o *OKXExchange) SupportedQuotes() []string {
	return []string{"USDT", "USDC", "BTC", "ETH"}
}

// FetchPrice implements PricedExchange.
func (o *OKXExchange) FetchPrice(_ context.Context, asset, quote string) (float64, error) {
	upper := strings.ToUpper(asset)
	upperQuote := strings.ToUpper(quote)

	if upper == upperQuote || (usdLike[upperQuote] && usdLike[upper]) {
		return 0, nil
	}

	if ticker, err := o.publicClient.FetchTicker(upper + "/" + upperQuote); err == nil && ticker.Last != nil {
		return *ticker.Last, nil
	}

	if upperQuote != "USDT" {
		if ticker, err := o.publicClient.FetchTicker(upper + "/USDT"); err == nil && ticker.Last != nil {
			if usdLike[upperQuote] {
				return *ticker.Last, nil
			}
		}
	}

	return 0, fmt.Errorf("okx: cannot determine price for %s in %s", asset, quote)
}

// getAllBalances aggregates balances across trading and funding wallets.
func (o *OKXExchange) getAllBalances(ctx context.Context) ([]exchanges.WalletBalance, error) {
	balances, _, err := o.getAllBalancesPartial(ctx)
	return balances, err
}

// getAllBalancesPartial is like getAllBalances but also returns failures.
func (o *OKXExchange) getAllBalancesPartial(ctx context.Context) ([]exchanges.WalletBalance, []exchanges.WalletFailure, error) {
	var all []exchanges.WalletBalance
	var failures []exchanges.WalletFailure
	var rawErrs []error // for the aggregate error message
	successes := 0
	for _, wt := range []string{"trading", "funding"} {
		wb, err := o.GetWalletBalances(ctx, wt)
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
		return nil, failures, fmt.Errorf("okx: all wallet balance fetches failed: %w", errors.Join(rawErrs...))
	}
	return all, failures, nil
}

// getTradingBalances fetches the OKX trading (spot) account balances.
func (o *OKXExchange) getTradingBalances(_ context.Context) ([]exchanges.WalletBalance, error) {
	bal, err := o.client.FetchBalance(map[string]interface{}{"type": "trading"})
	if err != nil {
		return nil, fmt.Errorf("trading: %w", err)
	}
	return walletBalancesFromCCXT(bal, "trading"), nil
}

// getFundingBalances fetches the OKX funding account balances.
func (o *OKXExchange) getFundingBalances(_ context.Context) ([]exchanges.WalletBalance, error) {
	bal, err := o.client.FetchBalance(map[string]interface{}{"type": "funding"})
	if err != nil {
		return nil, fmt.Errorf("funding: %w", err)
	}
	return walletBalancesFromCCXT(bal, "funding"), nil
}

// walletBalancesFromCCXT converts a CCXT Balances result to []exchanges.WalletBalance,
// skipping any currency with zero free and zero used.
func walletBalancesFromCCXT(bal ccxt.Balances, walletType string) []exchanges.WalletBalance {
	var out []exchanges.WalletBalance
	for currency, b := range bal.Balances {
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

	// Remove wallet-type prefix if present (e.g. "funding: " or "trading: ")
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
