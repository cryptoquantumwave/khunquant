package tools

// Tool name constants — single source of truth for all static tool names.
const (
	NameReadFile        = "read_file"
	NameWriteFile       = "write_file"
	NameListDir         = "list_dir"
	NameEditFile        = "edit_file"
	NameAppendFile      = "append_file"
	NameExec            = "exec"
	NameCron            = "cron"
	NameWebSearch       = "web_search"
	NameWebFetch        = "web_fetch"
	NameMessage         = "message"
	NameSendFile        = "send_file"
	NameFindSkills      = "find_skills"
	NameInstallSkill    = "install_skill"
	NameSpawn           = "spawn"
	NameGetAssetsList   = "get_assets_list"
	NameGetTotalValue   = "get_total_value"
	NameListPortfolios  = "list_portfolios"
	NameTakeSnapshot    = "take_snapshot"
	NameQuerySnapshots  = "query_snapshots"
	NameSnapshotSummary = "snapshot_summary"
	NameDeleteSnapshots = "delete_snapshots"
	NameI2C             = "i2c"
	NameSPI             = "spi"
	NameToolSearchRegex = "tool_search_tool_regex"
	NameToolSearchBM25  = "tool_search_tool_bm25"

	// Market intelligence (Track A)
	NameGetTicker    = "get_ticker"
	NameGetTickers   = "get_tickers"
	NameGetOHLCV     = "get_ohlcv"
	NameGetOrderBook = "get_orderbook"
	NameGetMarkets   = "get_markets"

	// Order execution (Track B)
	NameCreateOrder        = "create_order"
	NameCancelOrder        = "cancel_order"
	NameGetOrder           = "get_order"
	NameGetOpenOrders      = "get_open_orders"
	NameGetOrderHistory    = "get_order_history"
	NameGetTradeHistory    = "get_trade_history"
	NameEmergencyStop      = "emergency_stop"
	NamePaperTrade         = "paper_trade"
	NameGetOrderRateStatus = "get_order_rate_status"

	// Technical analysis (Track C)
	NameCalculateIndicators = "calculate_indicators"
	NameMarketAnalysis      = "market_analysis"
	NamePortfolioAllocation = "portfolio_allocation"

	// Alerts and transfers (Track D)
	NameSetPriceAlert     = "set_price_alert"
	NameSetIndicatorAlert = "set_indicator_alert"
	NameTransferFunds     = "transfer_funds"

	// Security
	NameConfigEncryptKeys = "config_encrypt_keys"

	// PnL — Profit and Loss (Track F)
	NameGetPnLSummary = "get_pnl_summary"
	NameGetPnLDetail  = "get_pnl_detail"

	// DCA — Dollar Cost Averaging (Track E)
	NameCreateDCAPlan   = "create_dca_plan"
	NameListDCAPlans    = "list_dca_plans"
	NameUpdateDCAPlan   = "update_dca_plan"
	NameDeleteDCAPlan   = "delete_dca_plan"
	NameExecuteDCAOrder = "execute_dca_order"
	NameGetDCAHistory   = "get_dca_history"
	NameGetDCASummary   = "get_dca_summary"
)

// Category constants for the web UI tool catalog.
const (
	CatFilesystem    = "filesystem"
	CatAutomation    = "automation"
	CatWeb           = "web"
	CatCommunication = "communication"
	CatSkills        = "skills"
	CatAgents        = "agents"
	CatPortfolios    = "portfolios"
	CatHardware      = "hardware"
	CatDiscovery     = "discovery"
	CatMarkets       = "markets"
	CatOrders        = "orders"
	CatAnalysis      = "analysis"
	CatAlerts        = "alerts"
	CatDCA           = "dca"
	CatPnL           = "pnl"
)

// Catalog description constants — short, UI-facing summaries for the web tool catalog.
// These are distinct from each tool's Description() method which is the detailed LLM-facing prompt.
const (
	DescReadFile        = "Read file content from the workspace or explicitly allowed paths."
	DescWriteFile       = "Create or overwrite files within the writable workspace scope."
	DescListDir         = "Inspect directories and enumerate files available to the agent."
	DescEditFile        = "Apply targeted edits to existing files without rewriting everything."
	DescAppendFile      = "Append content to the end of an existing file."
	DescExec            = "Run shell commands inside the configured workspace sandbox."
	DescCron            = "Schedule one-time or recurring reminders, jobs, and shell commands."
	DescWebSearch       = "Search the web using the configured providers."
	DescWebFetch        = "Fetch and summarize the contents of a webpage."
	DescMessage         = "Send a follow-up message back to the active user or chat."
	DescSendFile        = "Send an outbound file or media attachment to the active chat."
	DescFindSkills      = "Search external skill registries for installable skills."
	DescInstallSkill    = "Install a skill into the current workspace from a registry."
	DescSpawn           = "Launch a background subagent for long-running or delegated work."
	DescGetAssetsList   = "Retrieve asset balances from a configured cryptocurrency exchange."
	DescGetTotalValue   = "Estimate the total portfolio value in a quote currency by fetching all wallet balances and looking up live prices."
	DescListPortfolios  = "List all available portfolio accounts (exchange + account name pairs) that are enabled and have credentials configured."
	DescTakeSnapshot    = "Capture a snapshot of all portfolio balances and store it for historical tracking."
	DescQuerySnapshots  = "Query historical portfolio snapshots filtered by time range, label, source, or asset."
	DescSnapshotSummary = "Summarize portfolio performance across snapshots, showing gains and losses over time."
	DescDeleteSnapshots = "Delete historical portfolio snapshots by ID or filter criteria."
	DescI2C             = "Interact with I2C hardware devices exposed on the host."
	DescSPI             = "Interact with SPI hardware devices exposed on the host."
	DescToolSearchRegex = "Discover hidden MCP tools by regex search when tool discovery is enabled."
	DescToolSearchBM25  = "Discover hidden MCP tools by semantic ranking when tool discovery is enabled."
	DescGetPnLSummary  = "Compute cost basis and unrealized (and optionally realized) PnL for all currently held assets across one or all exchange accounts."
	DescGetPnLDetail   = "Per-symbol PnL audit: replay buy/sell trade history to show realized profit, average cost, current unrealized gain/loss, and fees paid."

	// DCA tool descriptions
	DescCreateDCAPlan   = "Create a new Dollar Cost Averaging plan to automatically invest a fixed amount on a schedule."
	DescListDCAPlans    = "List all configured DCA plans with their status, schedule, and recent execution history."
	DescUpdateDCAPlan   = "Update an existing DCA plan's amount, schedule, or other parameters."
	DescDeleteDCAPlan   = "Delete a DCA plan. In-flight executions are not cancelled."
	DescExecuteDCAOrder = "Manually trigger a DCA plan order execution outside its normal schedule."
	DescGetDCAHistory   = "Retrieve the execution history for a DCA plan: amounts invested, prices paid, and timestamps."
	DescGetDCASummary   = "Summarize a DCA plan's performance: total invested, average cost, current value, and unrealized PnL."
)
