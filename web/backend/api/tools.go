package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

type toolCatalogEntry struct {
	Name        string
	Description string
	Category    string
	ConfigKey   string
}

type toolSupportItem struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Category    string `json:"category"`
	ConfigKey   string `json:"config_key"`
	Status      string `json:"status"`
	ReasonCode  string `json:"reason_code,omitempty"`
}

type toolSupportResponse struct {
	Tools []toolSupportItem `json:"tools"`
}

type toolStateRequest struct {
	Enabled bool `json:"enabled"`
}

var toolCatalog = []toolCatalogEntry{
	{
		Name:        tools.NameReadFile,
		Description: tools.DescReadFile,
		Category:    tools.CatFilesystem,
		ConfigKey:   tools.NameReadFile,
	},
	{
		Name:        tools.NameWriteFile,
		Description: tools.DescWriteFile,
		Category:    tools.CatFilesystem,
		ConfigKey:   tools.NameWriteFile,
	},
	{
		Name:        tools.NameListDir,
		Description: tools.DescListDir,
		Category:    tools.CatFilesystem,
		ConfigKey:   tools.NameListDir,
	},
	{
		Name:        tools.NameEditFile,
		Description: tools.DescEditFile,
		Category:    tools.CatFilesystem,
		ConfigKey:   tools.NameEditFile,
	},
	{
		Name:        tools.NameAppendFile,
		Description: tools.DescAppendFile,
		Category:    tools.CatFilesystem,
		ConfigKey:   tools.NameAppendFile,
	},
	{
		Name:        tools.NameExec,
		Description: tools.DescExec,
		Category:    tools.CatFilesystem,
		ConfigKey:   tools.NameExec,
	},
	{
		Name:        tools.NameCron,
		Description: tools.DescCron,
		Category:    tools.CatAutomation,
		ConfigKey:   tools.NameCron,
	},
	{
		Name:        tools.NameWebSearch,
		Description: tools.DescWebSearch,
		Category:    tools.CatWeb,
		ConfigKey:   "web",
	},
	{
		Name:        tools.NameWebFetch,
		Description: tools.DescWebFetch,
		Category:    tools.CatWeb,
		ConfigKey:   tools.NameWebFetch,
	},
	{
		Name:        tools.NameMessage,
		Description: tools.DescMessage,
		Category:    tools.CatCommunication,
		ConfigKey:   tools.NameMessage,
	},
	{
		Name:        tools.NameSendFile,
		Description: tools.DescSendFile,
		Category:    tools.CatCommunication,
		ConfigKey:   tools.NameSendFile,
	},
	{
		Name:        tools.NameFindSkills,
		Description: tools.DescFindSkills,
		Category:    tools.CatSkills,
		ConfigKey:   tools.NameFindSkills,
	},
	{
		Name:        tools.NameInstallSkill,
		Description: tools.DescInstallSkill,
		Category:    tools.CatSkills,
		ConfigKey:   tools.NameInstallSkill,
	},
	{
		Name:        tools.NameSpawn,
		Description: tools.DescSpawn,
		Category:    tools.CatAgents,
		ConfigKey:   tools.NameSpawn,
	},
	{
		Name:        tools.NameGetAssetsList,
		Description: tools.DescGetAssetsList,
		Category:    tools.CatPortfolios,
		ConfigKey:   tools.NameGetAssetsList,
	},
	{
		Name:        tools.NameGetTotalValue,
		Description: tools.DescGetTotalValue,
		Category:    tools.CatPortfolios,
		ConfigKey:   tools.NameGetTotalValue,
	},
	{
		Name:        tools.NameListPortfolios,
		Description: tools.DescListPortfolios,
		Category:    tools.CatPortfolios,
		ConfigKey:   tools.NameListPortfolios,
	},
	{
		Name:        tools.NameTakeSnapshot,
		Description: tools.DescTakeSnapshot,
		Category:    tools.CatPortfolios,
		ConfigKey:   tools.NameTakeSnapshot,
	},
	{
		Name:        tools.NameQuerySnapshots,
		Description: tools.DescQuerySnapshots,
		Category:    tools.CatPortfolios,
		ConfigKey:   tools.NameQuerySnapshots,
	},
	{
		Name:        tools.NameSnapshotSummary,
		Description: tools.DescSnapshotSummary,
		Category:    tools.CatPortfolios,
		ConfigKey:   tools.NameSnapshotSummary,
	},
	{
		Name:        tools.NameDeleteSnapshots,
		Description: tools.DescDeleteSnapshots,
		Category:    tools.CatPortfolios,
		ConfigKey:   tools.NameDeleteSnapshots,
	},
	{
		Name:        tools.NameI2C,
		Description: tools.DescI2C,
		Category:    tools.CatHardware,
		ConfigKey:   tools.NameI2C,
	},
	{
		Name:        tools.NameSPI,
		Description: tools.DescSPI,
		Category:    tools.CatHardware,
		ConfigKey:   tools.NameSPI,
	},
	{
		Name:        tools.NameToolSearchRegex,
		Description: tools.DescToolSearchRegex,
		Category:    tools.CatDiscovery,
		ConfigKey:   "mcp.discovery.use_regex",
	},
	{
		Name:        tools.NameToolSearchBM25,
		Description: tools.DescToolSearchBM25,
		Category:    tools.CatDiscovery,
		ConfigKey:   "mcp.discovery.use_bm25",
	},

	// Market intelligence tools (Track A)
	{
		Name:        tools.NameGetTicker,
		Description: "Fetch the latest ticker for a single trading pair (price, bid, ask, 24h stats).",
		Category:    tools.CatMarkets,
		ConfigKey:   tools.NameGetTicker,
	},
	{
		Name:        tools.NameGetTickers,
		Description: "Fetch tickers for multiple symbols at once (max 20).",
		Category:    tools.CatMarkets,
		ConfigKey:   tools.NameGetTickers,
	},
	{
		Name:        tools.NameGetOHLCV,
		Description: "Fetch OHLCV candlestick data for chart analysis.",
		Category:    tools.CatMarkets,
		ConfigKey:   tools.NameGetOHLCV,
	},
	{
		Name:        tools.NameGetOrderBook,
		Description: "Fetch the current order book bid/ask depth.",
		Category:    tools.CatMarkets,
		ConfigKey:   tools.NameGetOrderBook,
	},
	{
		Name:        tools.NameGetMarkets,
		Description: "List all tradeable markets on a provider with lot size, tick size and fees.",
		Category:    tools.CatMarkets,
		ConfigKey:   tools.NameGetMarkets,
	},

	// Technical analysis tools (Track C)
	{
		Name:        tools.NameCalculateIndicators,
		Description: "Compute technical indicators (SMA, EMA, RSI, MACD, BB, ATR, Stochastic, VWAP) from live OHLCV data.",
		Category:    tools.CatAnalysis,
		ConfigKey:   tools.NameCalculateIndicators,
	},
	{
		Name:        tools.NameMarketAnalysis,
		Description: "Produce a structured market analysis combining price, 24h stats, and key indicators.",
		Category:    tools.CatAnalysis,
		ConfigKey:   tools.NameMarketAnalysis,
	},
	{
		Name:        tools.NamePortfolioAllocation,
		Description: "Compute portfolio allocation weights across all configured accounts with concentration warnings.",
		Category:    tools.CatAnalysis,
		ConfigKey:   tools.NamePortfolioAllocation,
	},

	// Order execution tools (Track B)
	{
		Name:        tools.NamePaperTrade,
		Description: "Simulate an order using live market price without placing a real order.",
		Category:    tools.CatOrders,
		ConfigKey:   tools.NamePaperTrade,
	},
	{
		Name:        tools.NameGetOrderRateStatus,
		Description: "Show current rate-limit token counts per provider.",
		Category:    tools.CatOrders,
		ConfigKey:   tools.NameGetOrderRateStatus,
	},
	{
		Name:        tools.NameGetOrder,
		Description: "Retrieve a single order by ID.",
		Category:    tools.CatOrders,
		ConfigKey:   tools.NameGetOrder,
	},
	{
		Name:        tools.NameGetOpenOrders,
		Description: "List all currently open orders, optionally filtered by symbol.",
		Category:    tools.CatOrders,
		ConfigKey:   tools.NameGetOpenOrders,
	},
	{
		Name:        tools.NameGetOrderHistory,
		Description: "Retrieve closed/filled order history.",
		Category:    tools.CatOrders,
		ConfigKey:   tools.NameGetOrderHistory,
	},
	{
		Name:        tools.NameGetTradeHistory,
		Description: "Retrieve personal trade execution history.",
		Category:    tools.CatOrders,
		ConfigKey:   tools.NameGetTradeHistory,
	},
	{
		Name:        tools.NameCreateOrder,
		Description: "Place a new order with 7 safety gates. Requires explicit confirmation.",
		Category:    tools.CatOrders,
		ConfigKey:   tools.NameCreateOrder,
	},
	{
		Name:        tools.NameCancelOrder,
		Description: "Cancel an open order by ID.",
		Category:    tools.CatOrders,
		ConfigKey:   tools.NameCancelOrder,
	},
	{
		Name:        tools.NameEmergencyStop,
		Description: "Cancel ALL open orders across all providers and pause automation. Irreversible.",
		Category:    tools.CatOrders,
		ConfigKey:   tools.NameEmergencyStop,
	},

	// Alert and transfer tools (Track D)
	{
		Name:        tools.NameSetPriceAlert,
		Description: "Create, list, or cancel price alerts that fire when a symbol crosses a threshold.",
		Category:    tools.CatAlerts,
		ConfigKey:   tools.NameSetPriceAlert,
	},
	{
		Name:        tools.NameSetIndicatorAlert,
		Description: "Create, list, or cancel indicator-based alerts (RSI, MACD, EMA, SMA).",
		Category:    tools.CatAlerts,
		ConfigKey:   tools.NameSetIndicatorAlert,
	},
	{
		Name:        tools.NameTransferFunds,
		Description: "Transfer funds between internal sub-accounts (e.g. spot → futures). Requires confirmation.",
		Category:    tools.CatAlerts,
		ConfigKey:   tools.NameTransferFunds,
	},
}

func (h *Handler) registerToolRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/tools", h.handleListTools)
	mux.HandleFunc("PUT /api/tools/{name}/state", h.handleUpdateToolState)
}

func (h *Handler) handleListTools(w http.ResponseWriter, r *http.Request) {
	cfg, err := config.LoadConfig(h.configPath)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to load config: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(toolSupportResponse{
		Tools: buildToolSupport(cfg),
	})
}

func (h *Handler) handleUpdateToolState(w http.ResponseWriter, r *http.Request) {
	cfg, err := config.LoadConfig(h.configPath)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to load config: %v", err), http.StatusInternalServerError)
		return
	}

	var req toolStateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("Invalid JSON: %v", err), http.StatusBadRequest)
		return
	}

	if err := applyToolState(cfg, r.PathValue("name"), req.Enabled); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := config.SaveConfig(h.configPath, cfg); err != nil {
		http.Error(w, fmt.Sprintf("Failed to save config: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func buildToolSupport(cfg *config.Config) []toolSupportItem {
	items := make([]toolSupportItem, 0, len(toolCatalog))
	for _, entry := range toolCatalog {
		status := "disabled"
		reasonCode := ""

		switch entry.Name {
		case tools.NameFindSkills, tools.NameInstallSkill:
			if cfg.Tools.IsToolEnabled(entry.ConfigKey) {
				if cfg.Tools.IsToolEnabled("skills") {
					status = "enabled"
				} else {
					status = "blocked"
					reasonCode = "requires_skills"
				}
			}
		case tools.NameSpawn:
			if cfg.Tools.IsToolEnabled(entry.ConfigKey) {
				if cfg.Tools.IsToolEnabled("subagent") {
					status = "enabled"
				} else {
					status = "blocked"
					reasonCode = "requires_subagent"
				}
			}
		case tools.NameToolSearchRegex:
			status, reasonCode = resolveDiscoveryToolSupport(cfg, cfg.Tools.MCP.Discovery.UseRegex)
		case tools.NameToolSearchBM25:
			status, reasonCode = resolveDiscoveryToolSupport(cfg, cfg.Tools.MCP.Discovery.UseBM25)
		case tools.NameI2C, tools.NameSPI:
			status, reasonCode = resolveHardwareToolSupport(cfg.Tools.IsToolEnabled(entry.ConfigKey))
		default:
			if cfg.Tools.IsToolEnabled(entry.ConfigKey) {
				status = "enabled"
			}
		}

		items = append(items, toolSupportItem{
			Name:        entry.Name,
			Description: entry.Description,
			Category:    entry.Category,
			ConfigKey:   entry.ConfigKey,
			Status:      status,
			ReasonCode:  reasonCode,
		})
	}
	return items
}

func resolveHardwareToolSupport(enabled bool) (string, string) {
	if !enabled {
		return "disabled", ""
	}
	if runtime.GOOS != "linux" {
		return "blocked", "requires_linux"
	}
	return "enabled", ""
}

func resolveDiscoveryToolSupport(cfg *config.Config, methodEnabled bool) (string, string) {
	if !cfg.Tools.IsToolEnabled("mcp") {
		return "disabled", ""
	}
	if !cfg.Tools.MCP.Discovery.Enabled {
		return "blocked", "requires_mcp_discovery"
	}
	if !methodEnabled {
		return "disabled", ""
	}
	return "enabled", ""
}

func applyToolState(cfg *config.Config, toolName string, enabled bool) error {
	switch toolName {
	case tools.NameReadFile:
		cfg.Tools.ReadFile.Enabled = enabled
	case tools.NameWriteFile:
		cfg.Tools.WriteFile.Enabled = enabled
	case tools.NameListDir:
		cfg.Tools.ListDir.Enabled = enabled
	case tools.NameEditFile:
		cfg.Tools.EditFile.Enabled = enabled
	case tools.NameAppendFile:
		cfg.Tools.AppendFile.Enabled = enabled
	case tools.NameExec:
		cfg.Tools.Exec.Enabled = enabled
	case tools.NameCron:
		cfg.Tools.Cron.Enabled = enabled
	case tools.NameWebSearch:
		cfg.Tools.Web.Enabled = enabled
	case tools.NameWebFetch:
		cfg.Tools.WebFetch.Enabled = enabled
	case tools.NameMessage:
		cfg.Tools.Message.Enabled = enabled
	case tools.NameSendFile:
		cfg.Tools.SendFile.Enabled = enabled
	case tools.NameFindSkills:
		cfg.Tools.FindSkills.Enabled = enabled
		if enabled {
			cfg.Tools.Skills.Enabled = true
		}
	case tools.NameInstallSkill:
		cfg.Tools.InstallSkill.Enabled = enabled
		if enabled {
			cfg.Tools.Skills.Enabled = true
		}
	case tools.NameSpawn:
		cfg.Tools.Spawn.Enabled = enabled
		if enabled {
			cfg.Tools.Subagent.Enabled = true
		}
	case tools.NameGetAssetsList:
		cfg.Tools.GetAssetsList.Enabled = enabled
	case tools.NameGetTotalValue:
		cfg.Tools.GetTotalValue.Enabled = enabled
	case tools.NameListPortfolios:
		cfg.Tools.ListPortfolios.Enabled = enabled
	case tools.NameTakeSnapshot:
		cfg.Tools.TakeSnapshot.Enabled = enabled
	case tools.NameQuerySnapshots:
		cfg.Tools.QuerySnapshots.Enabled = enabled
	case tools.NameSnapshotSummary:
		cfg.Tools.SnapshotSummary.Enabled = enabled
	case tools.NameDeleteSnapshots:
		cfg.Tools.DeleteSnapshots.Enabled = enabled
	case tools.NameI2C:
		cfg.Tools.I2C.Enabled = enabled
	case tools.NameSPI:
		cfg.Tools.SPI.Enabled = enabled
	case tools.NameToolSearchRegex:
		cfg.Tools.MCP.Discovery.UseRegex = enabled
		if enabled {
			cfg.Tools.MCP.Enabled = true
			cfg.Tools.MCP.Discovery.Enabled = true
		}
	case tools.NameToolSearchBM25:
		cfg.Tools.MCP.Discovery.UseBM25 = enabled
		if enabled {
			cfg.Tools.MCP.Enabled = true
			cfg.Tools.MCP.Discovery.Enabled = true
		}
	case tools.NamePaperTrade:
		cfg.Tools.PaperTrade.Enabled = enabled
	case tools.NameGetOrderRateStatus:
		cfg.Tools.GetOrderRateStatus.Enabled = enabled
	case tools.NameGetOrder:
		cfg.Tools.GetOrder.Enabled = enabled
	case tools.NameGetOpenOrders:
		cfg.Tools.GetOpenOrders.Enabled = enabled
	case tools.NameGetOrderHistory:
		cfg.Tools.GetOrderHistory.Enabled = enabled
	case tools.NameGetTradeHistory:
		cfg.Tools.GetTradeHistory.Enabled = enabled
	case tools.NameCreateOrder:
		cfg.Tools.CreateOrder.Enabled = enabled
	case tools.NameCancelOrder:
		cfg.Tools.CancelOrder.Enabled = enabled
	case tools.NameEmergencyStop:
		cfg.Tools.EmergencyStop.Enabled = enabled
	default:
		return fmt.Errorf("tool %q cannot be updated", toolName)
	}
	return nil
}
