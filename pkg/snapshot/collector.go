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

// acctEx pairs a resolved exchangeAccount with its live exchange instance.
type acctEx struct {
	ea exchangeAccount
	ex exchanges.Exchange
}

// pendingPos holds a position together with the native quote used to price it,
// before cross-exchange conversion to the snapshot quote.
type pendingPos struct {
	pos         Position
	nativeQuote string
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

	// Pass 1: create exchange instances so they can be reused for cross-rate lookups.
	var acctExchanges []acctEx
	for _, ea := range accounts {
		ex, err := exchanges.CreateExchangeForAccount(ea.exchange, ea.account, cfg)
		if err != nil {
			acctLabel := ea.exchange
			if ea.account != "" {
				acctLabel += "/" + ea.account
			}
			result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", acctLabel, err))
			continue
		}
		acctExchanges = append(acctExchanges, acctEx{ea, ex})
	}

	// Pass 2: collect positions, pricing each asset in the exchange's effective quote.
	var pending []pendingPos

	for _, ae := range acctExchanges {
		ea := ae.ea
		ex := ae.ex

		acctLabel := ea.exchange
		if ea.account != "" {
			acctLabel += "/" + ea.account
		}

		eQuote := effectiveQuote(ex, quote)

		we, ok := ex.(exchanges.WalletExchange)
		if !ok {
			// Basic exchange: use GetBalances (no pricing support).
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
					Quote:    eQuote,
				}
				if b.Locked > 0 {
					pos.Meta = map[string]string{"locked": fmt.Sprintf("%f", b.Locked)}
				}
				pending = append(pending, pendingPos{pos, eQuote})
			}
			continue
		}

		// Check supported wallet types: use "all" if available, otherwise
		// iterate each supported type individually and merge results.
		supportedTypes := we.SupportedWalletTypes()
		var balances []exchanges.WalletBalance
		var err error

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
				Quote:    eQuote,
			}

			if canPrice {
				price, err := pe.FetchPrice(ctx, b.Asset, eQuote)
				if err == nil {
					pos.Price = price
					if price == 0 {
						// Asset IS the quote currency.
						pos.Value = qty
					} else {
						pos.Value = qty * price
					}
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

			pending = append(pending, pendingPos{pos, eQuote})
		}

		if len(unpriced) > 0 {
			result.Errors = append(result.Errors,
				fmt.Sprintf("%s: could not price: %s", acctLabel, strings.Join(unpriced, ", ")))
		}
	}

	// Pass 3: build conversion rates — for each native quote ≠ snap.Quote, find
	// the exchange rate using any available PricedExchange.
	// convRates[nativeQuote] = multiplier to convert native quote value → snap.Quote value.
	convRates := map[string]float64{quote: 1.0}
	for _, pp := range pending {
		if _, known := convRates[pp.nativeQuote]; known {
			continue
		}
		for _, ae := range acctExchanges {
			pe, ok := ae.ex.(exchanges.PricedExchange)
			if !ok {
				continue
			}
			rate, err := pe.FetchPrice(ctx, pp.nativeQuote, quote)
			if err == nil && rate > 0 {
				convRates[pp.nativeQuote] = rate
				break
			}
		}
	}

	// Pass 4: accumulate TotalValue (with cross-rate conversion) and commit positions.
	var totalValue float64
	for _, pp := range pending {
		if pp.pos.Value > 0 {
			if rate, ok := convRates[pp.nativeQuote]; ok {
				totalValue += pp.pos.Value * rate
			}
		}
		snap.Positions = append(snap.Positions, pp.pos)
	}

	snap.TotalValue = totalValue
	return result, nil
}

// effectiveQuote returns the best quote currency to use for pricing on ex.
// If ex implements QuoteLister and requestedQuote is not in its supported list,
// the first supported quote is returned as a fallback.
func effectiveQuote(ex exchanges.Exchange, requestedQuote string) string {
	ql, ok := ex.(exchanges.QuoteLister)
	if !ok {
		return requestedQuote
	}
	supported := ql.SupportedQuotes()
	for _, q := range supported {
		if strings.EqualFold(q, requestedQuote) {
			return requestedQuote
		}
	}
	if len(supported) > 0 {
		return supported[0]
	}
	return requestedQuote
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
	if ex.Settrade.Enabled {
		for _, acc := range ex.Settrade.Accounts {
			result = append(result, exchangeAccount{"settrade", acc.Name})
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
