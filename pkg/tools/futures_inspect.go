package tools

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

// --- futures_validate_market ---

type FuturesValidateMarketTool struct{ cfg *config.Config }

func NewFuturesValidateMarketTool(cfg *config.Config) *FuturesValidateMarketTool {
	return &FuturesValidateMarketTool{cfg: cfg}
}

func (t *FuturesValidateMarketTool) Name() string        { return NameFuturesValidateMarket }
func (t *FuturesValidateMarketTool) Description() string { return DescFuturesValidateMarket }

func (t *FuturesValidateMarketTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"provider": map[string]any{"type": "string", "description": "binance or okx."},
			"account":  map[string]any{"type": "string", "description": "Account name (empty = default)."},
			"symbol":   map[string]any{"type": "string", "description": "Futures symbol, e.g. BTC/USDT:USDT."},
		},
		"required": []string{"provider", "symbol"},
	}
}

func (t *FuturesValidateMarketTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	providerID := stringArg(args, "provider")
	account := stringArg(args, "account")
	symbol := normalizeFuturesSymbol(stringArg(args, "symbol"))
	if providerID == "" || symbol == "" {
		return ErrorResult("provider and symbol are required")
	}
	fp, err := futuresProvider(ctx, t.cfg, providerID, account)
	if err != nil {
		return ErrorResult(err.Error()).WithError(err)
	}
	markets, err := fp.LoadFuturesMarkets(ctx)
	if err != nil {
		return ErrorResult(fmt.Sprintf("futures_validate_market: %v", err)).WithError(err)
	}
	m, ok := markets[symbol]
	if !ok {
		return ErrorResult(fmt.Sprintf("symbol %q not found in %s futures market catalogue", symbol, providerID))
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Futures market validation for %s on %s:\n", symbol, providerID))
	sb.WriteString(fmt.Sprintf("  Symbol:        %s\n", symbol))
	if m.Type != nil {
		sb.WriteString(fmt.Sprintf("  Type:          %s\n", *m.Type))
	}
	active := m.Active != nil && *m.Active
	sb.WriteString(fmt.Sprintf("  Active:        %v\n", active))
	if m.Swap != nil {
		sb.WriteString(fmt.Sprintf("  Swap:          %v\n", *m.Swap))
	}
	if m.Linear != nil {
		sb.WriteString(fmt.Sprintf("  Linear:        %v\n", *m.Linear))
	}
	if m.Settle != nil {
		sb.WriteString(fmt.Sprintf("  Settle:        %s\n", *m.Settle))
	}
	// Parse base currency from symbol (e.g. "BTC/USDT:USDT" → "BTC")
	baseCurrency := ""
	if idx := strings.Index(symbol, "/"); idx > 0 {
		baseCurrency = symbol[:idx]
	}

	contractSize := 0.0
	if m.ContractSize != nil {
		contractSize = *m.ContractSize
		baseUnit := baseCurrency
		if baseUnit == "" {
			baseUnit = "base"
		}
		sb.WriteString(fmt.Sprintf("  Contract size: %.8g %s per contract\n", contractSize, baseUnit))
	}
	if m.Taker != nil {
		sb.WriteString(fmt.Sprintf("  Taker fee:     %.4f%%\n", *m.Taker*100))
	}
	if m.Maker != nil {
		sb.WriteString(fmt.Sprintf("  Maker fee:     %.4f%%\n", *m.Maker*100))
	}
	if m.Limits.Amount.Min != nil {
		minContracts := *m.Limits.Amount.Min
		if contractSize > 0 && baseCurrency != "" {
			minBase := minContracts * contractSize
			sb.WriteString(fmt.Sprintf("  Min amount:    %.8g contracts (= %.8g %s)\n", minContracts, minBase, baseCurrency))
		} else {
			sb.WriteString(fmt.Sprintf("  Min amount:    %.8g contracts\n", minContracts))
		}
	}
	if m.Limits.Cost.Min != nil {
		settle := "USDT"
		if m.Settle != nil {
			settle = *m.Settle
		}
		sb.WriteString(fmt.Sprintf("  Min cost:      %.8g %s\n", *m.Limits.Cost.Min, settle))
	}
	if m.Limits.Leverage.Min != nil && m.Limits.Leverage.Max != nil {
		sb.WriteString(fmt.Sprintf("  Leverage:      %.0f–%.0f\n", *m.Limits.Leverage.Min, *m.Limits.Leverage.Max))
	}

	// Validation verdict
	var issues []string
	if !active {
		issues = append(issues, "market is not active")
	}
	if m.Swap == nil || !*m.Swap {
		issues = append(issues, "not a perpetual swap")
	}
	if len(issues) == 0 {
		sb.WriteString("\n  Symbol is valid for futures trading\n")
	} else {
		sb.WriteString(fmt.Sprintf("\n  Symbol cannot be traded: %s\n", strings.Join(issues, "; ")))
		return ErrorResult(sb.String())
	}
	return UserResult(sb.String())
}

// --- futures_risk_summary ---

type FuturesRiskSummaryTool struct{ cfg *config.Config }

func NewFuturesRiskSummaryTool(cfg *config.Config) *FuturesRiskSummaryTool {
	return &FuturesRiskSummaryTool{cfg: cfg}
}

func (t *FuturesRiskSummaryTool) Name() string        { return NameFuturesRiskSummary }
func (t *FuturesRiskSummaryTool) Description() string { return DescFuturesRiskSummary }

func (t *FuturesRiskSummaryTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"provider": map[string]any{"type": "string", "description": "binance or okx."},
			"account":  map[string]any{"type": "string"},
			"symbol":   map[string]any{"type": "string", "description": "Optional symbol filter."},
		},
		"required": []string{"provider"},
	}
}

func (t *FuturesRiskSummaryTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	providerID := stringArg(args, "provider")
	account := stringArg(args, "account")
	symbol := normalizeFuturesSymbol(stringArg(args, "symbol"))
	if providerID == "" {
		return ErrorResult("provider is required")
	}
	fp, err := futuresProvider(ctx, t.cfg, providerID, account)
	if err != nil {
		return ErrorResult(err.Error()).WithError(err)
	}
	var symbols []string
	if symbol != "" {
		symbols = []string{symbol}
	}
	positions, err := fp.FetchFuturesPositions(ctx, symbols)
	if err != nil {
		return ErrorResult(fmt.Sprintf("futures_risk_summary: %v", err)).WithError(err)
	}

	type posRow struct {
		sym, side, marginMode string
		contracts, lev, entry float64
		mark, liq, upnl       float64
		distPct, marginRatioPct float64
		label                 string
	}

	var rows []posRow
	sizes := &positionContractSizeResolver{fp: fp}
	var totalNotional, totalUnrealPnl float64
	var worstDist float64 = math.MaxFloat64
	var worstMarginRatio float64

	for _, p := range positions {
		if p.Contracts == nil || *p.Contracts == 0 {
			continue
		}
		distPct, marginRatioPct, label := marginHealthFromPosition(p)
		row := posRow{
			sym:            futuresStrPtr(p.Symbol),
			side:           futuresStrPtr(p.Side),
			marginMode:     futuresStrPtr(p.MarginMode),
			distPct:        distPct,
			marginRatioPct: marginRatioPct,
			label:          label,
		}
		if p.Contracts != nil {
			row.contracts = *p.Contracts
		}
		if p.Leverage != nil {
			row.lev = *p.Leverage
		}
		if p.EntryPrice != nil {
			row.entry = *p.EntryPrice
		}
		if p.MarkPrice != nil {
			row.mark = *p.MarkPrice
		}
		if p.LiquidationPrice != nil {
			row.liq = *p.LiquidationPrice
		}
		if p.UnrealizedPnl != nil {
			row.upnl = *p.UnrealizedPnl
		}
		if row.mark > 0 && row.contracts != 0 {
			// Contracts is a contract count: value it with the contract size (position's own,
			// else the market's; unresolved is treated as 1 for this display-only total).
			cs, _ := sizes.size(ctx, p, normalizeFuturesSymbol(row.sym))
			totalNotional += positionNotionalUSD(row.contracts, cs, row.mark)
		}
		totalUnrealPnl += row.upnl
		if distPct > 0 && distPct < worstDist {
			worstDist = distPct
		}
		if marginRatioPct > worstMarginRatio {
			worstMarginRatio = marginRatioPct
		}
		rows = append(rows, row)
	}

	if len(rows) == 0 {
		return UserResult(fmt.Sprintf("No active futures positions on %s.", providerID))
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Futures risk summary on %s (%d positions):\n\n", providerID, len(rows)))
	sb.WriteString(fmt.Sprintf("  Total notional:       %.2f USD\n", totalNotional))
	sb.WriteString(fmt.Sprintf("  Total unrealized PnL: %.4f\n", totalUnrealPnl))
	if worstDist < math.MaxFloat64 {
		sb.WriteString(fmt.Sprintf("  Worst liq. distance:  %.2f%%\n", worstDist))
	}
	if worstMarginRatio > 0 {
		sb.WriteString(fmt.Sprintf("  Worst margin ratio:   %.2f%%\n", worstMarginRatio))
	}
	sb.WriteString("\n")
	sb.WriteString(fmt.Sprintf("  %-18s %-6s %10s %6s %10s %10s %10s %8s %6s %s\n",
		"Symbol", "Side", "Size", "Lev", "Entry", "Mark", "Liq", "Dist%", "Margin%", "Risk"))
	for _, r := range rows {
		liqStr := "-"
		if r.liq > 0 {
			liqStr = fmt.Sprintf("%.4g", r.liq)
		}
		distStr := "-"
		if r.distPct > 0 {
			distStr = fmt.Sprintf("%.2f", r.distPct)
		}
		mRatioStr := "-"
		if r.marginRatioPct > 0 {
			mRatioStr = fmt.Sprintf("%.2f", r.marginRatioPct)
		}
		sb.WriteString(fmt.Sprintf("  %-18s %-6s %10.4g %6.0f %10.4g %10.4g %10s %8s %6s %s\n",
			r.sym, r.side, r.contracts, r.lev, r.entry, r.mark, liqStr, distStr, mRatioStr, r.label))
		sb.WriteString(fmt.Sprintf("    unrealized PnL: %.4f  margin: %s\n", r.upnl, r.marginMode))
	}
	return UserResult(sb.String())
}

// --- futures_estimate_funding_fee ---

type FuturesEstimateFundingFeeTool struct{ cfg *config.Config }

func NewFuturesEstimateFundingFeeTool(cfg *config.Config) *FuturesEstimateFundingFeeTool {
	return &FuturesEstimateFundingFeeTool{cfg: cfg}
}

func (t *FuturesEstimateFundingFeeTool) Name() string        { return NameFuturesEstimateFundingFee }
func (t *FuturesEstimateFundingFeeTool) Description() string { return DescFuturesEstimateFundingFee }

func (t *FuturesEstimateFundingFeeTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"provider": map[string]any{"type": "string", "description": "binance or okx."},
			"account":  map[string]any{"type": "string"},
			"symbol":   map[string]any{"type": "string", "description": "Futures symbol. If omitted and account set, estimates for all open positions."},
		},
		"required": []string{"provider"},
	}
}

func (t *FuturesEstimateFundingFeeTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	providerID := stringArg(args, "provider")
	account := stringArg(args, "account")
	symbol := normalizeFuturesSymbol(stringArg(args, "symbol"))
	if providerID == "" {
		return ErrorResult("provider is required")
	}
	fp, err := futuresProvider(ctx, t.cfg, providerID, account)
	if err != nil {
		return ErrorResult(err.Error()).WithError(err)
	}

	type fundingRow struct {
		symbol    string
		rate      float64
		notional  float64
		estimated float64
		nextTime  string
		side      string
	}

	var rows []fundingRow
	// Contracts is a contract count; value it with the contract size (position's own,
	// else the market's). An unresolved size is treated as 1 for this display-only estimate.
	sizes := &positionContractSizeResolver{fp: fp}

	if symbol != "" {
		// Single symbol estimate
		rate, err := fp.FetchFuturesFundingRate(ctx, symbol)
		if err != nil {
			return ErrorResult(fmt.Sprintf("futures_estimate_funding_fee: %v", err)).WithError(err)
		}
		row := fundingRow{symbol: symbol, side: "unknown"}
		if rate.FundingRate != nil {
			row.rate = *rate.FundingRate
		}
		if rate.NextFundingTimestamp != nil && *rate.NextFundingTimestamp > 0 {
			row.nextTime = time.UnixMilli(int64(*rate.NextFundingTimestamp)).UTC().Format(time.RFC3339)
		}
		// Try to get position size for notional
		positions, posErr := fp.FetchFuturesPositions(ctx, []string{symbol})
		if posErr == nil {
			for _, p := range positions {
				if p.Contracts == nil || *p.Contracts == 0 {
					continue
				}
				if p.Symbol != nil && normalizeFuturesSymbol(*p.Symbol) == symbol {
					if p.MarkPrice != nil {
						cs, _ := sizes.size(ctx, p, symbol)
						row.notional = positionNotionalUSD(*p.Contracts, cs, *p.MarkPrice)
					}
					if p.Side != nil {
						row.side = *p.Side
					}
					break
				}
			}
		}
		if row.notional > 0 {
			row.estimated = row.notional * row.rate
		}
		rows = append(rows, row)
	} else {
		// All open positions
		positions, err := fp.FetchFuturesPositions(ctx, nil)
		if err != nil {
			return ErrorResult(fmt.Sprintf("futures_estimate_funding_fee: %v", err)).WithError(err)
		}
		for _, p := range positions {
			if p.Contracts == nil || *p.Contracts == 0 || p.Symbol == nil {
				continue
			}
			sym := normalizeFuturesSymbol(*p.Symbol)
			rate, rErr := fp.FetchFuturesFundingRate(ctx, sym)
			if rErr != nil {
				continue
			}
			row := fundingRow{symbol: sym, side: futuresStrPtr(p.Side)}
			if rate.FundingRate != nil {
				row.rate = *rate.FundingRate
			}
			if rate.NextFundingTimestamp != nil && *rate.NextFundingTimestamp > 0 {
				row.nextTime = time.UnixMilli(int64(*rate.NextFundingTimestamp)).UTC().Format(time.RFC3339)
			}
			if p.MarkPrice != nil {
				cs, _ := sizes.size(ctx, p, sym)
				row.notional = positionNotionalUSD(*p.Contracts, cs, *p.MarkPrice)
			}
			if row.notional > 0 {
				row.estimated = row.notional * row.rate
			}
			rows = append(rows, row)
		}
	}

	if len(rows) == 0 {
		return UserResult(fmt.Sprintf("No open futures positions on %s to estimate funding for.", providerID))
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Funding fee estimate on %s:\n\n", providerID))
	sb.WriteString(fmt.Sprintf("  %-18s %-6s %12s %12s %12s %s\n", "Symbol", "Side", "Rate", "Notional", "Est. Fee", "Next Funding"))
	var totalFee float64
	for _, r := range rows {
		feeSign := "+"
		if r.estimated < 0 {
			feeSign = ""
		}
		sb.WriteString(fmt.Sprintf("  %-18s %-6s %12.6f %12.2f %s%12.4f  %s\n",
			r.symbol, r.side, r.rate, r.notional, feeSign, r.estimated, r.nextTime))
		totalFee += r.estimated
	}
	if len(rows) > 1 {
		sb.WriteString(fmt.Sprintf("\n  Total estimated next funding fee: %.4f\n", totalFee))
	}
	sb.WriteString("\nNote: funding fees are positive = you pay, negative = you receive.\n")
	return UserResult(sb.String())
}
