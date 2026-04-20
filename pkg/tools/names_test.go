package tools_test

import (
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

func TestToolNameConstants_NonEmpty(t *testing.T) {
	names := []string{
		tools.NameReadFile, tools.NameWriteFile, tools.NameListDir,
		tools.NameEditFile, tools.NameAppendFile, tools.NameExec,
		tools.NameCron, tools.NameWebSearch, tools.NameWebFetch,
		tools.NameMessage, tools.NameSendFile, tools.NameFindSkills,
		tools.NameInstallSkill, tools.NameSpawn,
		tools.NameGetAssetsList, tools.NameGetTotalValue, tools.NameListPortfolios,
		tools.NameTakeSnapshot, tools.NameQuerySnapshots,
		tools.NameSnapshotSummary, tools.NameDeleteSnapshots,
		tools.NameI2C, tools.NameSPI,
		tools.NameToolSearchRegex, tools.NameToolSearchBM25,
		tools.NameGetTicker, tools.NameGetTickers, tools.NameGetOHLCV,
		tools.NameGetOrderBook, tools.NameGetMarkets,
		tools.NameCreateOrder, tools.NameCancelOrder, tools.NameGetOrder,
		tools.NameGetOpenOrders, tools.NameGetOrderHistory, tools.NameGetTradeHistory,
		tools.NameEmergencyStop, tools.NamePaperTrade, tools.NameGetOrderRateStatus,
		tools.NameCalculateIndicators, tools.NameMarketAnalysis, tools.NamePortfolioAllocation,
		tools.NameSetPriceAlert, tools.NameSetIndicatorAlert, tools.NameTransferFunds,
	}

	for _, name := range names {
		if name == "" {
			t.Errorf("tool name constant is empty")
		}
	}
}

func TestToolNameConstants_Unique(t *testing.T) {
	names := []string{
		tools.NameReadFile, tools.NameWriteFile, tools.NameListDir,
		tools.NameEditFile, tools.NameAppendFile, tools.NameExec,
		tools.NameCron, tools.NameWebSearch, tools.NameWebFetch,
		tools.NameMessage, tools.NameSendFile, tools.NameFindSkills,
		tools.NameInstallSkill, tools.NameSpawn,
		tools.NameGetAssetsList, tools.NameGetTotalValue, tools.NameListPortfolios,
		tools.NameTakeSnapshot, tools.NameQuerySnapshots,
		tools.NameSnapshotSummary, tools.NameDeleteSnapshots,
		tools.NameI2C, tools.NameSPI,
		tools.NameToolSearchRegex, tools.NameToolSearchBM25,
		tools.NameGetTicker, tools.NameGetTickers, tools.NameGetOHLCV,
		tools.NameGetOrderBook, tools.NameGetMarkets,
		tools.NameCreateOrder, tools.NameCancelOrder, tools.NameGetOrder,
		tools.NameGetOpenOrders, tools.NameGetOrderHistory, tools.NameGetTradeHistory,
		tools.NameEmergencyStop, tools.NamePaperTrade, tools.NameGetOrderRateStatus,
		tools.NameCalculateIndicators, tools.NameMarketAnalysis, tools.NamePortfolioAllocation,
		tools.NameSetPriceAlert, tools.NameSetIndicatorAlert, tools.NameTransferFunds,
	}

	seen := make(map[string]bool)
	for _, name := range names {
		if seen[name] {
			t.Errorf("duplicate tool name constant: %q", name)
		}
		seen[name] = true
	}
}

func TestCategoryConstants_NonEmpty(t *testing.T) {
	cats := []string{
		tools.CatFilesystem, tools.CatAutomation, tools.CatWeb,
		tools.CatCommunication, tools.CatSkills, tools.CatAgents,
		tools.CatPortfolios, tools.CatHardware, tools.CatDiscovery,
		tools.CatMarkets, tools.CatOrders, tools.CatAnalysis, tools.CatAlerts,
	}
	for _, cat := range cats {
		if cat == "" {
			t.Errorf("category constant is empty")
		}
	}
}
