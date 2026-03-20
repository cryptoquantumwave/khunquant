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

func (t *ExchangeTotalValueTool) Name() string { return "get_total_value" }

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
	exchangeName := "binance"
	if v, ok := args["exchange"].(string); ok && v != "" {
		exchangeName = v
	}
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
			return ErrorResult(fmt.Sprintf("Quote %q is not supported on %s", quote, exchangeName))
		}
	}

	// Fetch all balances for the requested wallet scope.
	balances, err := pe.GetWalletBalances(ctx, walletType)
	if err != nil {
		return ErrorResult(fmt.Sprintf("get_total_value: fetch balances: %v", err))
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
