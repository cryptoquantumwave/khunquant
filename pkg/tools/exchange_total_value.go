package tools

import (
	"context"
	"fmt"
	"sort"
	"strings"

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

func (t *ExchangeTotalValueTool) Name() string { return "exchange_total_value" }

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
				"enum":        []string{"binance"},
			},
			"wallet_type": map[string]any{
				"type":        "string",
				"description": "Wallet scope. Same options as exchange_balance: all, spot, funding, futures_usdt, futures_coin, margin, earn_flexible, earn_locked, earn. Default: \"all\".",
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
	exchangeName := "binance"
	if v, ok := args["exchange"].(string); ok && v != "" {
		exchangeName = v
	}
	walletType := "all"
	if v, ok := args["wallet_type"].(string); ok && v != "" {
		walletType = v
	}
	quote := "USDT"
	if v, ok := args["quote"].(string); ok && v != "" {
		quote = strings.ToUpper(strings.TrimSpace(v))
	}

	ex, err := exchanges.CreateExchange(exchangeName, t.cfg)
	if err != nil {
		return ErrorResult(fmt.Sprintf("exchange_total_value: %v", err))
	}

	pe, ok := ex.(exchanges.PricedExchange)
	if !ok {
		return ErrorResult(fmt.Sprintf("exchange_total_value: exchange %q does not support price lookup", exchangeName))
	}

	// Fetch all balances for the requested wallet scope.
	balances, err := pe.GetWalletBalances(ctx, walletType)
	if err != nil {
		return ErrorResult(fmt.Sprintf("exchange_total_value: fetch balances: %v", err))
	}

	if len(balances) == 0 {
		return UserResult(fmt.Sprintf("No non-zero balances found on %s (%s wallet).", exchangeName, walletType))
	}

	// Aggregate total quantity per asset across all wallets.
	type assetTotal struct {
		asset  string
		amount float64
	}
	totals := make(map[string]float64)
	for _, b := range balances {
		totals[b.Asset] += b.Free + b.Locked
	}

	// Price each asset and build the breakdown.
	type line struct {
		asset      string
		amount     float64
		price      float64
		valueQuote float64
		unpriced   bool
	}
	var lines []line
	var totalValue float64
	var unpriced []string

	for asset, amount := range totals {
		if amount == 0 {
			continue
		}
		price, err := pe.FetchPrice(ctx, asset, quote)
		if err != nil {
			// Can't price — record but don't fail
			unpriced = append(unpriced, fmt.Sprintf("%s (%.8f)", asset, amount))
			lines = append(lines, line{asset: asset, amount: amount, unpriced: true})
			continue
		}
		var valueQuote float64
		if price == 0 {
			// asset IS the quote currency (stablecoin 1:1)
			valueQuote = amount
		} else {
			valueQuote = amount * price
		}
		totalValue += valueQuote
		lines = append(lines, line{asset: asset, amount: amount, price: price, valueQuote: valueQuote})
	}

	// Sort by value descending, unpriced at the bottom.
	sort.Slice(lines, func(i, j int) bool {
		if lines[i].unpriced != lines[j].unpriced {
			return !lines[i].unpriced // priced first
		}
		return lines[i].valueQuote > lines[j].valueQuote
	})

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Estimated total portfolio value on %s", exchangeName))
	if walletType != "all" {
		sb.WriteString(fmt.Sprintf(" (%s)", walletType))
	}
	sb.WriteString(fmt.Sprintf(": **%.2f %s**\n\n", totalValue, quote))

	sb.WriteString(fmt.Sprintf("%-12s %18s %16s %16s\n", "Asset", "Amount", "Price ("+quote+")", "Value ("+quote+")"))
	sb.WriteString(strings.Repeat("-", 64) + "\n")

	for _, l := range lines {
		if l.unpriced {
			sb.WriteString(fmt.Sprintf("%-12s %18.8f %16s %16s\n", l.asset, l.amount, "n/a", "n/a"))
			continue
		}
		priceStr := "1 (stable)"
		if l.price > 0 {
			priceStr = fmt.Sprintf("%.6f", l.price)
		}
		sb.WriteString(fmt.Sprintf("%-12s %18.8f %16s %16.2f\n", l.asset, l.amount, priceStr, l.valueQuote))
	}

	sb.WriteString(strings.Repeat("-", 64) + "\n")
	sb.WriteString(fmt.Sprintf("%-12s %18s %16s %16.2f\n", "TOTAL", "", "", totalValue))

	if len(unpriced) > 0 {
		sb.WriteString(fmt.Sprintf("\nNote: could not price %d asset(s): %s\n", len(unpriced), strings.Join(unpriced, ", ")))
	}

	return UserResult(sb.String())
}
