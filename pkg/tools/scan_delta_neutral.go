package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	ccxt "github.com/ccxt/ccxt/go/v4"
	"golang.org/x/sync/errgroup"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
	"github.com/cryptoquantumwave/khunquant/pkg/utils"
)

// cmcListingFn is a package-level test seam for CMC listing.
var cmcListingFn = func(ctx context.Context, cfg *config.Config, baseURL string, topN int) ([]string, error) {
	return fetchCMCListing(ctx, baseURL, topN)
}

// futuresCapableProviders lists the exchanges the scanner can screen for
// delta-neutral funding opportunities. An empty or "all" provider argument scans
// every entry here and combines the results. To support a new exchange, add its
// provider name here (it must implement broker.FuturesProvider) — no other change
// to the scanner is required.
var futuresCapableProviders = []string{"binance", "okx"}

// resolveScanProviders maps the user's provider argument to the concrete list to
// scan: empty or "all" (case-insensitive) → every futures-capable provider;
// otherwise the single named provider.
func resolveScanProviders(provider string) []string {
	p := strings.ToLower(strings.TrimSpace(provider))
	if p == "" || p == "all" {
		out := make([]string, len(futuresCapableProviders))
		copy(out, futuresCapableProviders)
		return out
	}
	return []string{p}
}

type ScanDeltaNeutralOpportunitiesTool struct {
	cfg *config.Config
}

func NewScanDeltaNeutralOpportunitiesTool(cfg *config.Config) *ScanDeltaNeutralOpportunitiesTool {
	return &ScanDeltaNeutralOpportunitiesTool{cfg: cfg}
}

func (t *ScanDeltaNeutralOpportunitiesTool) Name() string {
	return NameScanDeltaNeutralOpportunities
}

func (t *ScanDeltaNeutralOpportunitiesTool) Description() string {
	return DescScanDeltaNeutralOpportunities
}

func (t *ScanDeltaNeutralOpportunitiesTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"provider": map[string]any{
				"type":        "string",
				"description": "Exchange provider name: 'binance' or 'okx'. Leave empty or pass 'all' to scan every supported exchange and combine the ranked results.",
			},
			"account": map[string]any{
				"type":        "string",
				"description": "Account name within the provider. Leave empty to use the default account.",
			},
			"top_n": map[string]any{
				"type":        "integer",
				"description": "Top N crypto assets by CMC market-cap rank to screen (default 100, max 500).",
			},
			"quote": map[string]any{
				"type":        "string",
				"description": "Quote currency for futures symbols, e.g. 'USDT' (default 'USDT').",
			},
			"limit_results": map[string]any{
				"type":        "integer",
				"description": "Limit output table to top N results (default 20).",
			},
			"include_stability": map[string]any{
				"type":        "boolean",
				"description": "Fetch funding rate history and compute stability stats for top K assets (default true).",
			},
			"top_k_stability": map[string]any{
				"type":        "integer",
				"description": "For stage 2, fetch history for top K ranked assets (default 15).",
			},
			"min_abs_funding_apr": map[string]any{
				"type":        "number",
				"description": "Filter assets with |APR| below this threshold (percent, optional, default 0).",
			},
			"sort_by": map[string]any{
				"type":        "string",
				"enum":        []string{"funding_rate", "apr", "7d_avg", "14d_avg"},
				"description": "Field to sort by (default 'funding_rate'). Values are SIGNED: 'funding_rate'/'apr' use the current funding/APR; '7d_avg'/'14d_avg' use the funding-history mean. Sorting by '7d_avg' or '14d_avg' computes stability for ALL candidates (more API calls).",
			},
			"sort_order": map[string]any{
				"type":        "string",
				"enum":        []string{"asc", "desc"},
				"description": "Sort direction (default 'desc'). 'desc' = most-positive first → most-negative last; 'asc' = most-negative first → most-positive last.",
			},
			"cmc_base_url": map[string]any{
				"type":        "string",
				"description": "Override CMC API base URL (for testing or custom endpoints).",
			},
		},
		"required": []string{"provider"},
	}
}

type opportunityRow struct {
	rank               int
	exchange           string
	asset              string
	symbol             string
	spotSymbol         string
	spotStatus         string // "yes", "no-spot", or "unknown"
	fundingPercent     float64
	apr                float64
	direction          string
	stability7dMean    *float64 // nil if not fetched
	stability7dStddev  *float64
	stability14dMean   *float64
	stability14dStddev *float64
	label              string
}

func (t *ScanDeltaNeutralOpportunitiesTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	provider := stringArg(args, "provider")
	account := stringArg(args, "account")
	topN := int(numberArg(args, "top_n"))
	quote := stringArg(args, "quote")
	limitResults := int(numberArg(args, "limit_results"))
	includeStability := boolArgWithDefault(args, "include_stability", true)
	topKStability := int(numberArg(args, "top_k_stability"))
	minAbsFundingAPR := numberArg(args, "min_abs_funding_apr")
	cmcBaseURL := stringArg(args, "cmc_base_url")
	sortBy := strings.ToLower(strings.TrimSpace(stringArg(args, "sort_by")))
	sortOrder := strings.ToLower(strings.TrimSpace(stringArg(args, "sort_order")))

	// Validation and defaults.
	// Empty or "all" provider → scan every supported exchange and combine.
	scanProviders := resolveScanProviders(provider)
	if sortBy == "" {
		sortBy = "funding_rate"
	}
	if !validSortBy(sortBy) {
		return ErrorResult(fmt.Sprintf("invalid sort_by %q (valid: funding_rate, apr, 7d_avg, 14d_avg)", sortBy))
	}
	if sortOrder == "" {
		sortOrder = "desc"
	}
	if sortOrder != "asc" && sortOrder != "desc" {
		return ErrorResult(fmt.Sprintf("invalid sort_order %q (valid: asc, desc)", sortOrder))
	}
	// Sorting by a stability field requires stability stats for every candidate.
	sortByStability := sortBy == "7d_avg" || sortBy == "14d_avg"
	if sortByStability {
		includeStability = true
	}
	if topN <= 0 || topN > 500 {
		topN = 100
	}
	if quote == "" {
		quote = "USDT"
	}
	if limitResults <= 0 {
		limitResults = 20
	}
	if topKStability <= 0 {
		topKStability = 15
	}
	if cmcBaseURL == "" {
		cmcBaseURL = "https://api.coinmarketcap.com/data-api/v3/cryptocurrency/listing"
	}

	// Stage 0: Fetch CMC listing.
	symbols, err := cmcListingFn(ctx, t.cfg, cmcBaseURL, topN)
	if err != nil {
		return ErrorResult(fmt.Sprintf("CMC listing fetch failed: %v", err)).WithError(err)
	}
	if len(symbols) == 0 {
		return UserResult("No symbols found in CMC listing.")
	}

	// Stage 1: Scan each requested provider and combine the results. A per-symbol
	// provider handle is retained so stage 2 (history stability) can fetch from the
	// right exchange when scanning "all".
	opportunities := make([]opportunityRow, 0)
	provHandles := make(map[string]broker.FuturesProvider) // exchange -> provider handle
	var providerErrs []string
	for _, prov := range scanProviders {
		rows, fp, err := t.scanProvider(ctx, prov, account, quote, symbols, minAbsFundingAPR)
		if err != nil {
			providerErrs = append(providerErrs, fmt.Sprintf("%s: %v", prov, err))
			continue
		}
		provHandles[prov] = fp
		opportunities = append(opportunities, rows...)
	}

	// If every requested provider failed, surface the error.
	if len(opportunities) == 0 && len(providerErrs) > 0 {
		return ErrorResult(fmt.Sprintf("scan failed for all providers: %s", strings.Join(providerErrs, "; ")))
	}

	// Pre-rank by abs(APR) descending — this only decides WHICH rows get stability
	// fetched in the top-K case; the final user-facing sort is applied after stage 2.
	sort.Slice(opportunities, func(i, j int) bool {
		return math.Abs(opportunities[i].apr) > math.Abs(opportunities[j].apr)
	})

	// Stage 2: Optionally fetch stability (across all exchanges) using each row's
	// own provider handle. When sorting by a stability field, fetch for ALL
	// candidates (bounded by maxStabilityFetch) so the final sort is correct;
	// otherwise just the top-K most-interesting rows.
	if includeStability && len(opportunities) > 0 {
		eg, egCtx := errgroup.WithContext(ctx)
		eg.SetLimit(4)

		topK := topKStability
		if sortByStability {
			topK = len(opportunities)
			if topK > maxStabilityFetch {
				topK = maxStabilityFetch
			}
		}
		if len(opportunities) < topK {
			topK = len(opportunities)
		}

		mu := sync.Mutex{}
		for i := 0; i < topK; i++ {
			i := i // capture
			fp := provHandles[opportunities[i].exchange]
			if fp == nil {
				continue
			}
			eg.Go(func() error {
				history, err := fp.FetchPublicFundingRateHistory(egCtx, opportunities[i].symbol, nil, 200)
				if err != nil || len(history) == 0 {
					return nil // Silently skip on error / no data.
				}

				stats7d := computeFundingStatsWindow(history, 7*24*time.Hour)
				stats14d := computeFundingStatsWindow(history, 14*24*time.Hour)

				mu.Lock()
				opportunities[i].stability7dMean = &stats7d.mean
				opportunities[i].stability7dStddev = &stats7d.stddev
				opportunities[i].stability14dMean = &stats14d.mean
				opportunities[i].stability14dStddev = &stats14d.stddev
				mu.Unlock()

				return nil
			})
		}
		_ = eg.Wait() // Ignore partial failures.
	}

	// Final user-facing sort (after stage 2 so stability values are populated).
	sortOpportunities(opportunities, sortBy, sortOrder)

	if len(opportunities) == 0 {
		return UserResult("No opportunities found (opportunities list is empty).")
	}
	out := formatScanResults(opportunities, limitResults, includeStability, scanProviders, providerErrs, sortBy, sortOrder)
	return UserResult(out)
}

// maxStabilityFetch caps how many history fetches a stability-field sort triggers.
const maxStabilityFetch = 200

// validSortBy reports whether s is a recognized sort field.
func validSortBy(s string) bool {
	switch s {
	case "funding_rate", "apr", "7d_avg", "14d_avg":
		return true
	}
	return false
}

// sortOpportunities orders rows by the chosen signed field and direction.
// Rows missing a stability value (nil) always sink to the bottom regardless of
// direction, so rows without data never outrank rows with real data. Ties break
// by abs(apr) desc then symbol for deterministic output.
func sortOpportunities(rows []opportunityRow, sortBy, sortOrder string) {
	// key returns (value, present). present=false → sink to bottom.
	key := func(r opportunityRow) (float64, bool) {
		switch sortBy {
		case "apr":
			return r.apr, true
		case "7d_avg":
			if r.stability7dMean == nil {
				return 0, false
			}
			return *r.stability7dMean, true
		case "14d_avg":
			if r.stability14dMean == nil {
				return 0, false
			}
			return *r.stability14dMean, true
		default: // funding_rate
			return r.fundingPercent, true
		}
	}
	desc := sortOrder != "asc"
	sort.SliceStable(rows, func(i, j int) bool {
		vi, pi := key(rows[i])
		vj, pj := key(rows[j])
		if pi != pj {
			return pi // present rows before absent rows, both directions
		}
		if !pi { // both absent → deterministic tiebreak
			return tieBreak(rows[i], rows[j])
		}
		if vi != vj {
			if desc {
				return vi > vj
			}
			return vi < vj
		}
		return tieBreak(rows[i], rows[j])
	})
}

// tieBreak gives a stable deterministic order: larger abs(apr) first, then symbol.
func tieBreak(a, b opportunityRow) bool {
	ai, bi := math.Abs(a.apr), math.Abs(b.apr)
	if ai != bi {
		return ai > bi
	}
	return a.symbol < b.symbol
}

// scanProvider screens a single exchange: loads its futures + spot markets, batch-
// fetches funding rates for the candidate symbols, and returns ranked-but-unsorted
// opportunity rows tagged with the exchange, plus the provider handle (for stage 2).
func (t *ScanDeltaNeutralOpportunitiesTool) scanProvider(
	ctx context.Context,
	provider, account, quote string,
	symbols []string,
	minAbsFundingAPR float64,
) ([]opportunityRow, broker.FuturesProvider, error) {
	fp, err := futuresProvider(ctx, t.cfg, provider, account)
	if err != nil {
		return nil, nil, err
	}

	markets, _ := fp.LoadFuturesMarkets(ctx) // Silently ignore error; proceed without filtering.

	// Load spot markets to flag whether each asset also has a spot pair on this
	// exchange. We do NOT filter on this — symbols without spot stay in the list
	// with a caution flag. spotMarkets == nil → spot status "unknown".
	var spotMarkets map[string]ccxt.MarketInterface
	if md, ok := fp.(broker.MarketDataProvider); ok {
		spotMarkets, _ = md.LoadMarkets(ctx)
	}

	// Build candidate symbols (preserving CMC rank index) and filter active/swap.
	candidateSymbols := make([]string, 0, len(symbols))
	symbolToBase := make(map[string]string)
	symbolToIdx := make(map[string]int)
	for i, base := range symbols {
		futSym := normalizeFuturesSymbol(base + "/" + quote)
		symbolToBase[futSym] = base
		symbolToIdx[futSym] = i
		if len(markets) > 0 {
			m, exists := markets[futSym]
			if !exists || !isActiveSwap(m) {
				continue // No active perp on this exchange.
			}
		}
		candidateSymbols = append(candidateSymbols, futSym)
	}

	rates, err := fp.FetchFuturesFundingRates(ctx, candidateSymbols)
	if err != nil {
		return nil, nil, fmt.Errorf("batch funding fetch: %w", err)
	}

	rows := make([]opportunityRow, 0, len(candidateSymbols))
	for _, futSym := range candidateSymbols {
		rate, exists := rates[futSym]
		if !exists || rate.FundingRate == nil {
			continue
		}

		fr := *rate.FundingRate
		apr := fr * periodsPerDay(rate.Interval) * 365 * 100 // percent

		if minAbsFundingAPR != 0 && math.Abs(apr) < minAbsFundingAPR {
			continue
		}

		base := symbolToBase[futSym]
		dir := "short perp"
		if fr < 0 {
			dir = "long perp"
		}

		spotSym := base + "/" + quote
		spotStatus := spotStatusFor(spotMarkets, spotSym)

		row := opportunityRow{
			rank:           symbolToIdx[futSym] + 1,
			exchange:       provider,
			asset:          base,
			symbol:         futSym,
			spotSymbol:     spotSym,
			spotStatus:     spotStatus,
			fundingPercent: fr * 100,
			apr:            apr,
			direction:      dir,
			label:          "watch",
		}
		// Positive carry → attractive, unless there's no spot leg available here.
		if apr > 0 && spotStatus != "no-spot" {
			row.label = "attractive"
		}

		rows = append(rows, row)
	}

	return rows, fp, nil
}

// spotStatusFor reports whether a spot pair exists on the exchange for the given
// spot symbol (e.g. "BTC/USDT"). Returns:
//   - "unknown" when spot markets could not be loaded (don't claim "no spot" falsely),
//   - "no-spot" when markets loaded but the symbol is absent or not an active spot pair,
//   - "yes" when an active spot market exists.
func spotStatusFor(spotMarkets map[string]ccxt.MarketInterface, spotSymbol string) string {
	if spotMarkets == nil {
		return "unknown"
	}
	m, exists := spotMarkets[spotSymbol]
	if !exists {
		return "no-spot"
	}
	if m.Active != nil && !*m.Active {
		return "no-spot"
	}
	if m.Spot != nil && !*m.Spot {
		return "no-spot"
	}
	return "yes"
}

// isActiveSwap checks if a market is an active swap/perpetual.
func isActiveSwap(m ccxt.MarketInterface) bool {
	// Check if active.
	if m.Active != nil && !*m.Active {
		return false
	}
	// Check if swap.
	if m.Swap == nil || !*m.Swap {
		return false
	}
	return true
}

// periodsPerDay returns the number of periods per day based on the interval string.
func periodsPerDay(interval *string) float64 {
	if interval == nil {
		return 3.0 // Default: 8-hour intervals.
	}
	switch strings.ToLower(*interval) {
	case "1h":
		return 24.0
	case "4h":
		return 6.0
	case "8h":
		return 3.0
	default:
		return 3.0
	}
}

// computeFundingStatsWindow computes stats over the past duration.
func computeFundingStatsWindow(history []ccxt.FundingRateHistory, duration time.Duration) fundingStatWindow {
	if len(history) == 0 {
		return fundingStatWindow{}
	}

	now := time.Now().UTC().UnixMilli()
	cutoffMs := now - duration.Milliseconds()

	var windowed []float64
	for _, r := range history {
		if r.Timestamp != nil && *r.Timestamp >= cutoffMs && r.FundingRate != nil {
			windowed = append(windowed, *r.FundingRate)
		}
	}

	if len(windowed) == 0 {
		return fundingStatWindow{}
	}

	// Compute mean.
	sum := 0.0
	for _, v := range windowed {
		sum += v
	}
	mean := sum / float64(len(windowed))

	// Compute stddev.
	varSum := 0.0
	for _, v := range windowed {
		diff := v - mean
		varSum += diff * diff
	}
	stddev := math.Sqrt(varSum / float64(len(windowed)))

	// Min/Max.
	minV := windowed[0]
	maxV := windowed[0]
	for _, v := range windowed {
		if v < minV {
			minV = v
		}
		if v > maxV {
			maxV = v
		}
	}

	return fundingStatWindow{
		mean:   mean,
		stddev: stddev,
		max:    maxV,
		min:    minV,
		count:  len(windowed),
	}
}

// formatScanResults builds a human-readable table. scannedProviders are the
// exchanges that were screened (for the header); providerErrs lists any that
// failed (surfaced as a caution so partial results are clearly labeled).
func formatScanResults(opportunities []opportunityRow, limitResults int, includeStability bool, scannedProviders, providerErrs []string, sortBy, sortOrder string) string {
	var sb strings.Builder

	sb.WriteString("=== Delta-Neutral Funding Carry Scan ===\n")
	sb.WriteString(fmt.Sprintf("Exchanges scanned: %s\n", strings.Join(scannedProviders, ", ")))
	sb.WriteString(fmt.Sprintf("Sorted by: %s %s\n\n", sortBy, sortOrder))

	if len(opportunities) == 0 {
		sb.WriteString("No opportunities found matching the criteria.\n")
		return sb.String()
	}

	// Limit output.
	display := opportunities
	if len(display) > limitResults {
		display = display[:limitResults]
	}

	// Header.
	if includeStability {
		sb.WriteString(fmt.Sprintf("%-5s %-8s %-8s %-15s %-8s %10s %10s %-12s %10s %10s %10s %10s %s\n",
			"Rank", "Exch", "Asset", "Futures", "Spot", "Funding%", "APR%", "Direction", "7d Mean%", "7d Std%", "14d Mean%", "14d Std%", "Label"))
		sb.WriteString(strings.Repeat("-", 160) + "\n")
	} else {
		sb.WriteString(fmt.Sprintf("%-5s %-8s %-8s %-15s %-8s %10s %10s %-12s %s\n",
			"Rank", "Exch", "Asset", "Futures", "Spot", "Funding%", "APR%", "Direction", "Label"))
		sb.WriteString(strings.Repeat("-", 100) + "\n")
	}

	// Rows.
	for i, row := range display {
		rank := i + 1
		var line string
		if includeStability {
			s7dMean := "-"
			s7dStd := "-"
			s14dMean := "-"
			s14dStd := "-"
			if row.stability7dMean != nil {
				s7dMean = fmt.Sprintf("%+.4f", *row.stability7dMean*100)
			}
			if row.stability7dStddev != nil {
				s7dStd = fmt.Sprintf("%.4f", *row.stability7dStddev*100)
			}
			if row.stability14dMean != nil {
				s14dMean = fmt.Sprintf("%+.4f", *row.stability14dMean*100)
			}
			if row.stability14dStddev != nil {
				s14dStd = fmt.Sprintf("%.4f", *row.stability14dStddev*100)
			}
			line = fmt.Sprintf("%-5d %-8s %-8s %-15s %-8s %10.6f %10.2f %-12s %10s %10s %10s %10s %s\n",
				rank, row.exchange, row.asset, row.symbol, spotCell(row.spotStatus), row.fundingPercent, row.apr, row.direction,
				s7dMean, s7dStd, s14dMean, s14dStd, row.label)
		} else {
			line = fmt.Sprintf("%-5d %-8s %-8s %-15s %-8s %10.6f %10.2f %-12s %s\n",
				rank, row.exchange, row.asset, row.symbol, spotCell(row.spotStatus), row.fundingPercent, row.apr, row.direction, row.label)
		}
		sb.WriteString(line)
	}

	// Caution: list any displayed assets that have a perp but no spot pair here.
	var noSpot []string
	var unknownSpot bool
	for _, row := range display {
		switch row.spotStatus {
		case "no-spot":
			noSpot = append(noSpot, row.asset)
		case "unknown":
			unknownSpot = true
		}
	}

	sb.WriteString("\n")
	if len(providerErrs) > 0 {
		sb.WriteString(fmt.Sprintf("⚠️  Some exchanges could not be scanned (results are partial): %s\n",
			strings.Join(providerErrs, "; ")))
	}
	if len(noSpot) > 0 {
		sb.WriteString(fmt.Sprintf("⚠️  No spot pair on its exchange for: %s — funding rank is still valid, but the delta-neutral spot leg cannot be opened there (source spot elsewhere, or treat as futures-only).\n",
			strings.Join(noSpot, ", ")))
	}
	if unknownSpot {
		sb.WriteString("⚠️  Spot availability could not be verified for some rows (spot markets unavailable) — shown as 'unknown'.\n")
	}
	sb.WriteString("Spot column: 'yes' = spot pair available | 'no-spot' = perp only on that exchange | 'unknown' = could not verify.\n")
	sb.WriteString("Note: Funding-only screen — drill into top picks with get_orderbook/futures_risk_summary before building a plan.\n")
	sb.WriteString("Legend: 'attractive' = positive carry + stable | 'watch' = near-zero/unstable/no-spot | 'blocked' = no perp or no funding\n")

	return sb.String()
}

// spotCell renders the spot-availability status for the table column.
func spotCell(status string) string {
	switch status {
	case "yes":
		return "yes"
	case "no-spot":
		return "NO-SPOT"
	default:
		return "unknown"
	}
}

// cmcListingResponse is the structure for CMC API response.
type cmcListingResponse struct {
	Data struct {
		CryptoCurrencyList []struct {
			Symbol  string `json:"symbol"`
			CMCRank int    `json:"cmcRank"`
		} `json:"cryptoCurrencyList"`
	} `json:"data"`
}

// fetchCMCListing fetches the top N crypto symbols from CMC.
func fetchCMCListing(ctx context.Context, baseURL string, topN int) ([]string, error) {
	client, err := utils.CreateHTTPClient("", 15*time.Second)
	if err != nil {
		return nil, fmt.Errorf("create http client: %w", err)
	}
	defer client.CloseIdleConnections()

	symbols := make([]string, 0, topN)
	limit := 100

	for start := 1; len(symbols) < topN; start += limit {
		url := fmt.Sprintf("%s?start=%d&limit=%d&sortBy=rank&sortType=desc&convert=USD&cryptoType=all&tagType=all",
			baseURL, start, limit)

		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			return nil, fmt.Errorf("new request: %w", err)
		}

		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "KhunQuant/1.0")

		resp, err := utils.DoRequestWithRetry(client, req)
		if err != nil {
			return nil, fmt.Errorf("request failed: %w", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
		}

		var data cmcListingResponse
		if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
			return nil, fmt.Errorf("decode response: %w", err)
		}

		if len(data.Data.CryptoCurrencyList) == 0 {
			break
		}

		for _, item := range data.Data.CryptoCurrencyList {
			if len(symbols) >= topN {
				break
			}
			symbols = append(symbols, item.Symbol)
		}

		if len(data.Data.CryptoCurrencyList) < limit {
			break
		}
	}

	return symbols, nil
}

// Helper functions for argument parsing.

func boolArgWithDefault(args map[string]any, key string, defaultVal bool) bool {
	if v, ok := args[key].(bool); ok {
		return v
	}
	return defaultVal
}
