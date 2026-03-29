package okx

import (
	"context"
	"fmt"
	"strings"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/khunquant/khunquant/pkg/config"
	"github.com/khunquant/khunquant/pkg/exchanges"
)

// Name is the canonical identifier for this exchange.
const Name = "okx"

// OKXExchange implements exchanges.WalletExchange using the CCXT Go library.
type OKXExchange struct {
	client    *ccxt.Okx
	isTestnet bool
}

// NewOKXExchange creates a new OKXExchange using resolved credentials.
func NewOKXExchange(creds config.OKXExchangeAccount, testnet bool) (*OKXExchange, error) {
	if creds.APIKey == "" || creds.Secret == "" || creds.Passphrase == "" {
		return nil, fmt.Errorf("okx: api_key, secret, and passphrase are required")
	}

	ccxtCreds := map[string]interface{}{
		"apiKey":   creds.APIKey,
		"secret":   creds.Secret,
		"password": creds.Passphrase,
	}

	client := ccxt.NewOkx(ccxtCreds)

	if testnet {
		client.SetSandboxMode(true)
	}

	if _, err := client.LoadMarkets(); err != nil {
		return nil, fmt.Errorf("okx: load markets: %w", err)
	}

	return &OKXExchange{
		client:    client,
		isTestnet: testnet,
	}, nil
}

// Name returns the exchange identifier.
func (o *OKXExchange) Name() string { return Name }

// SupportedWalletTypes returns all wallet types this exchange supports.
func (o *OKXExchange) SupportedWalletTypes() []string {
	return []string{"trading", "funding", "all"}
}

// GetBalances implements the basic Exchange interface (trading account only).
func (o *OKXExchange) GetBalances(ctx context.Context) ([]exchanges.Balance, error) {
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

	if ticker, err := o.client.FetchTicker(upper + "/" + upperQuote); err == nil && ticker.Last != nil {
		return *ticker.Last, nil
	}

	if upperQuote != "USDT" {
		if ticker, err := o.client.FetchTicker(upper + "/USDT"); err == nil && ticker.Last != nil {
			if usdLike[upperQuote] {
				return *ticker.Last, nil
			}
		}
	}

	return 0, fmt.Errorf("okx: cannot determine price for %s in %s", asset, quote)
}

// getAllBalances aggregates balances across trading and funding wallets.
func (o *OKXExchange) getAllBalances(ctx context.Context) ([]exchanges.WalletBalance, error) {
	var all []exchanges.WalletBalance
	for _, wt := range []string{"trading", "funding"} {
		wb, err := o.GetWalletBalances(ctx, wt)
		if err != nil {
			continue
		}
		all = append(all, wb...)
	}
	return all, nil
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
