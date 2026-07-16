package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/exchanges"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// ExchangeTotalValueTool estimates the total portfolio value in a quote currency
// by summing all wallet balances and looking up live prices for each asset.
type ExchangeTotalValueTool struct {
	cfg *config.Config
}

func NewExchangeTotalValueTool(cfg *config.Config) *ExchangeTotalValueTool {
	return &ExchangeTotalValueTool{cfg: cfg}
}

func (t *ExchangeTotalValueTool) Name() string { return NameGetTotalValue }

func (t *ExchangeTotalValueTool) Description() string {
	return "Estimate the total portfolio value in a quote currency (default USDT) by fetching all wallet balances and looking up live prices for each asset."
}

func (t *ExchangeTotalValueTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"exchange": map[string]any{
				"type":        "string",
				"description": "Exchange to query (default: \"binance\")",
				"enum":        []string{"binance", "binanceth", "bitkub", "okx", "settrade", "webull"},
			},
			"account": map[string]any{
				"type":        "string",
				"description": "Account name to query (e.g. \"HighRiskPort\"). Omit for default account.",
			},
			"wallet_type": map[string]any{
				"type":        "string",
				"description": "Wallet scope. Same options as get_assets_list: all, spot, funding, futures_usdt, futures_coin, margin, earn_flexible, earn_locked, earn. Default: \"all\".",
				"enum":        []string{"spot", "funding", "futures_usdt", "futures_coin", "margin", "earn_flexible", "earn_locked", "earn", "all"},
			},
			"quote": map[string]any{
				"type":        "string",
				"description": "Quote currency for valuation (default: \"USDT\")",
			},
		},
		"required": []string{},
	}
}

func (t *ExchangeTotalValueTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	exchangeName, _ := args["exchange"].(string)
	accountName := ""
	if v, ok := args["account"].(string); ok {
		accountName = strings.TrimSpace(v)
	}
	walletType := "all"
	if v, ok := args["wallet_type"].(string); ok && v != "" {
		walletType = v
	}
	quote := "USDT"
	if v, ok := args["quote"].(string); ok && v != "" {
		quote = strings.ToUpper(strings.TrimSpace(v))
	}

	// No exchange specified — sum across all enabled platforms.
	if exchangeName == "" {
		return t.executeAll(ctx, walletType, quote)
	}

	return t.executeSingle(ctx, exchangeName, accountName, walletType, quote)
}

func (t *ExchangeTotalValueTool) executeAll(ctx context.Context, walletType, quote string) *ToolResult {
	refs := broker.ListConfiguredAccounts(t.cfg)
	if len(refs) == 0 {
		return UserResult("No exchange accounts are configured.")
	}

	type lineItem struct {
		label       string
		value       float64
		nativeQuote string // set when the subtotal could not be converted to quote
		unpriced    []string
		err         string
	}

	// Pass 1: create exchanges up front so every priced exchange can serve
	// cross-rate lookups for the others.
	type acctEx struct {
		label      string
		providerID string
		account    string
		pe         exchanges.PricedExchange
	}
	var lines []lineItem
	var acctExchanges []acctEx
	for _, ref := range refs {
		label := ref.ProviderID
		if ref.Account != "" {
			label += " (" + ref.Account + ")"
		}

		ex, err := exchanges.CreateExchangeForAccount(ref.ProviderID, ref.Account, t.cfg)
		if err != nil {
			lines = append(lines, lineItem{label: label, err: err.Error()})
			continue
		}
		pe, ok := ex.(exchanges.PricedExchange)
		if !ok {
			lines = append(lines, lineItem{label: label, err: "pricing not supported"})
			continue
		}
		acctExchanges = append(acctExchanges, acctEx{label: label, providerID: ref.ProviderID, account: ref.Account, pe: pe})
	}

	// Pass 2: subtotal each account in its effective quote. An exchange that
	// does not support the requested quote (e.g. USD-only Webull under the
	// default USDT) is priced in its own quote and converted below.
	type pendingLine struct {
		item        lineItem
		nativeQuote string
	}
	var pending []pendingLine
	for _, ae := range acctExchanges {
		eQuote := priceableQuote(ae.pe, quote)

		balances, err := ae.pe.GetWalletBalances(ctx, walletType)
		if err != nil {
			errMsg := reauthText(err, ae.providerID, ae.account)
			if errMsg == "" {
				errMsg = trimCCXTError(err)
			}
			lines = append(lines, lineItem{label: ae.label, err: errMsg})
			continue
		}

		totals := make(map[string]float64)
		for _, b := range balances {
			totals[b.Asset] += b.Free + b.Locked
		}

		var subtotal float64
		var unpriced []string
		for asset, amount := range totals {
			if amount == 0 {
				continue
			}
			price, err := ae.pe.FetchPrice(ctx, asset, eQuote)
			if err != nil {
				unpriced = append(unpriced, asset)
				continue
			}
			if price == 0 {
				subtotal += amount
			} else {
				subtotal += amount * price
			}
		}

		pending = append(pending, pendingLine{
			item:        lineItem{label: ae.label, value: subtotal, unpriced: unpriced},
			nativeQuote: strings.ToUpper(eQuote),
		})
	}

	// Pass 3: resolve conversion rates for native quotes ≠ requested quote.
	// FetchPrice returning (0, nil) signals a 1:1 usd-like pair.
	convRates := map[string]float64{quote: 1.0}
	for _, pl := range pending {
		if _, known := convRates[pl.nativeQuote]; known {
			continue
		}
		if exchanges.USDLike(pl.nativeQuote) && exchanges.USDLike(quote) {
			convRates[pl.nativeQuote] = 1.0
			continue
		}
		for _, ae := range acctExchanges {
			rate, err := ae.pe.FetchPrice(ctx, pl.nativeQuote, quote)
			if err == nil {
				if rate == 0 {
					rate = 1.0
				}
				convRates[pl.nativeQuote] = rate
				break
			}
		}
	}

	// Pass 4: convert subtotals into the requested quote and accumulate the
	// grand total. Subtotals with no known rate stay in their native quote
	// and are excluded from the total (flagged on their line).
	var grandTotal float64
	for _, pl := range pending {
		li := pl.item
		if rate, ok := convRates[pl.nativeQuote]; ok {
			li.value *= rate
			grandTotal += li.value
		} else {
			li.nativeQuote = pl.nativeQuote
		}
		lines = append(lines, li)
	}

	ts := time.Now().UTC().Format(time.RFC3339)
	var sb strings.Builder
	fmt.Fprintf(&sb, "Time: %s\n", ts)
	for _, li := range lines {
		if li.err != "" {
			fmt.Fprintf(&sb, "  %-30s  ERROR: %s\n", li.label, li.err)
		} else {
			lineQuote := quote
			if li.nativeQuote != "" {
				lineQuote = li.nativeQuote
			}
			row := fmt.Sprintf("  %-30s  %10.2f %s", li.label, li.value, lineQuote)
			if li.nativeQuote != "" {
				row += fmt.Sprintf(" (no %s rate; excluded from total)", quote)
			}
			if len(li.unpriced) > 0 {
				row += fmt.Sprintf(" (could not price: %s)", strings.Join(li.unpriced, ", "))
			}
			sb.WriteString(row + "\n")
		}
	}
	fmt.Fprintf(&sb, "  %-30s  %10.2f %s\n", "TOTAL", grandTotal, quote)
	return UserResult(sb.String())
}

// priceableQuote returns the quote currency to price assets in on pe: the
// requested quote when supported, otherwise the exchange's first supported
// quote (mirrors effectiveQuote in pkg/snapshot).
func priceableQuote(pe exchanges.PricedExchange, requested string) string {
	ql, ok := pe.(exchanges.QuoteLister)
	if !ok {
		return requested
	}
	supported := ql.SupportedQuotes()
	for _, q := range supported {
		if strings.EqualFold(q, requested) {
			return requested
		}
	}
	if len(supported) > 0 {
		return supported[0]
	}
	return requested
}

func (t *ExchangeTotalValueTool) executeSingle(ctx context.Context, exchangeName, accountName, walletType, quote string) *ToolResult {
	ex, err := exchanges.CreateExchangeForAccount(exchangeName, accountName, t.cfg)
	if err != nil {
		return ErrorResult(fmt.Sprintf("get_total_value: %v", err))
	}

	pe, ok := ex.(exchanges.PricedExchange)
	if !ok {
		return ErrorResult(fmt.Sprintf("get_total_value: exchange %q does not support price lookup", exchangeName))
	}

	// Probe that the quote currency is supported (skip for BTC since BTC/BTC is a self-pair returning 0).
	if quote != "BTC" {
		if _, err := pe.FetchPrice(ctx, "BTC", quote); err != nil {
			if hint := reauthHint(err, exchangeName, accountName); hint != nil {
				return hint
			}
			clean := trimCCXTError(err)
			if isNetworkError(clean) {
				return ErrorResult(fmt.Sprintf("Exchange %q is unreachable (network error): %s", exchangeName, clean))
			}
			msg := fmt.Sprintf("Quote %q is not supported on %s: %s", quote, exchangeName, clean)
			if ql, ok := pe.(exchanges.QuoteLister); ok {
				msg += fmt.Sprintf(". Supported quotes: %s", strings.Join(ql.SupportedQuotes(), ", "))
			}
			return ErrorResult(msg)
		}
	}

	// Fetch all balances for the requested wallet scope.
	balances, err := pe.GetWalletBalances(ctx, walletType)
	if err != nil {
		if hint := reauthHint(err, exchangeName, accountName); hint != nil {
			return hint
		}
		return ErrorResult(fmt.Sprintf("get_total_value: fetch balances: %s", trimCCXTError(err)))
	}

	exchangeHeader := exchangeName
	if accountName != "" {
		exchangeHeader += " (" + accountName + ")"
	}

	if len(balances) == 0 {
		return UserResult(fmt.Sprintf("No non-zero balances found on %s (%s wallet).", exchangeHeader, walletType))
	}

	// Aggregate total quantity per asset across all wallets.
	totals := make(map[string]float64)
	for _, b := range balances {
		totals[b.Asset] += b.Free + b.Locked
	}

	// Price each asset and accumulate total value.
	var totalValue float64
	var unpriced []string

	for asset, amount := range totals {
		if amount == 0 {
			continue
		}
		price, err := pe.FetchPrice(ctx, asset, quote)
		if err != nil {
			unpriced = append(unpriced, asset)
			continue
		}
		if price == 0 {
			// asset IS the quote currency (stablecoin 1:1)
			totalValue += amount
		} else {
			totalValue += amount * price
		}
	}

	// Build compact single-line output.
	var result string
	ts := time.Now().UTC().Format(time.RFC3339)
	if accountName != "" {
		result = fmt.Sprintf("Time: %s, Account: %s, Total value: %.2f %s", ts, accountName, totalValue, quote)
	} else {
		result = fmt.Sprintf("Time: %s, Exchange: %s, Total value: %.2f %s", ts, exchangeName, totalValue, quote)
	}
	if len(unpriced) > 0 {
		result += fmt.Sprintf(" (Note: could not price: %s)", strings.Join(unpriced, ", "))
	}

	return UserResult(result)
}

// trimCCXTError strips CCXT stack trace noise from an error, keeping only the
// first meaningful line (e.g. "[ccxtError]::[NetworkError]::[...]").
func trimCCXTError(err error) string {
	s := err.Error()
	if idx := strings.Index(s, "\nStack:"); idx >= 0 {
		s = s[:idx]
	}
	// Also trim at the first bare newline to drop any secondary goroutine dumps.
	if idx := strings.Index(s, "\n"); idx >= 0 {
		s = strings.TrimSpace(s[:idx])
	}
	return s
}

// isNetworkError reports whether the error message indicates a network/connectivity
// failure rather than an unsupported-quote error.
func isNetworkError(msg string) bool {
	for _, kw := range []string{"NetworkError", "no such host", "dial tcp", "connection refused", "i/o timeout", "network error"} {
		if strings.Contains(msg, kw) {
			return true
		}
	}
	return false
}
