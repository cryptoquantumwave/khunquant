package agent

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/khunquant/khunquant/pkg/config"
	"github.com/khunquant/khunquant/pkg/media"
	"github.com/khunquant/khunquant/pkg/memory"
	"github.com/khunquant/khunquant/pkg/providers"
	"github.com/khunquant/khunquant/pkg/routing"
	"github.com/khunquant/khunquant/pkg/session"
	"github.com/khunquant/khunquant/pkg/snapshot"
	"github.com/khunquant/khunquant/pkg/tools"

	_ "github.com/khunquant/khunquant/pkg/exchanges/binance"
	_ "github.com/khunquant/khunquant/pkg/exchanges/binanceth"
	_ "github.com/khunquant/khunquant/pkg/exchanges/bitkub"
	_ "github.com/khunquant/khunquant/pkg/exchanges/okx"
	_ "github.com/khunquant/khunquant/pkg/exchanges/settrade"
)

// AgentInstance represents a fully configured agent with its own workspace,
// session manager, context builder, and tool registry.
type AgentInstance struct {
	ID                        string
	Name                      string
	Model                     string
	Fallbacks                 []string
	Workspace                 string
	MaxIterations             int
	MaxTokens                 int
	Temperature               float64
	ThinkingLevel             ThinkingLevel
	ContextWindow             int
	SummarizeMessageThreshold int
	SummarizeTokenPercent     int
	Provider                  providers.LLMProvider
	Sessions                  session.SessionStore
	ContextBuilder            *ContextBuilder
	Tools                     *tools.ToolRegistry
	Subagents                 *config.SubagentsConfig
	SkillsFilter              []string
	Candidates                []providers.FallbackCandidate

	// snapshotStore is the shared snapshot database, closed when the agent shuts down.
	snapshotStore *snapshot.Store

	// Router is non-nil when model routing is configured and the light model
	// was successfully resolved. It scores each incoming message and decides
	// whether to route to LightCandidates or stay with Candidates.
	Router *routing.Router
	// LightCandidates holds the resolved provider candidates for the light model.
	// Pre-computed at agent creation to avoid repeated model_list lookups at runtime.
	LightCandidates []providers.FallbackCandidate
}

// NewAgentInstance creates an agent instance from config.
func NewAgentInstance(
	agentCfg *config.AgentConfig,
	defaults *config.AgentDefaults,
	cfg *config.Config,
	provider providers.LLMProvider,
) *AgentInstance {
	workspace := resolveAgentWorkspace(agentCfg, defaults)
	os.MkdirAll(workspace, 0o755)

	model := resolveAgentModel(agentCfg, defaults)
	fallbacks := resolveAgentFallbacks(agentCfg, defaults)

	restrict := defaults.RestrictToWorkspace
	readRestrict := restrict && !defaults.AllowReadOutsideWorkspace

	// Compile path whitelist patterns from config.
	allowReadPaths := buildAllowReadPatterns(cfg)
	allowWritePaths := compilePatterns(cfg.Tools.AllowWritePaths)

	toolsRegistry := tools.NewToolRegistry()

	if cfg.Tools.IsToolEnabled("read_file") {
		maxReadFileSize := cfg.Tools.ReadFile.MaxReadFileSize
		toolsRegistry.Register(tools.NewReadFileTool(workspace, readRestrict, maxReadFileSize, allowReadPaths))
	}
	if cfg.Tools.IsToolEnabled("write_file") {
		toolsRegistry.Register(tools.NewWriteFileTool(workspace, restrict, allowWritePaths))
	}
	if cfg.Tools.IsToolEnabled("list_dir") {
		toolsRegistry.Register(tools.NewListDirTool(workspace, readRestrict, allowReadPaths))
	}
	if cfg.Tools.IsToolEnabled("exec") {
		execTool, err := tools.NewExecToolWithConfig(workspace, restrict, cfg, allowReadPaths)
		if err != nil {
			log.Fatalf("Critical error: unable to initialize exec tool: %v", err)
		}
		toolsRegistry.Register(execTool)
	}

	if cfg.Tools.IsToolEnabled("edit_file") {
		toolsRegistry.Register(tools.NewEditFileTool(workspace, restrict, allowWritePaths))
	}
	if cfg.Tools.IsToolEnabled("append_file") {
		toolsRegistry.Register(tools.NewAppendFileTool(workspace, restrict, allowWritePaths))
	}

	if cfg.Tools.IsToolEnabled("get_assets_list") {
		toolsRegistry.Register(tools.NewExchangeBalanceTool(cfg))
	}
	if cfg.Tools.IsToolEnabled("get_total_value") {
		toolsRegistry.Register(tools.NewExchangeTotalValueTool(cfg))
	}
	if cfg.Tools.IsToolEnabled("list_portfolios") {
		toolsRegistry.Register(tools.NewListPortfoliosTool(cfg))
	}

	// Snapshot tools — share a single Store instance.
	var snapshotStore *snapshot.Store
	if cfg.Tools.IsToolEnabled("take_snapshot") || cfg.Tools.IsToolEnabled("query_snapshots") ||
		cfg.Tools.IsToolEnabled("snapshot_summary") || cfg.Tools.IsToolEnabled("delete_snapshots") {
		var err error
		snapshotStore, err = snapshot.NewStore(workspace)
		if err != nil {
			log.Printf("snapshot: init store: %v; snapshot tools disabled", err)
		}
	}
	if snapshotStore != nil {
		if cfg.Tools.IsToolEnabled("take_snapshot") {
			toolsRegistry.Register(tools.NewTakeSnapshotTool(cfg, snapshotStore))
		}
		if cfg.Tools.IsToolEnabled("query_snapshots") {
			toolsRegistry.Register(tools.NewQuerySnapshotsTool(snapshotStore))
		}
		if cfg.Tools.IsToolEnabled("snapshot_summary") {
			toolsRegistry.Register(tools.NewSnapshotSummaryTool(snapshotStore))
		}
		if cfg.Tools.IsToolEnabled("delete_snapshots") {
			toolsRegistry.Register(tools.NewDeleteSnapshotsTool(snapshotStore))
		}
	}

	// Market intelligence tools (Track A).
	if cfg.Tools.IsToolEnabled("get_ticker") {
		toolsRegistry.Register(tools.NewGetTickerTool(cfg))
	}
	if cfg.Tools.IsToolEnabled("get_tickers") {
		toolsRegistry.Register(tools.NewGetTickersTool(cfg))
	}
	if cfg.Tools.IsToolEnabled("get_ohlcv") {
		toolsRegistry.Register(tools.NewGetOHLCVTool(cfg))
	}
	if cfg.Tools.IsToolEnabled("get_orderbook") {
		toolsRegistry.Register(tools.NewGetOrderBookTool(cfg))
	}
	if cfg.Tools.IsToolEnabled("get_markets") {
		toolsRegistry.Register(tools.NewGetMarketsTool(cfg))
	}

	// Order execution tools (Track B).
	if cfg.Tools.IsToolEnabled("create_order") {
		toolsRegistry.Register(tools.NewCreateOrderTool(cfg))
	}
	if cfg.Tools.IsToolEnabled("cancel_order") {
		toolsRegistry.Register(tools.NewCancelOrderTool(cfg))
	}
	if cfg.Tools.IsToolEnabled("get_order") {
		toolsRegistry.Register(tools.NewGetOrderTool(cfg))
	}
	if cfg.Tools.IsToolEnabled("get_open_orders") {
		toolsRegistry.Register(tools.NewGetOpenOrdersTool(cfg))
	}
	if cfg.Tools.IsToolEnabled("get_order_history") {
		toolsRegistry.Register(tools.NewGetOrderHistoryTool(cfg))
	}
	if cfg.Tools.IsToolEnabled("get_trade_history") {
		toolsRegistry.Register(tools.NewGetTradeHistoryTool(cfg))
	}
	if cfg.Tools.IsToolEnabled("emergency_stop") {
		toolsRegistry.Register(tools.NewEmergencyStopTool(cfg))
	}
	if cfg.Tools.IsToolEnabled("paper_trade") {
		toolsRegistry.Register(tools.NewPaperTradeTool(cfg))
	}
	if cfg.Tools.IsToolEnabled("get_order_rate_status") {
		toolsRegistry.Register(tools.NewGetOrderRateStatusTool())
	}

	// Technical analysis tools (Track C).
	if cfg.Tools.IsToolEnabled("calculate_indicators") {
		toolsRegistry.Register(tools.NewCalculateIndicatorsTool(cfg))
	}
	if cfg.Tools.IsToolEnabled("market_analysis") {
		toolsRegistry.Register(tools.NewMarketAnalysisTool(cfg))
	}
	if cfg.Tools.IsToolEnabled("portfolio_allocation") {
		toolsRegistry.Register(tools.NewPortfolioAllocationTool(cfg))
	}

	// Transfer tools (Track D — alert tools require cron service, registered in gateway).
	if cfg.Tools.IsToolEnabled("transfer_funds") {
		toolsRegistry.Register(tools.NewTransferFundsTool(cfg))
	}

	sessionsDir := filepath.Join(workspace, "sessions")
	sessions := initSessionStore(sessionsDir)

	mcpDiscoveryActive := cfg.Tools.MCP.Enabled && cfg.Tools.MCP.Discovery.Enabled
	contextBuilder := NewContextBuilder(workspace).WithToolDiscovery(
		mcpDiscoveryActive && cfg.Tools.MCP.Discovery.UseBM25,
		mcpDiscoveryActive && cfg.Tools.MCP.Discovery.UseRegex,
	)

	agentID := routing.DefaultAgentID
	agentName := ""
	var subagents *config.SubagentsConfig
	var skillsFilter []string

	if agentCfg != nil {
		agentID = routing.NormalizeAgentID(agentCfg.ID)
		agentName = agentCfg.Name
		subagents = agentCfg.Subagents
		skillsFilter = agentCfg.Skills
	}

	maxIter := defaults.MaxToolIterations
	if maxIter == 0 {
		maxIter = 20
	}

	maxTokens := defaults.MaxTokens
	if maxTokens == 0 {
		maxTokens = 8192
	}

	contextWindow := defaults.ContextWindow
	if contextWindow == 0 {
		// Default heuristic: 4x the output token limit.
		// Most models have context windows well above their output limits
		// (e.g., GPT-4o 128k ctx / 16k out, Claude 200k ctx / 8k out).
		// 4x is a conservative lower bound that avoids premature
		// summarization while remaining safe — the reactive
		// forceCompression handles any overshoot.
		contextWindow = maxTokens * 4
	}

	temperature := 0.7
	if defaults.Temperature != nil {
		temperature = *defaults.Temperature
	}

	var thinkingLevelStr string
	if mc, err := cfg.GetModelConfig(model); err == nil {
		thinkingLevelStr = mc.ThinkingLevel
	}
	thinkingLevel := parseThinkingLevel(thinkingLevelStr)

	summarizeMessageThreshold := defaults.SummarizeMessageThreshold
	if summarizeMessageThreshold == 0 {
		summarizeMessageThreshold = 20
	}

	summarizeTokenPercent := defaults.SummarizeTokenPercent
	if summarizeTokenPercent == 0 {
		summarizeTokenPercent = 75
	}

	// Resolve fallback candidates
	candidates := resolveModelCandidates(cfg, defaults.Provider, model, fallbacks)

	// Model routing setup: pre-resolve light model candidates at creation time
	// to avoid repeated model_list lookups on every incoming message.
	var router *routing.Router
	var lightCandidates []providers.FallbackCandidate
	if rc := defaults.Routing; rc != nil && rc.Enabled && rc.LightModel != "" {
		resolved := resolveModelCandidates(cfg, defaults.Provider, rc.LightModel, nil)
		if len(resolved) > 0 {
			router = routing.New(routing.RouterConfig{
				LightModel: rc.LightModel,
				Threshold:  rc.Threshold,
			})
			lightCandidates = resolved
		} else {
			log.Printf("routing: light_model %q not found in model_list — routing disabled for agent %q",
				rc.LightModel, agentID)
		}
	}

	return &AgentInstance{
		snapshotStore:             snapshotStore,
		ID:                        agentID,
		Name:                      agentName,
		Model:                     model,
		Fallbacks:                 fallbacks,
		Workspace:                 workspace,
		MaxIterations:             maxIter,
		MaxTokens:                 maxTokens,
		Temperature:               temperature,
		ThinkingLevel:             thinkingLevel,
		ContextWindow:             contextWindow,
		SummarizeMessageThreshold: summarizeMessageThreshold,
		SummarizeTokenPercent:     summarizeTokenPercent,
		Provider:                  provider,
		Sessions:                  sessions,
		ContextBuilder:            contextBuilder,
		Tools:                     toolsRegistry,
		Subagents:                 subagents,
		SkillsFilter:              skillsFilter,
		Candidates:                candidates,
		Router:                    router,
		LightCandidates:           lightCandidates,
	}
}

// resolveAgentWorkspace determines the workspace directory for an agent.
func resolveAgentWorkspace(agentCfg *config.AgentConfig, defaults *config.AgentDefaults) string {
	if agentCfg != nil && strings.TrimSpace(agentCfg.Workspace) != "" {
		return expandHome(strings.TrimSpace(agentCfg.Workspace))
	}
	// Use the configured default workspace (respects KHUNQUANT_HOME)
	if agentCfg == nil || agentCfg.Default || agentCfg.ID == "" || routing.NormalizeAgentID(agentCfg.ID) == "main" {
		return expandHome(defaults.Workspace)
	}
	// For named agents without explicit workspace, use default workspace with agent ID suffix
	id := routing.NormalizeAgentID(agentCfg.ID)
	return filepath.Join(expandHome(defaults.Workspace), "..", "workspace-"+id)
}

// resolveAgentModel resolves the primary model for an agent.
func resolveAgentModel(agentCfg *config.AgentConfig, defaults *config.AgentDefaults) string {
	if agentCfg != nil && agentCfg.Model != nil && strings.TrimSpace(agentCfg.Model.Primary) != "" {
		return strings.TrimSpace(agentCfg.Model.Primary)
	}
	return defaults.GetModelName()
}

// resolveAgentFallbacks resolves the fallback models for an agent.
func resolveAgentFallbacks(agentCfg *config.AgentConfig, defaults *config.AgentDefaults) []string {
	if agentCfg != nil && agentCfg.Model != nil && agentCfg.Model.Fallbacks != nil {
		return agentCfg.Model.Fallbacks
	}
	return defaults.ModelFallbacks
}

func compilePatterns(patterns []string) []*regexp.Regexp {
	compiled := make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			fmt.Printf("Warning: invalid path pattern %q: %v\n", p, err)
			continue
		}
		compiled = append(compiled, re)
	}
	return compiled
}

func buildAllowReadPatterns(cfg *config.Config) []*regexp.Regexp {
	var configured []string
	if cfg != nil {
		configured = cfg.Tools.AllowReadPaths
	}

	compiled := compilePatterns(configured)
	mediaDirPattern := regexp.MustCompile(mediaTempDirPattern())
	for _, pattern := range compiled {
		if pattern.String() == mediaDirPattern.String() {
			return compiled
		}
	}

	return append(compiled, mediaDirPattern)
}

func mediaTempDirPattern() string {
	sep := regexp.QuoteMeta(string(os.PathSeparator))
	return "^" + regexp.QuoteMeta(filepath.Clean(media.TempDir())) + "(?:" + sep + "|$)"
}

// Close releases resources held by the agent's session store and snapshot store.
func (a *AgentInstance) Close() error {
	if a.snapshotStore != nil {
		a.snapshotStore.Close()
	}
	if a.Sessions != nil {
		return a.Sessions.Close()
	}
	return nil
}

// initSessionStore creates the session persistence backend.
// It uses the JSONL store by default and auto-migrates legacy JSON sessions.
// Falls back to SessionManager if the JSONL store cannot be initialized or
// if migration fails (which indicates the store cannot write reliably).
func initSessionStore(dir string) session.SessionStore {
	store, err := memory.NewJSONLStore(dir)
	if err != nil {
		log.Printf("memory: init store: %v; using json sessions", err)
		return session.NewSessionManager(dir)
	}

	if n, merr := memory.MigrateFromJSON(context.Background(), dir, store); merr != nil {
		// Migration failure means the store could not write data.
		// Fall back to SessionManager to avoid a split state where
		// some sessions are in JSONL and others remain in JSON.
		log.Printf("memory: migration failed: %v; falling back to json sessions", merr)
		store.Close()
		return session.NewSessionManager(dir)
	} else if n > 0 {
		log.Printf("memory: migrated %d session(s) to jsonl", n)
	}

	return session.NewJSONLBackend(store)
}

func expandHome(path string) string {
	if path == "" {
		return path
	}
	if path[0] == '~' {
		home, _ := os.UserHomeDir()
		if len(path) > 1 && path[1] == '/' {
			return home + path[1:]
		}
		return home
	}
	return path
}
