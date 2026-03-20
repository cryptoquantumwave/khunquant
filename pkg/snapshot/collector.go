package snapshot

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/khunquant/khunquant/pkg/config"
	"github.com/khunquant/khunquant/pkg/exchanges"
)

// CollectOptions controls which exchanges/accounts to snapshot.
type CollectOptions struct {
	Source  string // specific source, or "" for all
	Account string
	Quote   string // default "USDT"
	Label   string
	Note    string
}

// exchangeAccount pairs an exchange name with an account name for iteration.
type exchangeAccount struct {
	exchange string
	account  string
}

// CollectResult holds the assembled snapshot and any per-account errors
// that occurred during collection.
type CollectResult struct {
	Snapshot *Snapshot
	Errors   []string // per-account errors (e.g. "binance/main: connection refused")
}

// CollectFromExchanges gathers balances from configured exchanges and
// assembles a Snapshot ready to be saved. Errors from individual accounts
// are collected in CollectResult.Errors so the caller can surface them to the user.
func CollectFromExchanges(ctx context.Context, cfg *config.Config, opts CollectOptions) (*CollectResult, error) {
	quote := opts.Quote
	if quote == "" {
		quote = "USDT"
	}

	accounts := listExchangeAccounts(cfg)
	if opts.Source != "" {
		var filtered []exchangeAccount
		for _, ea := range accounts {
			if strings.EqualFold(ea.exchange, opts.Source) {
				if opts.Account == "" || strings.EqualFold(ea.account, opts.Account) {
					filtered = append(filtered, ea)
				}
			}
		}
		accounts = filtered
	}

	if len(accounts) == 0 {
		return nil, fmt.Errorf("no matching exchange accounts found")
	}

	snap := &Snapshot{
		TakenAt: time.Now().UTC(),
		Quote:   quote,
		Label:   opts.Label,
		Note:    opts.Note,
	}

	result := &CollectResult{Snapshot: snap}
	var totalValue float64

	for _, ea := range accounts {
		acctLabel := ea.exchange
		if ea.account != "" {
			acctLabel += "/" + ea.account
		}

		ex, err := exchanges.CreateExchangeForAccount(ea.exchange, ea.account, cfg)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", acctLabel, err))
			continue
		}

		we, ok := ex.(exchanges.WalletExchange)
		if !ok {
			// Basic exchange: use GetBalances.
			balances, err := ex.GetBalances(ctx)
			if err != nil {
				result.Errors = append(result.Errors, fmt.Sprintf("%s: get balances: %v", acctLabel, err))
				continue
			}
			for _, b := range balances {
				qty := b.Free + b.Locked
				if qty == 0 {
					continue
				}
				pos := Position{
					Source:   ea.exchange,
					Account:  ea.account,
					Category: "spot",
					Asset:    b.Asset,
					Quantity: qty,
				}
				if b.Locked > 0 {
					pos.Meta = map[string]string{"locked": fmt.Sprintf("%f", b.Locked)}
				}
				snap.Positions = append(snap.Positions, pos)
			}
			continue
		}

		// Check supported wallet types: use "all" if available, otherwise
		// iterate each supported type individually and merge results.
		supportedTypes := we.SupportedWalletTypes()
		var balances []exchanges.WalletBalance

		if sliceContains(supportedTypes, "all") {
			balances, err = we.GetWalletBalances(ctx, "all")
			if err != nil {
				result.Errors = append(result.Errors, fmt.Sprintf("%s: get wallet balances: %v", acctLabel, err))
				continue
			}
		} else {
			for _, wt := range supportedTypes {
				wb, err := we.GetWalletBalances(ctx, wt)
				if err != nil {
					result.Errors = append(result.Errors, fmt.Sprintf("%s: %s wallet: %v", acctLabel, wt, err))
					continue
				}
				balances = append(balances, wb...)
			}
		}

		// Try to price assets if exchange supports it.
		pe, canPrice := ex.(exchanges.PricedExchange)
		var unpriced []string

		for _, b := range balances {
			qty := b.Free + b.Locked
			if qty == 0 {
				continue
			}

			pos := Position{
				Source:   ea.exchange,
				Account:  ea.account,
				Category: b.WalletType,
				Asset:    b.Asset,
				Quantity: qty,
			}

			if canPrice {
				price, err := pe.FetchPrice(ctx, b.Asset, quote)
				if err == nil {
					pos.Price = price
					if price == 0 {
						// Asset IS the quote currency.
						pos.Value = qty
					} else {
						pos.Value = qty * price
					}
					totalValue += pos.Value
				} else {
					unpriced = append(unpriced, b.Asset)
				}
			}

			if b.Locked > 0 {
				if pos.Meta == nil {
					pos.Meta = make(map[string]string)
				}
				pos.Meta["locked"] = fmt.Sprintf("%f", b.Locked)
			}
			for k, v := range b.Extra {
				if pos.Meta == nil {
					pos.Meta = make(map[string]string)
				}
				pos.Meta[k] = v
			}

			snap.Positions = append(snap.Positions, pos)
		}

		if len(unpriced) > 0 {
			result.Errors = append(result.Errors,
				fmt.Sprintf("%s: could not price: %s", acctLabel, strings.Join(unpriced, ", ")))
		}
	}

	snap.TotalValue = totalValue
	return result, nil
}

// listExchangeAccounts returns all configured exchange/account pairs from config.
func listExchangeAccounts(cfg *config.Config) []exchangeAccount {
	var result []exchangeAccount
	ex := cfg.Exchanges

	if ex.Binance.Enabled {
		for _, acc := range ex.Binance.Accounts {
			result = append(result, exchangeAccount{"binance", acc.Name})
		}
	}
	if ex.BinanceTH.Enabled {
		for _, acc := range ex.BinanceTH.Accounts {
			result = append(result, exchangeAccount{"binanceth", acc.Name})
		}
	}
	if ex.Bitkub.Enabled {
		for _, acc := range ex.Bitkub.Accounts {
			result = append(result, exchangeAccount{"bitkub", acc.Name})
		}
	}
	if ex.OKX.Enabled {
		for _, acc := range ex.OKX.Accounts {
			result = append(result, exchangeAccount{"okx", acc.Name})
		}
	}

	return result
}

// sliceContains reports whether s contains v.
func sliceContains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
