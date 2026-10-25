package gateway

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

// The automatic snapshots go to the store the default agent's tools read, which is
// the default AGENT's workspace (as for the proactive database), not necessarily
// cfg.WorkspacePath().
func TestFinance_SnapshotStoreIsTheDefaultAgentsWorkspace(t *testing.T) {
	root := t.TempDir()
	defaultsWS := filepath.Join(root, "defaults-ws")
	agentWS := filepath.Join(root, "agent-ws")

	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = defaultsWS
	cfg.Agents.List = []config.AgentConfig{{ID: "trader", Default: true, Workspace: agentWS}}
	cfg.Exchanges.Binance = config.BinanceExchangeConfig{Enabled: true, Accounts: []config.ExchangeAccount{{Name: "main"}}}

	_, closers := financeSources(assemblyDeps{Cfg: cfg})
	for _, c := range closers {
		_ = c.Close()
	}
	if _, err := os.Stat(filepath.Join(agentWS, "memory", "snapshots", "snapshots.db")); err != nil {
		t.Fatalf("no snapshot store in the default agent's workspace: %v", err)
	}
	if _, err := os.Stat(filepath.Join(defaultsWS, "memory", "snapshots", "snapshots.db")); err == nil {
		t.Fatal("a snapshot store was created in the defaults workspace, where the default agent's tools do not look")
	}
}
