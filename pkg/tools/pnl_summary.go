package tools

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// GetPnLSummaryTool computes cost basis and unrealized (and optionally realized)
// PnL for all currently held assets across one or all exchange accounts.
type GetPnLSummaryTool struct {
	cfg *config.Config
}

func NewGetPnLSummaryTool(cfg *config.Config) *GetPnLSummaryTool {
	return &GetPnLSummaryTool{cfg: cfg}
}

func (t *GetPnLSummaryTool) Name() string { return NameGetPnLSummary }

func (t *GetPnLSummaryTool) Description() string {
	return "Compute cost basis and unrealized PnL for all currently held assets across one or all exchange accounts (Binance, Bitkub, OKX, Settrade). " +
		"Use list_portfolios first if exchange or account names are unclear. " +
		"Set include_realized=true to also compute realized PnL from trade history (slower)."
}

func (t *GetPnLSummaryTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"provider": map[string]any{
				"type":        "string",
				"description": "Exchange name (e.g. binance, bitkub, settrade). Omit to scan all enabled accounts.",
			},
			"account": map[string]any{
				"type":        "string",
				"description": "Account name within the provider. Omit for default.",
			},
			"quote": map[string]any{
				"type":        "string",
				"description": `Quote currency for PnL values. "auto" uses each venue's native quote (THB for bitkub/settrade, USDT otherwise). Default: "auto".`,
			},
			"assets": map[string]any{
				"type":        "string",
				"description": `Comma-separated asset filter, e.g. "BTC,ETH". Omit for all assets.`,
			},
			"include_realized": map[string]any{
				"type":        "boolean",
				"description": "Also compute realized PnL from trade history. Slower — issues one FetchMyTrades call per held asset.",
			},
		},
		"required": []string{},
	}
}

func (t *GetPnLSummaryTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	providerID, _ := args["provider"].(string)
	account, _ := args["account"].(string)
	quoteArg, _ := args["quote"].(string)
	if quoteArg == "" {
		quoteArg = "auto"
	}
	assetsArg, _ := args["assets"].(string)
	includeRealized, _ := args["include_realized"].(bool)

	assetFilter := map[string]bool{}
	for _, a := range strings.Split(assetsArg, ",") {
		a = strings.TrimSpace(strings.ToUpper(a))
		if a != "" {
			assetFilter[a] = true
		}
	}

	// Resolve target accounts.
	var targets []broker.AccountRef
	if providerID != "" {
		targets = []broker.AccountRef{{ProviderID: providerID, Account: account}}
	} else {
		targets = broker.ListConfiguredAccounts(t.cfg)
	}
	if len(targets) == 0 {
		return ErrorResult("get_pnl_summary: no configured exchange accounts found")
	}

	var sb strings.Builder
	sb.WriteString("PnL Summary")
	if providerID != "" {
		sb.WriteString(fmt.Sprintf(" — %s", providerID))
		if account != "" {
			sb.WriteString(fmt.Sprintf(" (%s)", account))
		}
	} else {
		sb.WriteString(" — all accounts")
	}
	sb.WriteString("\n")

	var bitkubNote, tradeCapNote bool

	for _, ref := range targets {
		p, err := broker.CreateProviderForAccount(ref.ProviderID, ref.Account, t.cfg)
		if err != nil {
			sb.WriteString(fmt.Sprintf("\n[!] %s (%s): provider unavailable: %v\n", ref.ProviderID, ref.Account, err))
			continue
		}

		pp, ok := p.(broker.PortfolioProvider)
		if !ok {
			sb.WriteString(fmt.Sprintf("\n[!] %s (%s): balance query not supported\n", ref.ProviderID, ref.Account))
			continue
		}

		q := quoteArg
		if q == "auto" {
			q = nativeQuoteForProvider(ref.ProviderID)
		}
		wt := walletTypeForPnL(ref.ProviderID)

		balances, err := pp.GetWalletBalances(ctx, wt)
		if err != nil {
			sb.WriteString(fmt.Sprintf("\n[!] %s (%s): GetWalletBalances: %v\n", ref.ProviderID, ref.Account, err))
			continue
		}

		// Filter to non-zero, non-quote assets matching optional filter.
		type row struct {
			asset        string
			qty          float64
			avgCost      float64
			price        float64
			mktValue     float64
			unrealized   float64
			unrealizedPct float64
			realized     float64
			fees         float64
			note         string
		}
		var rows []row
		var totalMktValue, totalUnrealized, totalRealized float64

		for _, b := range balances {
			asset := strings.ToUpper(b.Asset)
			qty := b.Free + b.Locked
			if qty <= 0 {
				continue
			}
			if strings.EqualFold(asset, q) {
				continue
			}
			if len(assetFilter) > 0 && !assetFilter[asset] {
				continue
			}

			var r row
			r.asset = asset
			r.qty = qty

			if ref.ProviderID == "settrade" {
				// Settrade already provides PnL fields.
				r.avgCost = parseExtraFloat(b.Extra, "avg_cost")
				r.price = parseExtraFloat(b.Extra, "market_price")
				r.mktValue = parseExtraFloat(b.Extra, "market_value")
				r.unrealized = parseExtraFloat(b.Extra, "unrealized_pl")
				pct := parseExtraFloat(b.Extra, "percent_profit")
				r.unrealizedPct = pct
			} else {
				// Compute from trade history.
				var tradeNote string
				sym := asset + "/" + q

				tp, hasTrades := p.(broker.TradingProvider)
				md, hasPrice := p.(broker.MarketDataProvider)

				var calcResult PnLResult
				if hasTrades {
					trades, terr := tp.FetchMyTrades(ctx, sym, nil, 200)
					if terr == nil {
						calcResult = ComputeAvgCost(trades)
						if calcResult.TruncatedAt200 {
							tradeNote = "200-fill cap"
							tradeCapNote = true
						}
					} else {
						tradeNote = "trade fetch failed"
					}
					if ref.ProviderID == "bitkub" {
						bitkubNote = true
					}
				} else {
					tradeNote = "trade history unavailable"
				}

				r.avgCost = calcResult.Held.AvgCost
				r.fees = calcResult.Fees
				if includeRealized {
					r.realized = calcResult.Realized
				}

				if hasPrice {
					ticker, terr := md.FetchTicker(ctx, sym)
					if terr == nil && ticker.Last != nil {
						r.price = *ticker.Last
					}
				}
				if r.price == 0 {
					// fallback to FetchPrice
					fp, ferr := pp.FetchPrice(ctx, asset, q)
					if ferr == nil && fp > 0 {
						r.price = fp
					}
				}

				if r.price > 0 {
					r.mktValue = qty * r.price
					if r.avgCost > 0 {
						r.unrealized = (r.price - r.avgCost) * calcResult.Held.Qty
						if r.avgCost*calcResult.Held.Qty > 0 {
							r.unrealizedPct = r.unrealized / (r.avgCost * calcResult.Held.Qty) * 100
						}
					}
				}
				r.note = tradeNote
			}

			rows = append(rows, r)
			totalMktValue += r.mktValue
			totalUnrealized += r.unrealized
			if includeRealized {
				totalRealized += r.realized
			}
		}

		// Render this account block.
		header := fmt.Sprintf("%s (%s)", ref.ProviderID, ref.Account)
		sb.WriteString(fmt.Sprintf("\n=== %s — %s ===\n\n", header, q))

		if len(rows) == 0 {
			sb.WriteString("  No holdings found.\n")
			continue
		}

		// Table header.
		sb.WriteString(fmt.Sprintf("%-10s %16s %16s %16s %16s %16s %9s",
			"Asset", "Qty", "Avg Cost", "Price", "Mkt Value", "Unrlz PnL", "Unrlz%"))
		if includeRealized {
			sb.WriteString(fmt.Sprintf(" %16s", "Realized"))
		}
		sb.WriteString("\n")
		sep := strings.Repeat("-", 10) + " " + strings.Repeat("-", 16)
		sepLine := sep + " " + sep + " " + sep + " " + sep + " " + sep + " " + strings.Repeat("-", 9)
		if includeRealized {
			sepLine += " " + strings.Repeat("-", 16)
		}
		sb.WriteString(sepLine + "\n")

		for _, rw := range rows {
			note := ""
			if rw.note != "" {
				note = " [" + rw.note + "]"
			}
			pnlSign := pnlSignStr(rw.unrealized)
			avgCostStr := "n/a"
			if rw.avgCost > 0 {
				avgCostStr = fmt.Sprintf("%.4f", rw.avgCost)
			}
			priceStr := "n/a"
			if rw.price > 0 {
				priceStr = fmt.Sprintf("%.4f", rw.price)
			}
			mktStr := "n/a"
			if rw.mktValue > 0 {
				mktStr = fmt.Sprintf("%.4f", rw.mktValue)
			}
			unrealStr := "n/a"
			pctStr := "n/a"
			if rw.avgCost > 0 && rw.price > 0 {
				unrealStr = fmt.Sprintf("%s%.4f", pnlSign, rw.unrealized)
				pctStr = fmt.Sprintf("%s%.2f%%", pnlSign, rw.unrealizedPct)
			}

			line := fmt.Sprintf("%-10s %16s %16s %16s %16s %16s %9s",
				rw.asset+note, formatAmount(rw.qty), avgCostStr, priceStr, mktStr, unrealStr, pctStr)
			if includeRealized {
				realSign := pnlSignStr(rw.realized)
				line += fmt.Sprintf(" %16s", fmt.Sprintf("%s%.4f", realSign, rw.realized))
			}
			sb.WriteString(line + "\n")
		}

		sb.WriteString(sepLine + "\n")

		// Subtotals.
		totalUnrealSign := pnlSignStr(totalUnrealized)
		totalUnrealPct := 0.0
		if totalMktValue-totalUnrealized > 0 {
			totalUnrealPct = totalUnrealized / (totalMktValue - totalUnrealized) * 100
		}
		sb.WriteString(fmt.Sprintf("\n  Market value:     %.4f %s\n", totalMktValue, q))
		sb.WriteString(fmt.Sprintf("  Unrealized PnL:   %s%.4f %s (%s%.2f%%)\n",
			totalUnrealSign, totalUnrealized, q, totalUnrealSign, totalUnrealPct))
		if includeRealized {
			realSign := pnlSignStr(totalRealized)
			sb.WriteString(fmt.Sprintf("  Realized PnL:     %s%.4f %s\n", realSign, totalRealized, q))
			combined := totalUnrealized + totalRealized
			combSign := pnlSignStr(combined)
			sb.WriteString(fmt.Sprintf("  Combined PnL:     %s%.4f %s\n", combSign, combined, q))
		}
		sb.WriteString(fmt.Sprintf("  As of: %s UTC\n", time.Now().UTC().Format("2006-01-02 15:04")))
	}

	// Notes section.
	var notes []string
	if bitkubNote {
		notes = append(notes, "Bitkub: trade history reconstructed from filled orders (no per-fill endpoint); fee amounts are in the order currency and may not be in the quote.")
	}
	if tradeCapNote {
		notes = append(notes, "Trade history capped at 200 fills per symbol — older trades are excluded; avg_cost may be inaccurate for long-held positions.")
	}
	if len(notes) > 0 {
		sb.WriteString("\nNotes:\n")
		for _, n := range notes {
			sb.WriteString("  • " + n + "\n")
		}
	}

	return UserResult(sb.String())
}

// nativeQuoteForProvider returns the default quote currency for a provider.
func nativeQuoteForProvider(providerID string) string {
	switch providerID {
	case "bitkub", "settrade":
		return "THB"
	default:
		return "USDT"
	}
}

// walletTypeForPnL returns the appropriate wallet type for PnL balance queries.
func walletTypeForPnL(providerID string) string {
	switch providerID {
	case "settrade":
		return "stock"
	default:
		return "spot"
	}
}

func parseExtraFloat(extra map[string]string, key string) float64 {
	if extra == nil {
		return 0
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(extra[key]), 64)
	if err != nil {
		return 0
	}
	return v
}

func pnlSignStr(f float64) string {
	if f >= 0 {
		return "+"
	}
	return ""
}
