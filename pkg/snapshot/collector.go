package snapshot

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/exchanges"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
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

	// ErrorsByAccount attributes the failures in Errors to their account, keyed
	// "exchange/account" (the config spelling; account names are never empty
	// here). Several messages for one account are joined with "; ". An account
	// listed here has an incomplete valuation, so its total must not be used.
	ErrorsByAccount map[string]string

	// AccountTotals is the value of each account that was collected without any
	// account-level error, in the snapshot quote, keyed "exchange/account". An
	// account with no balances is present with a total of 0. An account in
	// ErrorsByAccount is never present.
	AccountTotals map[string]float64

	// AccountWallets lists, for each account in AccountTotals, the wallet types
	// that were successfully read (sorted; empty wallets count). Two totals for
	// the same account are comparable only if their wallet lists are equal: a
	// wallet type that failed this time but not last time makes the total look
	// like a loss.
	AccountWallets map[string][]string

	// WalletErrorsByAccount records wallet types that failed inside an
	// otherwise successful account (also listed in Errors). Unlike
	// ErrorsByAccount this does not exclude the account from AccountTotals; the
	// missing wallet type is simply absent from AccountWallets.
	WalletErrorsByAccount map[string]string

	// QuoteRates maps each native quote currency that positions were priced in
	// (Position.Quote) to the multiplier that converts a value in it into the
	// snapshot quote. It is the rate TotalValue was built with, so
	// Position.Value*QuoteRates[Position.Quote] is the position's value in
	// Snapshot.Quote. A quote with no rate is absent.
	QuoteRates map[string]float64

	// UnpricedByAccount lists, per "exchange/account", the assets whose price could
	// not be fetched (sorted, without duplicates). Each such account also has its
	// "could not price" entry in Errors and ErrorsByAccount, unchanged; this field
	// only tells a caller what that entry was, so that "the only problem was
	// unpriced assets" can be told from a failed read or a missing conversion rate
	// (see OnlyUnpricedErrors). An unpriced asset is in Snapshot.Positions with a
	// Value of 0 and is not in TotalValue.
	UnpricedByAccount map[string][]string
}

// addUnpriced records the assets of an account that could not be priced.
func (r *CollectResult) addUnpriced(label string, assets []string) {
	if r.UnpricedByAccount == nil {
		r.UnpricedByAccount = make(map[string][]string)
	}
	sorted := append([]string(nil), assets...)
	sort.Strings(sorted)
	out := sorted[:0]
	for i, a := range sorted {
		if i == 0 || a != sorted[i-1] {
			out = append(out, a)
		}
	}
	r.UnpricedByAccount[label] = out
}

// OnlyUnpricedErrors reports whether the collection has errors and every one of
// them is an account's "could not price" entry: no account or wallet failed to
// read and no conversion rate is missing. The collector adds exactly one Errors
// entry per account with unpriced assets, so this is true when the number of
// entries equals the number of accounts in UnpricedByAccount.
func (r *CollectResult) OnlyUnpricedErrors() bool {
	return r != nil && len(r.Errors) > 0 && len(r.Errors) == len(r.UnpricedByAccount)
}

// Unpriced returns the unpriced assets as sorted "exchange/account:ASSET" entries.
func (r *CollectResult) Unpriced() []string {
	if r == nil {
		return nil
	}
	var out []string
	for label, assets := range r.UnpricedByAccount {
		for _, a := range assets {
			out = append(out, label+":"+a)
		}
	}
	sort.Strings(out)
	return out
}

// addError records an account failure in both Errors and ErrorsByAccount.
func (r *CollectResult) addError(label, msg string) {
	r.Errors = append(r.Errors, label+": "+msg)
	if r.ErrorsByAccount == nil {
		r.ErrorsByAccount = make(map[string]string)
	}
	if prev, ok := r.ErrorsByAccount[label]; ok {
		msg = prev + "; " + msg
	}
	r.ErrorsByAccount[label] = msg
}

// addWalletError records a wallet type that failed inside an account that is
// still usable: it is listed in Errors and WalletErrorsByAccount only.
func (r *CollectResult) addWalletError(label, msg string) {
	r.Errors = append(r.Errors, label+": "+msg)
	if r.WalletErrorsByAccount == nil {
		r.WalletErrorsByAccount = make(map[string]string)
	}
	if prev, ok := r.WalletErrorsByAccount[label]; ok {
		msg = prev + "; " + msg
	}
	r.WalletErrorsByAccount[label] = msg
}

// accountLabel is the "exchange/account" key used by Errors and the per-account maps.
func accountLabel(exchange, account string) string {
	if account == "" {
		return exchange
	}
	return exchange + "/" + account
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
	source := strings.TrimSpace(opts.Source)
	if source != "" && !strings.EqualFold(source, "all") {
		var filtered []exchangeAccount
		for _, ea := range accounts {
			if strings.EqualFold(ea.exchange, source) {
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
			result.addError(accountLabel(ea.exchange, ea.account), err.Error())
			continue
		}
		acctExchanges = append(acctExchanges, acctEx{ea, ex})
	}

	// Pass 2: collect positions, pricing each asset in the exchange's effective quote.
	var pending []pendingPos
	wallets := make(map[string][]string) // label -> wallet types read successfully

	for _, ae := range acctExchanges {
		ea := ae.ea
		ex := ae.ex

		acctLabel := accountLabel(ea.exchange, ea.account)

		eQuote := effectiveQuote(ex, quote)

		we, ok := ex.(exchanges.WalletExchange)
		if !ok {
			// Basic exchange: use GetBalances (no pricing support).
			balances, err := ex.GetBalances(ctx)
			if err != nil {
				result.addError(acctLabel, fmt.Sprintf("get balances: %v", err))
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
			wallets[acctLabel] = []string{"spot"}
			continue
		}

		// Check supported wallet types: use "all" if available, otherwise
		// iterate each supported type individually and merge results.
		supportedTypes := we.SupportedWalletTypes()
		var balances []exchanges.WalletBalance
		var err error

		if sliceContains(supportedTypes, "all") {
			var failures []exchanges.WalletFailure
			if pwe, ok := ex.(exchanges.PartialWalletExchange); ok {
				// The plain "all" call drops a failed wallet type silently and
				// returns what is left; the partial variant reports it, so the
				// wallet list below says what the total really covers.
				balances, failures, err = pwe.GetWalletBalancesPartial(ctx, "all")
			} else {
				balances, err = we.GetWalletBalances(ctx, "all")
			}
			if err != nil {
				for _, f := range failures {
					result.addError(acctLabel, fmt.Sprintf("%s wallet: %v", f.WalletType, f.Err))
				}
				result.addError(acctLabel, fmt.Sprintf("get wallet balances: %v", err))
				continue
			}
			failed := make(map[string]bool, len(failures))
			for _, f := range failures {
				failed[f.WalletType] = true
				result.addWalletError(acctLabel, fmt.Sprintf("%s wallet: %v", f.WalletType, f.Err))
			}
			var read []string
			for _, wt := range supportedTypes {
				if wt != "all" && !failed[wt] {
					read = append(read, wt)
				}
			}
			if len(read) == 0 {
				// The exchange lists no concrete wallet types, so name the
				// aggregate and mark what dropped out of it.
				read = []string{"all"}
				for _, f := range failures {
					read = append(read, "-"+f.WalletType)
				}
			}
			wallets[acctLabel] = read
		} else {
			type walletFailure struct {
				wt  string
				err error
			}
			var failures []walletFailure
			var read []string
			for _, wt := range supportedTypes {
				wb, err := we.GetWalletBalances(ctx, wt)
				if err != nil {
					failures = append(failures, walletFailure{wt, err})
					continue
				}
				read = append(read, wt)
				balances = append(balances, wb...)
			}
			for _, f := range failures {
				msg := fmt.Sprintf("%s wallet: %v", f.wt, f.err)
				if len(read) == 0 {
					// Nothing could be read at all: the account has no usable valuation.
					result.addError(acctLabel, msg)
				} else {
					result.addWalletError(acctLabel, msg)
				}
			}
			if len(read) == 0 && len(failures) > 0 {
				continue
			}
			wallets[acctLabel] = read
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

			priced := false
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
					priced = true
				}
			}
			if !priced {
				// Fall back to the provider's own valuation (e.g. Webull
				// positions carry market_value even when live market data is
				// unavailable or subscription-gated).
				if mv, err := strconv.ParseFloat(b.Extra["market_value"], 64); err == nil && mv > 0 {
					pos.Value = mv
					pos.Price = mv / qty
					priced = true
				}
			}
			if !priced && canPrice {
				unpriced = append(unpriced, b.Asset)
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
			result.addError(acctLabel, fmt.Sprintf("could not price: %s", strings.Join(unpriced, ", ")))
			result.addUnpriced(acctLabel, unpriced)
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
		// USD and USD stablecoins are 1:1 — this needs no exchange, so a
		// USD-only broker (e.g. Webull) converts even when it is the sole
		// configured account.
		if exchanges.USDLike(pp.nativeQuote) && exchanges.USDLike(quote) {
			convRates[pp.nativeQuote] = 1.0
			continue
		}
		for _, ae := range acctExchanges {
			pe, ok := ae.ex.(exchanges.PricedExchange)
			if !ok {
				continue
			}
			rate, err := pe.FetchPrice(ctx, pp.nativeQuote, quote)
			if err == nil {
				if rate == 0 {
					// FetchPrice returns 0 to signal the asset IS the quote
					// currency (or a 1:1 usd-like pair, e.g. USD→USDT).
					rate = 1.0
				}
				convRates[pp.nativeQuote] = rate
				break
			}
		}
	}

	// Pass 4: accumulate TotalValue (with cross-rate conversion) and commit positions.
	//
	// Per-account totals are accumulated alongside. An account whose valuation
	// is incomplete (any error above, or a quote with no conversion rate) is left
	// out of AccountTotals rather than reported low.
	result.AccountTotals = make(map[string]float64, len(acctExchanges))
	result.AccountWallets = make(map[string][]string, len(acctExchanges))
	for _, ae := range acctExchanges {
		label := accountLabel(ae.ea.exchange, ae.ea.account)
		if _, failed := result.ErrorsByAccount[label]; !failed {
			result.AccountTotals[label] = 0
			w := append([]string(nil), wallets[label]...)
			sort.Strings(w)
			result.AccountWallets[label] = w
		}
	}
	var totalValue float64
	for _, pp := range pending {
		if pp.pos.Value > 0 {
			label := accountLabel(pp.pos.Source, pp.pos.Account)
			if rate, ok := convRates[pp.nativeQuote]; ok {
				v := pp.pos.Value * rate
				totalValue += v
				if _, tracked := result.AccountTotals[label]; tracked {
					result.AccountTotals[label] += v
				}
			} else {
				// The position cannot be converted, so the account's total
				// would be silently low.
				result.addError(label, fmt.Sprintf("no %s conversion rate for %s", quote, pp.nativeQuote))
				delete(result.AccountTotals, label)
				delete(result.AccountWallets, label)
			}
		}
		snap.Positions = append(snap.Positions, pp.pos)
	}

	snap.TotalValue = totalValue
	result.QuoteRates = convRates
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
	refs := broker.ListConfiguredAccounts(cfg)
	result := make([]exchangeAccount, len(refs))
	for i, ref := range refs {
		result[i] = exchangeAccount{ref.ProviderID, ref.Account}
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
