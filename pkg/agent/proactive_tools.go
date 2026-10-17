package agent

import (
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

// AllProactiveTools in ProactiveTurnRequest.Tools asks for every tool the
// turn's ceiling allows, which is what a proactive turn was offered before the
// subset existed.
const AllProactiveTools = config.AllToolsName

// heartbeatTools is the tool subset of a governed heartbeat turn when the
// request names none. The v2 HEARTBEAT.md template tells the model which tools
// to use for a portfolio check, and a tool it names that the turn does not have
// costs a wasted round trip, so every tool that template names is here. The
// rest are the owner-facing reads a check on "how is my money doing" needs and
// nothing that browses, lists orders or explores markets.
//
// A name that is not registered on this install (a disabled exchange, no DCA
// store) simply is not offered; a name the ceiling denies would not be offered
// either. The subset can only narrow what the ceiling allows.
//
//	tool                       why a heartbeat has it
//	get_profile                the owner's language, thresholds and quiet hours (template: "Tailor every threshold")
//	list_portfolios            template step 1: which accounts exist
//	get_total_value            template step 1: value per portfolio and combined
//	get_assets_list            holdings, to weigh a move by position size
//	portfolio_allocation       template step 1 and the allocation-drift check
//	get_pnl_summary            template step 1: realised / unrealised PnL
//	get_pnl_detail             template: position-level breakdown to investigate a trigger
//	get_ticker, get_tickers    current price of a held asset, to confirm a move
//	futures_get_positions      template: leveraged exposure
//	futures_risk_summary       template: margin ratio, distance to liquidation
//	funding_rate_history       template: funding spike that changes carrying cost
//	get_dca_summary            DCA plans in force, so a dip is not reported as news to a plan that buys it
//	get_delta_neutral_summary  hedged positions, whose leg moves cancel
//	snapshot_summary           template: yesterday's and last week's totals
//	query_snapshots            template: the snapshot baseline
//	take_snapshot              template: today's snapshot so tomorrow has a baseline (heartbeat kind only)
//	calculate_indicators       template: technical context for an asset that tripped a trigger
//	market_analysis            template: the same, as one call
//	web_search                 template: news for held assets
//	web_fetch                  template: reading a result (heartbeat kind only)
//	load_skill                 a skill the owner's HEARTBEAT.md names
//
// respond_proactive is not in the table: it is always present.
var heartbeatTools = []string{
	tools.NameGetProfile,
	tools.NameListPortfolios,
	tools.NameGetTotalValue,
	tools.NameGetAssetsList,
	tools.NamePortfolioAllocation,
	tools.NameGetPnLSummary,
	tools.NameGetPnLDetail,
	tools.NameGetTicker,
	tools.NameGetTickers,
	tools.NameFuturesGetPositions,
	tools.NameFuturesRiskSummary,
	tools.NameFundingRateHistory,
	tools.NameGetDCASummary,
	tools.NameGetDeltaNeutralSummary,
	tools.NameSnapshotSummary,
	tools.NameQuerySnapshots,
	tools.NameTakeSnapshot,
	tools.NameCalculateIndicators,
	tools.NameMarketAnalysis,
	tools.NameWebSearch,
	tools.NameWebFetch,
	tools.NameLoadSkill,
}

// candidateTools is the tool subset of a candidate-driven turn when the request
// names none. Such a turn composes a message about candidates the system already
// found and shows the owner the evidence itself, so it needs the owner's profile,
// the price of the subject, the exposure behind it and the plans around it, and
// nothing that scans the portfolio.
//
//	tool                       why a candidate turn has it
//	get_profile                the owner's language and tone
//	get_ticker, get_tickers    the current price of the candidate's subject
//	futures_get_positions      the exposure a futures candidate is about
//	futures_risk_summary       margin and liquidation distance for the same
//	get_dca_summary            whether a DCA plan already acts on the subject
//	get_delta_neutral_summary  whether a hedge already covers it
//	load_skill                 a skill the caller's prompt names
//	propose_order              suggest a risk-reducing order for the owner to approve; offered only
//	                           when approvals are on and the turn's ceiling grants it (proposeOrderOpen)
var candidateTools = []string{
	tools.NameGetProfile,
	tools.NameGetTicker,
	tools.NameGetTickers,
	tools.NameFuturesGetPositions,
	tools.NameFuturesRiskSummary,
	tools.NameGetDCASummary,
	tools.NameGetDeltaNeutralSummary,
	tools.NameLoadSkill,
	tools.NameProposeOrder,
}

// defaultProactiveTools is the built-in subset for a kind of turn.
func defaultProactiveTools(kind proactive.TurnKind) []string {
	if kind == proactive.TurnHeartbeatLegacy {
		return heartbeatTools
	}
	return candidateTools
}

// proactiveToolFilter turns a request's tool list into the predicate the turn's
// registry clone is filtered with. It returns nil, meaning no filter, for a list
// that contains "*"; the built-in default for the kind for an empty list (blank
// names do not count); otherwise exactly the names listed. A name that is not a
// registered tool matches nothing. Whatever the filter keeps, the turn's ceiling
// still decides at definition time and at execution which tools are shown and
// callable, so the filter can only narrow.
func proactiveToolFilter(kind proactive.TurnKind, requested []string) func(name string) bool {
	var names []string
	for _, n := range requested {
		n = strings.TrimSpace(n)
		if n == AllProactiveTools {
			return nil
		}
		if n != "" {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		names = defaultProactiveTools(kind)
	}
	set := make(map[string]struct{}, len(names))
	for _, n := range names {
		set[n] = struct{}{}
	}
	return func(name string) bool {
		_, ok := set[name]
		return ok
	}
}
