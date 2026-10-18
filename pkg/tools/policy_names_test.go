package tools

import "testing"

// TestPolicyKeysAreRealTools reconciles the policy table against the tool name
// constants.
//
// A policy keyed on a name no tool answers to is not a policy - it is a gap
// that reads like coverage. Keys are Name* constants, so a typo is already a
// compile error; what this catches is a constant that is renamed or retired
// while the table keeps referring to it, and a financial tool quietly losing
// its gate as a result.
//
// The same reconciliation in web/backend/api is what found the option tools
// shipping with no way to disable them.
func TestPolicyKeysAreRealTools(t *testing.T) {
	known := knownToolNames()

	for name := range defaultToolPolicies() {
		if !known[name] {
			t.Errorf("policy key %q matches no tool name constant: the policy never applies", name)
		}
	}
}

// TestPolicyCoversKnownFinancialTools is the opposite direction: the tools that
// move money must carry a policy. Listed explicitly rather than derived, so
// adding a financial tool means deciding about it here rather than inheriting
// silence.
func TestPolicyCoversKnownFinancialTools(t *testing.T) {
	table := defaultToolPolicies()

	for _, name := range []string{
		NameCreateOrder,
		NameTransferFunds,
		NameOptionCreateOrder,
		NameFuturesSetLeverage,
		NameFuturesOpenPosition,
		NameFuturesModifyProtection,
		NameOpenDeltaNeutralPosition,
		NameResizeDeltaNeutralPosition,
		NameManageEarnPosition,
		NameInstallSkill,
		NameProposeOrder, // places nothing itself, but an approved proposal becomes a real order
	} {
		if _, ok := table[name]; !ok {
			t.Errorf("%q moves money or runs third-party code but has no policy entry", name)
		}
	}
}

// knownToolNames lists every static tool name. Kept beside the policy table so
// the two are reviewed together.
func knownToolNames() map[string]bool {
	names := []string{
		NameReadFile, NameWriteFile, NameListDir, NameEditFile, NameAppendFile,
		NameExec, NameCron, NameWebSearch, NameWebFetch, NameMessage, NameSendFile,
		NameFindSkills, NameInstallSkill, NameLoadSkill, NameSpawn, NameDelegate, NameSubagent,
		NameGetAssetsList, NameGetTotalValue, NameListPortfolios, NameWebullReconnect,
		NameTakeSnapshot, NameQuerySnapshots, NameSnapshotSummary, NameDeleteSnapshots,
		NameI2C, NameSPI, NameSerial, NameToolSearchRegex, NameToolSearchBM25,
		NameGetTicker, NameGetTickers, NameGetOHLCV, NameGetOrderBook, NameGetMarkets,
		NameCreateOrder, NameCancelOrder, NameGetOrder, NameGetOpenOrders,
		NameGetOrderHistory, NameGetTradeHistory, NameEmergencyStop, NamePaperTrade,
		NameGetOrderRateStatus,
		NameOptionQuote, NameOptionCreateOrder, NameOptionCancelOrder,
		NameOptionGetOrder, NameOptionOpenOrders,
		NameFuturesSetLeverage, NameFuturesOpenPosition, NameFuturesGetOrder,
		NameFuturesGetPositions, NameFuturesGetFunding, NameFuturesValidateMarket,
		NameFuturesRiskSummary, NameFuturesEstimateFundingFee, NameFuturesClosePosition,
		NameFuturesReducePosition, NameFuturesModifyProtection, NameFuturesCancelOrders,
		NameFuturesEmergencyFlatten, NameFundingRateHistory,
		NameCalculateIndicators, NameMarketAnalysis, NamePortfolioAllocation,
		NameSetPriceAlert, NameSetIndicatorAlert, NameTransferFunds,
		NameConfigEncryptKeys, NameGetPnLSummary, NameGetPnLDetail,
		NameCreateDCAPlan, NameListDCAPlans, NameUpdateDCAPlan, NameDeleteDCAPlan,
		NameExecuteDCAOrder, NameGetDCAHistory, NameGetDCASummary,
		NameCreateDeltaNeutralPlan, NameListDeltaNeutralPlans, NameGetDeltaNeutralPlan,
		NameUpdateDeltaNeutralPlan, NameDeleteDeltaNeutralPlan, NameGetDeltaNeutralSummary,
		NameGetDeltaNeutralHistory, NameGetDeltaNeutralSpread, NameGetDeltaNeutralEarn,
		NamePrepareDeltaNeutralPlan, NameOpenDeltaNeutralPosition,
		NameUnwindDeltaNeutralPosition, NameResizeDeltaNeutralPosition,
		NameScanDeltaNeutralOpportunities, NameRenderDeltaNeutralYieldChart,
		NameEarnOverview, NameManageEarnPosition,
		NameGetProfile, NameUpdateProfile, NameRespondProactive,
		NameCreateIntent, NameListIntents, NameUpdateIntent, NameProposeOrder, NameRemember,
	}

	known := make(map[string]bool, len(names))
	for _, n := range names {
		known[n] = true
	}
	return known
}
