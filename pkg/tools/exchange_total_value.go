package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/khunquant/khunquant/pkg/config"
	"github.com/khunquant/khunquant/pkg/exchanges"
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
				"enum":        []string{"binance", "binanceth", "bitkub", "okx", "settrade"},
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

type accountRef struct {
	exchange string
	account  string
}

func (t *ExchangeTotalValueTool) enabledAccounts() []accountRef {
	var refs []accountRef
	ex := t.cfg.Exchanges

	if ex.Binance.Enabled {
		for i, acc := range ex.Binance.Accounts {
			name := acc.Name
			if name == "" {
				name = fmt.Sprintf("%d", i+1)
			}
			refs = append(refs, accountRef{exchange: "binance", account: name})
		}
	}
	if ex.BinanceTH.Enabled {
		for i, acc := range ex.BinanceTH.Accounts {
			name := acc.Name
			if name == "" {
				name = fmt.Sprintf("%d", i+1)
			}
			refs = append(refs, accountRef{exchange: "binanceth", account: name})
		}
	}
	if ex.Bitkub.Enabled {
		for i, acc := range ex.Bitkub.Accounts {
			name := acc.Name
			if name == "" {
				name = fmt.Sprintf("%d", i+1)
			}
			refs = append(refs, accountRef{exchange: "bitkub", account: name})
		}
	}
	if ex.OKX.Enabled {
		for i, acc := range ex.OKX.Accounts {
			name := acc.Name
			if name == "" {
				name = fmt.Sprintf("%d", i+1)
			}
			refs = append(refs, accountRef{exchange: "okx", account: name})
		}
	}
	if ex.Settrade.Enabled {
		for i, acc := range ex.Settrade.Accounts {
			name := acc.Name
			if name == "" {
				name = fmt.Sprintf("%d", i+1)
			}
			refs = append(refs, accountRef{exchange: "settrade", account: name})
		}
	}
	return refs
}

func (t *ExchangeTotalValueTool) executeAll(ctx context.Context, walletType, quote string) *ToolResult {
	refs := t.enabledAccounts()
	if len(refs) == 0 {
		return UserResult("No exchange accounts are configured.")
	}

	type lineItem struct {
		label    string
		value    float64
		unpriced []string
		err      string
	}

	var lines []lineItem
	var grandTotal float64

	for _, ref := range refs {
		label := ref.exchange
		if ref.account != "" {
			label += " (" + ref.account + ")"
		}

		ex, err := exchanges.CreateExchangeForAccount(ref.exchange, ref.account, t.cfg)
		if err != nil {
			lines = append(lines, lineItem{label: label, err: err.Error()})
			continue
		}
		pe, ok := ex.(exchanges.PricedExchange)
		if !ok {
			lines = append(lines, lineItem{label: label, err: "pricing not supported"})
			continue
		}

		balances, err := pe.GetWalletBalances(ctx, walletType)
		if err != nil {
			lines = append(lines, lineItem{label: label, err: trimCCXTError(err)})
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
			price, err := pe.FetchPrice(ctx, asset, quote)
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

		grandTotal += subtotal
		lines = append(lines, lineItem{label: label, value: subtotal, unpriced: unpriced})
	}

	ts := time.Now().UTC().Format(time.RFC3339)
	var sb strings.Builder
	fmt.Fprintf(&sb, "Time: %s\n", ts)
	for _, li := range lines {
		if li.err != "" {
			fmt.Fprintf(&sb, "  %-30s  ERROR: %s\n", li.label, li.err)
		} else {
			row := fmt.Sprintf("  %-30s  %10.2f %s", li.label, li.value, quote)
			if len(li.unpriced) > 0 {
				row += fmt.Sprintf(" (could not price: %s)", strings.Join(li.unpriced, ", "))
			}
			sb.WriteString(row + "\n")
		}
	}
	fmt.Fprintf(&sb, "  %-30s  %10.2f %s\n", "TOTAL", grandTotal, quote)
	return UserResult(sb.String())
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
