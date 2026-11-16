package tools

import "context"

// publicReadTools are the read-class tools a person who is not the owner may still
// call while an owner is configured: public market data, and workspace and web
// reads that touch no account. Every OTHER read-class tool reads the owner's
// accounts, plans or profile (the exchange accounts configured are the owner's:
// there are no per-person accounts) and is refused to a non-owner's turn.
//
// This is the one explicit placement of the read class (TestEveryReadToolIsPlacedForOwner
// makes it exhaustive): a read tool added later is in neither set, counts as owner-data
// (fail closed), and the test fails until someone places it in publicReadTools or
// ownerDataReadTools.
var publicReadTools = map[string]bool{
	// Workspace, web and discovery reads.
	NameReadFile: true, NameListDir: true, NameWebSearch: true, NameWebFetch: true,
	NameFindSkills: true, NameLoadSkill: true, NameToolSearchRegex: true, NameToolSearchBM25: true,
	nameShortGrep: true, nameShortExpand: true,

	// Public market data and analysis.
	NameGetTicker: true, NameGetTickers: true, NameGetOHLCV: true, NameGetOrderBook: true,
	NameGetMarkets: true, NameCalculateIndicators: true, NameMarketAnalysis: true,
	NameFundingRateHistory: true, NameOptionQuote: true, NameFuturesValidateMarket: true,
	NameFuturesGetFunding: true, NameGetDeltaNeutralSpread: true, NameScanDeltaNeutralOpportunities: true,
}

// ownerDataReadTools are the read-class tools that read the owner's accounts, plans,
// orders, history or profile. A non-owner's turn is refused them.
var ownerDataReadTools = map[string]bool{
	// Portfolio, PnL and snapshots.
	NameGetAssetsList: true, NameGetTotalValue: true, NameListPortfolios: true, NameQuerySnapshots: true,
	NameSnapshotSummary: true, NameGetPnLSummary: true, NameGetPnLDetail: true, NamePortfolioAllocation: true,

	// Orders, trades and positions.
	NameGetOrder: true, NameGetOpenOrders: true, NameGetOrderHistory: true, NameGetTradeHistory: true,
	NameGetOrderRateStatus: true, NameOptionGetOrder: true, NameOptionOpenOrders: true,
	NameFuturesGetOrder: true, NameFuturesGetPositions: true, NameFuturesRiskSummary: true,
	NameFuturesEstimateFundingFee: true,

	// DCA and delta-neutral plans, earn, profile and intents.
	NameListDCAPlans: true, NameGetDCAHistory: true, NameGetDCASummary: true, NameListDeltaNeutralPlans: true,
	NameGetDeltaNeutralPlan: true, NameGetDeltaNeutralSummary: true, NameGetDeltaNeutralHistory: true,
	NameGetDeltaNeutralEarn: true, NameEarnOverview: true, NameGetProfile: true, NameListIntents: true,
}

// nonOwnerPersonTurn reports whether the turn carried by ctx is a person who is not
// the owner, while an owner is configured. It asks checkOwnerTurn (the one owner
// rule) and refuses exactly its ownerOther verdict, so the owner, the web launcher's
// authenticated chat and their sub-turns (which keep the sender) pass, and so do
// the turns that are not persons (untagged, heartbeat, cron, proactive, the DCA job,
// approval executors: ownerBackground). A non-owner's sub-turn keeps that person's
// sender and is refused with them. With no owner configured, or no config armed, it
// is false: nothing changes for an install that never set an owner.
func nonOwnerPersonTurn(ctx context.Context) bool {
	cfg := armedConfig.Load()
	if cfg == nil {
		return false
	}
	if _, ok := cfg.Proactive.OwnerRoute(); !ok {
		return false
	}
	_, v := checkOwnerTurn(ctx, cfg, false)
	return v == ownerOther
}

// ownerDataToolRefused reports whether the named tool is one a non-owner person's
// turn may not call: every trade-class and funds-class tool, and every read-class
// tool that is not a placed public one (an unplaced read tool counts as owner-data).
func ownerDataToolRefused(name string) bool {
	switch ClassOf(name) {
	case ClassTrade, ClassFunds:
		return true
	case ClassRead:
		return !publicReadTools[name]
	}
	return false
}

// nonOwnerMayNotCall is the predicate turnMayCall and enforceToolPolicy share: the
// turn is a non-owner person's and the tool is owner-data, a trade or a funds tool.
func nonOwnerMayNotCall(ctx context.Context, name string) bool {
	return ownerDataToolRefused(name) && nonOwnerPersonTurn(ctx)
}

// nonOwnerRefusal is the text a refused non-owner turn gets. It names no figures.
func nonOwnerRefusal(name string) *ToolResult {
	return policyRefusal(name, "this turn may not use "+name+": an owner is configured, and trading, moving funds "+
		"and reading the owner's accounts, plans and profile are done only in the owner's own conversation. "+
		"Nothing was executed and nothing was read. Public market data is still available. Tell the person to ask the owner.")
}
