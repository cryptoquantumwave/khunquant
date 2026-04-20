package tools

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

const concentrationWarningPct = 30.0

// PortfolioAllocationTool computes portfolio allocation across all configured accounts.
type PortfolioAllocationTool struct {
	cfg *config.Config
}

func NewPortfolioAllocationTool(cfg *config.Config) *PortfolioAllocationTool {
	return &PortfolioAllocationTool{cfg: cfg}
}

func (t *PortfolioAllocationTool) Name() string { return NamePortfolioAllocation }

func (t *PortfolioAllocationTool) Description() string {
	return "Compute portfolio allocation weights across all configured accounts. Shows per-asset and per-exchange distribution, and flags assets with concentration above 30%."
}

func (t *PortfolioAllocationTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"quote": map[string]any{
				"type":        "string",
				"description": "Quote currency for valuation (default: USDT).",
			},
		},
		"required": []string{},
	}
}

func (t *PortfolioAllocationTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	quote, _ := args["quote"].(string)
	if quote == "" {
		quote = "USDT"
	}

	accounts := broker.ListConfiguredAccounts(t.cfg)
	if len(accounts) == 0 {
		return UserResult("No configured accounts found.")
	}

	type holding struct {
		asset    string
		provider string
		value    float64
	}

	var holdings []holding
	var errs []string
	totalValue := 0.0

	for _, ref := range accounts {
		p, err := broker.CreateProviderForAccount(ref.ProviderID, ref.Account, t.cfg)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s/%s: %v", ref.ProviderID, ref.Account, err))
			continue
		}

		pp, ok := p.(broker.PortfolioProvider)
		if !ok {
			continue
		}

		bals, err := pp.GetBalances(ctx)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s/%s: GetBalances: %v", ref.ProviderID, ref.Account, err))
			continue
		}

		for _, b := range bals {
			qty := b.Free + b.Locked
			if qty == 0 {
				continue
			}
			price, err := pp.FetchPrice(ctx, b.Asset, quote)
			value := 0.0
			if err == nil {
				if price == 0 {
					value = qty // 1:1 stablecoin
				} else {
					value = qty * price
				}
			}
			holdings = append(holdings, holding{
				asset:    b.Asset,
				provider: ref.ProviderID,
				value:    value,
			})
			totalValue += value
		}
	}

	if len(holdings) == 0 {
		return UserResult("No holdings found across configured accounts.")
	}

	// Aggregate by asset.
	assetMap := map[string]float64{}
	for _, h := range holdings {
		assetMap[h.asset] += h.value
	}
	// Aggregate by provider.
	provMap := map[string]float64{}
	for _, h := range holdings {
		provMap[h.provider] += h.value
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Portfolio Allocation (quote: %s, total: %.2f)\n\n", quote, totalValue))

	// Per-asset allocation.
	sb.WriteString("By Asset:\n")
	sb.WriteString(fmt.Sprintf("%-12s  %14s  %8s  %s\n", "Asset", "Value", "Weight%", "Warning"))
	sb.WriteString(fmt.Sprintf("%-12s  %14s  %8s  %s\n", strings.Repeat("-", 12), strings.Repeat("-", 14), strings.Repeat("-", 8), strings.Repeat("-", 10)))

	type assetRow struct {
		asset string
		value float64
	}
	var assetRows []assetRow
	for asset, val := range assetMap {
		assetRows = append(assetRows, assetRow{asset, val})
	}
	sort.Slice(assetRows, func(i, j int) bool { return assetRows[i].value > assetRows[j].value })

	var warnings []string
	for _, row := range assetRows {
		weight := 0.0
		if totalValue > 0 {
			weight = row.value / totalValue * 100
		}
		warn := ""
		if weight > concentrationWarningPct {
			warn = "⚠ HIGH"
			warnings = append(warnings, fmt.Sprintf("%s at %.1f%%", row.asset, weight))
		}
		sb.WriteString(fmt.Sprintf("%-12s  %14.2f  %7.2f%%  %s\n", row.asset, row.value, weight, warn))
	}

	// Per-exchange allocation.
	sb.WriteString("\nBy Exchange:\n")
	sb.WriteString(fmt.Sprintf("%-16s  %14s  %8s\n", "Exchange", "Value", "Weight%"))
	sb.WriteString(fmt.Sprintf("%-16s  %14s  %8s\n", strings.Repeat("-", 16), strings.Repeat("-", 14), strings.Repeat("-", 8)))

	type provRow struct {
		prov  string
		value float64
	}
	var provRows []provRow
	for prov, val := range provMap {
		provRows = append(provRows, provRow{prov, val})
	}
	sort.Slice(provRows, func(i, j int) bool { return provRows[i].value > provRows[j].value })
	for _, row := range provRows {
		weight := 0.0
		if totalValue > 0 {
			weight = row.value / totalValue * 100
		}
		sb.WriteString(fmt.Sprintf("%-16s  %14.2f  %7.2f%%\n", row.prov, row.value, weight))
	}

	if len(warnings) > 0 {
		sb.WriteString("\n⚠ Concentration warnings (>30%):\n")
		for _, w := range warnings {
			sb.WriteString("  - " + w + "\n")
		}
	}

	if len(errs) > 0 {
		sb.WriteString("\nNote: some accounts had errors:\n")
		for _, e := range errs {
			sb.WriteString("  - " + e + "\n")
		}
	}

	return UserResult(sb.String())
}
