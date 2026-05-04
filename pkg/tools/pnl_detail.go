package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

const maxPnLDetailLimit = 200

// GetPnLDetailTool provides a per-symbol PnL audit: replays buy/sell trade
// history to show realized profit, average cost, current unrealized gain/loss,
// and fees paid.
type GetPnLDetailTool struct {
	cfg *config.Config
}

func NewGetPnLDetailTool(cfg *config.Config) *GetPnLDetailTool {
	return &GetPnLDetailTool{cfg: cfg}
}

func (t *GetPnLDetailTool) Name() string { return NameGetPnLDetail }

func (t *GetPnLDetailTool) Description() string {
	return "Per-symbol PnL deep-dive: replay all buy/sell trades for one symbol to show realized profit, " +
		"weighted-average cost basis, unrealized gain/loss, and total fees paid. " +
		"Requires provider and symbol. Use get_pnl_summary for cross-asset or cross-exchange overviews."
}

func (t *GetPnLDetailTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"provider": map[string]any{
				"type":        "string",
				"description": "Exchange name (e.g. binance, bitkub, settrade).",
			},
			"account": map[string]any{
				"type":        "string",
				"description": "Account name within the provider. Omit for default.",
			},
			"symbol": map[string]any{
				"type":        "string",
				"description": `Trading pair in CCXT format, e.g. "SOL/USDT", "BTC/THB", "PTT".`,
			},
			"since": map[string]any{
				"type":        "string",
				"description": `Start of trade history window. Accepts ISO 8601 ("2025-01-01") or relative ("30d", "365d"). Default: no lower bound (up to last 200 trades).`,
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": fmt.Sprintf("Max trades to retrieve (1–%d). Default: %d.", maxPnLDetailLimit, maxPnLDetailLimit),
			},
		},
		"required": []string{"provider", "symbol"},
	}
}

func (t *GetPnLDetailTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	providerID, _ := args["provider"].(string)
	account, _ := args["account"].(string)
	symbol, _ := args["symbol"].(string)

	if providerID == "" {
		return ErrorResult("get_pnl_detail: provider is required")
	}
	if symbol == "" {
		return ErrorResult("get_pnl_detail: symbol is required")
	}

	// Normalise symbol to CCXT uppercase with slash.
	symbol = strings.ToUpper(strings.ReplaceAll(symbol, "_", "/"))

	limit := maxPnLDetailLimit
	if v, ok := args["limit"].(float64); ok && v > 0 {
		limit = int(v)
		if limit > maxPnLDetailLimit {
			limit = maxPnLDetailLimit
		}
	}

	var since *int64
	if s, ok := args["since"].(string); ok && s != "" {
		if t2 := parseTimeParam(s); t2 != nil {
			ms := t2.UnixMilli()
			since = &ms
		}
	}

	p, err := broker.CreateProviderForAccount(providerID, account, t.cfg)
	if err != nil {
		return ErrorResult(fmt.Sprintf("get_pnl_detail: provider %q: %v", providerID, err))
	}

	tp, ok := p.(broker.TradingProvider)
	if !ok {
		return ErrorResult(fmt.Sprintf("get_pnl_detail: provider %q does not support trade history", providerID))
	}

	trades, err := tp.FetchMyTrades(ctx, symbol, since, limit)
	if err != nil {
		return ErrorResult(fmt.Sprintf("get_pnl_detail: FetchMyTrades: %v", err)).WithError(err)
	}

	r := ComputeAvgCost(trades)

	// Derive quote from symbol if possible (e.g. "SOL/USDT" → "USDT").
	quote := ""
	if idx := strings.Index(symbol, "/"); idx >= 0 {
		quote = symbol[idx+1:]
	} else {
		quote = nativeQuoteForProvider(providerID)
	}

	// Fetch current price.
	var currentPrice float64
	md, hasPrice := p.(broker.MarketDataProvider)
	if hasPrice {
		ticker, terr := md.FetchTicker(ctx, symbol)
		if terr == nil && ticker.Last != nil {
			currentPrice = *ticker.Last
		}
	}
	if currentPrice == 0 {
		pp, hasPP := p.(broker.PortfolioProvider)
		if hasPP {
			base := symbol
			if idx := strings.Index(symbol, "/"); idx >= 0 {
				base = symbol[:idx]
			}
			fp, ferr := pp.FetchPrice(ctx, base, quote)
			if ferr == nil && fp > 0 {
				currentPrice = fp
			}
		}
	}

	// Format output.
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("PnL Detail — %s on %s", symbol, providerID))
	if account != "" {
		sb.WriteString(fmt.Sprintf(" (%s)", account))
	}
	sb.WriteString("\n\n")

	// Trades window.
	tradeWindow := "all available"
	if r.FirstTs != nil && r.LastTs != nil {
		first := time.UnixMilli(*r.FirstTs).UTC().Format("2006-01-02")
		last := time.UnixMilli(*r.LastTs).UTC().Format("2006-01-02")
		tradeWindow = fmt.Sprintf("%s → %s", first, last)
	}
	sb.WriteString(fmt.Sprintf("  Trades window:    %s (%d trades)\n", tradeWindow, r.TradeCount))
	if r.TruncatedAt200 {
		sb.WriteString(fmt.Sprintf("  [!] Hit %d-trade cap — older history excluded; avg_cost may be underestimated.\n", maxPnLDetailLimit))
	}
	sb.WriteString("\n")

	avgBuyPrice := 0.0
	if r.BoughtQty > 0 {
		avgBuyPrice = r.BoughtCost / r.BoughtQty
	}
	avgSellPrice := 0.0
	if r.SoldQty > 0 {
		avgSellPrice = r.SoldProceeds / r.SoldQty
	}

	sb.WriteString(fmt.Sprintf("  Total bought:     %s %s @ avg %s %s  (cost: %s %s)\n",
		formatAmount(r.BoughtQty), baseSymbol(symbol),
		fmt.Sprintf("%.4f", avgBuyPrice), quote,
		fmt.Sprintf("%.4f", r.BoughtCost), quote))

	if r.SoldQty > 0 {
		sb.WriteString(fmt.Sprintf("  Total sold:       %s %s @ avg %s %s  (proceeds: %s %s)\n",
			formatAmount(r.SoldQty), baseSymbol(symbol),
			fmt.Sprintf("%.4f", avgSellPrice), quote,
			fmt.Sprintf("%.4f", r.SoldProceeds), quote))
	}

	if r.Fees > 0 {
		sb.WriteString(fmt.Sprintf("  Total fees:       %s (currency varies by venue)\n", fmt.Sprintf("%.6f", r.Fees)))
	}

	sb.WriteString("\n")

	if r.SoldQty > 0 {
		sign := pnlSignStr(r.Realized)
		realPct := 0.0
		if r.BoughtCost > 0 {
			realPct = r.Realized / r.BoughtCost * 100
		}
		sb.WriteString(fmt.Sprintf("  Realized PnL:     %s%.4f %s  (%s%.2f%%)\n",
			sign, r.Realized, quote, sign, realPct))
	}

	if r.Held.Qty > 0 {
		sb.WriteString(fmt.Sprintf("  Currently held:   %s %s @ avg cost %s %s\n",
			formatAmount(r.Held.Qty), baseSymbol(symbol),
			fmt.Sprintf("%.4f", r.Held.AvgCost), quote))

		if currentPrice > 0 {
			mktValue := r.Held.Qty * currentPrice
			unrealized := (currentPrice - r.Held.AvgCost) * r.Held.Qty
			sign := pnlSignStr(unrealized)
			unrealPct := 0.0
			if r.Held.TotalCost > 0 {
				unrealPct = unrealized / r.Held.TotalCost * 100
			}
			sb.WriteString(fmt.Sprintf("  Current price:    %.4f %s\n", currentPrice, quote))
			sb.WriteString(fmt.Sprintf("  Market value:     %.4f %s\n", mktValue, quote))
			sb.WriteString(fmt.Sprintf("  Unrealized PnL:   %s%.4f %s  (%s%.2f%%)\n",
				sign, unrealized, quote, sign, unrealPct))

			if r.SoldQty > 0 {
				combined := r.Realized + unrealized
				combSign := pnlSignStr(combined)
				sb.WriteString(fmt.Sprintf("\n  Combined PnL:     %s%.4f %s\n", combSign, combined, quote))
			}
		} else {
			sb.WriteString("  Current price:    unavailable\n")
		}
	} else if r.TradeCount > 0 {
		sb.WriteString(fmt.Sprintf("  Currently held:   0 %s (fully closed within this window)\n", baseSymbol(symbol)))
	}

	// Provider-specific notes.
	if providerID == "bitkub" {
		sb.WriteString("\nNote: Bitkub trade history reconstructed from filled orders; fee amounts may be in the bought asset rather than the quote.\n")
	}
	if r.TruncatedAt200 {
		sb.WriteString(fmt.Sprintf("Note: Use a narrower 'since' window to retrieve fewer than %d trades and get a more accurate avg_cost.\n", maxPnLDetailLimit))
	}

	return UserResult(sb.String())
}

// baseSymbol extracts the base asset from a CCXT pair (e.g. "SOL/USDT" → "SOL").
func baseSymbol(symbol string) string {
	if idx := strings.Index(symbol, "/"); idx >= 0 {
		return symbol[:idx]
	}
	return symbol
}
