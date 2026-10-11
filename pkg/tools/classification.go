package tools

// Class says what a tool can DO, independent of how it is named or configured.
//
// It exists for one purpose today: the ceiling on autonomous turns (a heartbeat,
// a scheduled agent job, and anything spawned from either). Those turns have no
// person present, and their prompt can be written by content the assistant read
// earlier, so they may only call tools whose worst case is reading, telling the
// user something, or touching the workspace. See turnMayCall in policy.go.
//
// Classify by capability, and when in doubt choose the MORE restrictive class: a
// tool that is wrongly denied is a missing feature in a background turn, a tool
// that is wrongly allowed is a trade.
type Class int

const (
	// ClassUnknown is the zero value and what ClassOf returns for any name not in
	// the table below, MCP tools included. Denied in autonomous turns.
	ClassUnknown Class = iota
	// ClassRead only reads: market data, balances, history, indicators, web
	// search and fetch, workspace reads, and tools that merely list or discover.
	ClassRead
	// ClassNotify tells the user something: message, send_file, a rendered chart.
	ClassNotify
	// ClassLocalState writes to the workspace: files, snapshots, memory.
	ClassLocalState
	// ClassAutomation creates or changes things that will run later: cron jobs,
	// alerts, DCA and delta-neutral plans.
	ClassAutomation
	// ClassPropose is reserved for tools that queue a proposal for a person to
	// approve. None exist yet.
	ClassPropose
	// ClassTrade places, cancels or modifies orders or positions. Exits are
	// trade tools too: an autonomous turn does not get to unwind by itself.
	ClassTrade
	// ClassFunds moves money between accounts or into earn products.
	ClassFunds
	// ClassSystem runs commands, installs code, reads or changes secrets and
	// configuration, starts sub-agents or drives hardware.
	ClassSystem
)

// String names the class for logs and test failures.
func (c Class) String() string {
	switch c {
	case ClassRead:
		return "read"
	case ClassNotify:
		return "notify"
	case ClassLocalState:
		return "local_state"
	case ClassAutomation:
		return "automation"
	case ClassPropose:
		return "propose"
	case ClassTrade:
		return "trade"
	case ClassFunds:
		return "funds"
	case ClassSystem:
		return "system"
	default:
		return "unknown"
	}
}

// Names of the two memory tools the seahorse context engine registers; they are
// not part of the tool catalog, so they have no constant in names.go.
const (
	nameShortGrep   = "short_grep"
	nameShortExpand = "short_expand"
)

// toolClasses is the classification table. It is explicit on purpose: a
// pattern such as "get_* is read" is how a mutating tool with a read-sounding
// name ends up trusted. TestClassOf_AllNameConstantsClassified and
// TestClassOf_AllRegisteredToolsClassified fail when a tool is added without a
// row here.
var toolClasses = map[string]Class{
	// Workspace and web reads.
	NameReadFile:   ClassRead,
	NameListDir:    ClassRead,
	NameWebSearch:  ClassRead,
	NameWebFetch:   ClassRead,
	NameFindSkills: ClassRead,
	NameLoadSkill:  ClassRead,
	// Discovery tools only promote hidden tools into the definition list; the
	// promoted tools are classified (or denied as unknown) on their own.
	NameToolSearchRegex: ClassRead,
	NameToolSearchBM25:  ClassRead,
	nameShortGrep:       ClassRead,
	nameShortExpand:     ClassRead,

	// Portfolio, PnL and snapshot reads.
	NameGetAssetsList:       ClassRead,
	NameGetTotalValue:       ClassRead,
	NameListPortfolios:      ClassRead,
	NameQuerySnapshots:      ClassRead,
	NameSnapshotSummary:     ClassRead,
	NameGetPnLSummary:       ClassRead,
	NameGetPnLDetail:        ClassRead,
	NamePortfolioAllocation: ClassRead,

	// Market data and analysis.
	NameGetTicker:           ClassRead,
	NameGetTickers:          ClassRead,
	NameGetOHLCV:            ClassRead,
	NameGetOrderBook:        ClassRead,
	NameGetMarkets:          ClassRead,
	NameCalculateIndicators: ClassRead,
	NameMarketAnalysis:      ClassRead,
	NameFundingRateHistory:  ClassRead,
	NameOptionQuote:         ClassRead,

	// Order, position and plan reads.
	NameGetOrder:                      ClassRead,
	NameGetOpenOrders:                 ClassRead,
	NameGetOrderHistory:               ClassRead,
	NameGetTradeHistory:               ClassRead,
	NameGetOrderRateStatus:            ClassRead,
	NameOptionGetOrder:                ClassRead,
	NameOptionOpenOrders:              ClassRead,
	NameFuturesGetOrder:               ClassRead,
	NameFuturesGetPositions:           ClassRead,
	NameFuturesGetFunding:             ClassRead,
	NameFuturesValidateMarket:         ClassRead,
	NameFuturesRiskSummary:            ClassRead,
	NameFuturesEstimateFundingFee:     ClassRead,
	NameListDCAPlans:                  ClassRead,
	NameGetDCAHistory:                 ClassRead,
	NameGetDCASummary:                 ClassRead,
	NameListDeltaNeutralPlans:         ClassRead,
	NameGetDeltaNeutralPlan:           ClassRead,
	NameGetDeltaNeutralSummary:        ClassRead,
	NameGetDeltaNeutralHistory:        ClassRead,
	NameGetDeltaNeutralSpread:         ClassRead,
	NameGetDeltaNeutralEarn:           ClassRead,
	NameScanDeltaNeutralOpportunities: ClassRead,
	NameEarnOverview:                  ClassRead,

	// Tell the user something. The chart is rendered and sent to the active chat.
	NameMessage:                      ClassNotify,
	NameSendFile:                     ClassNotify,
	NameRenderDeltaNeutralYieldChart: ClassNotify,

	// Workspace writes.
	NameWriteFile:    ClassLocalState,
	NameEditFile:     ClassLocalState,
	NameAppendFile:   ClassLocalState,
	NameTakeSnapshot: ClassLocalState,

	// Things that run later.
	NameCron:                    ClassAutomation,
	NameSetPriceAlert:           ClassAutomation,
	NameSetIndicatorAlert:       ClassAutomation,
	NameCreateDCAPlan:           ClassAutomation,
	NameUpdateDCAPlan:           ClassAutomation,
	NameDeleteDCAPlan:           ClassAutomation,
	NameCreateDeltaNeutralPlan:  ClassAutomation,
	NameUpdateDeltaNeutralPlan:  ClassAutomation,
	NameDeleteDeltaNeutralPlan:  ClassAutomation,
	NamePrepareDeltaNeutralPlan: ClassAutomation,

	// Orders and positions. Every exit is here as well: the previous rule that
	// exits are never gated holds for turns a person started, not for these.
	NameCreateOrder:                ClassTrade,
	NameCancelOrder:                ClassTrade,
	NameEmergencyStop:              ClassTrade,
	NamePaperTrade:                 ClassTrade,
	NameOptionCreateOrder:          ClassTrade,
	NameOptionCancelOrder:          ClassTrade,
	NameFuturesSetLeverage:         ClassTrade,
	NameFuturesOpenPosition:        ClassTrade,
	NameFuturesClosePosition:       ClassTrade,
	NameFuturesReducePosition:      ClassTrade,
	NameFuturesModifyProtection:    ClassTrade,
	NameFuturesCancelOrders:        ClassTrade,
	NameFuturesEmergencyFlatten:    ClassTrade,
	NameExecuteDCAOrder:            ClassTrade,
	NameOpenDeltaNeutralPosition:   ClassTrade,
	NameUnwindDeltaNeutralPosition: ClassTrade,
	NameResizeDeltaNeutralPosition: ClassTrade,

	// Money movement.
	NameTransferFunds:      ClassFunds,
	NameManageEarnPosition: ClassFunds,

	// Everything that can run code, touch secrets, start agents or drive devices.
	// delete_snapshots is here rather than under local state: it destroys history
	// the user may rely on, and nothing about a background turn needs to do that.
	NameExec:              ClassSystem,
	NameInstallSkill:      ClassSystem,
	NameConfigEncryptKeys: ClassSystem,
	NameSpawn:             ClassSystem,
	NameSubagent:          ClassSystem,
	NameDelegate:          ClassSystem,
	NameWebullReconnect:   ClassSystem,
	NameDeleteSnapshots:   ClassSystem,
	NameI2C:               ClassSystem,
	NameSPI:               ClassSystem,
	NameSerial:            ClassSystem,
}

// ClassOf returns the class of the named tool, or ClassUnknown for a name that
// is not in the table (an MCP tool, a tool added without being classified).
func ClassOf(name string) Class {
	return toolClasses[name]
}
