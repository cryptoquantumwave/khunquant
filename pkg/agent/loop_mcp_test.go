package agent

import (
	"errors"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

func TestMCPRuntime_InitialState(t *testing.T) {
	r := &mcpRuntime{}
	if r.getInitErr() != nil {
		t.Error("initial initErr should be nil")
	}
	if r.hasManager() {
		t.Error("fresh mcpRuntime should have no manager")
	}
	if r.takeManager() != nil {
		t.Error("takeManager on fresh runtime should return nil")
	}
}

func TestMCPRuntime_SetGetInitErr(t *testing.T) {
	r := &mcpRuntime{}
	e := errors.New("test error")
	r.setInitErr(e)
	if got := r.getInitErr(); got != e {
		t.Errorf("getInitErr = %v, want %v", got, e)
	}
}

func TestMCPRuntime_SetInitErr_Nil(t *testing.T) {
	r := &mcpRuntime{}
	r.setInitErr(errors.New("some error"))
	r.setInitErr(nil)
	if r.getInitErr() != nil {
		t.Error("setInitErr(nil) should clear error")
	}
}

func TestMCPRuntime_SetManager_ClearsInitErr(t *testing.T) {
	r := &mcpRuntime{}
	r.setInitErr(errors.New("prior error"))
	r.setManager(nil)
	if r.getInitErr() != nil {
		t.Error("setManager should clear initErr")
	}
}

func TestMCPRuntime_HasManager_False(t *testing.T) {
	r := &mcpRuntime{}
	if r.hasManager() {
		t.Error("fresh mcpRuntime should have no manager")
	}
}

func TestMCPRuntime_TakeManager_NilWhenEmpty(t *testing.T) {
	r := &mcpRuntime{}
	if r.takeManager() != nil {
		t.Error("takeManager on empty runtime should return nil")
	}
}

func TestEnsureMCPInitialized_NoServers(t *testing.T) {
	al, cfg, _, _, cleanup := newTestAgentLoop(t)
	defer cleanup()

	cfg.Tools.MCP.Servers = nil
	al.cfg = cfg

	ctx := t.Context()
	if err := al.ensureMCPInitialized(ctx); err != nil {
		t.Errorf("ensureMCPInitialized with no servers should succeed: %v", err)
	}
}

func TestEnsureMCPInitialized_AllServersDisabled(t *testing.T) {
	al, cfg, _, _, cleanup := newTestAgentLoop(t)
	defer cleanup()

	cfg.Tools.MCP.Servers = map[string]config.MCPServerConfig{
		"test-server": {Enabled: false, Command: "echo"},
	}
	al.cfg = cfg

	ctx := t.Context()
	if err := al.ensureMCPInitialized(ctx); err != nil {
		t.Errorf("ensureMCPInitialized with all disabled servers should succeed: %v", err)
	}
}

func TestAgentRegistry_AllowedMCPServers(t *testing.T) {
	t.Run("returns nil when any agent allows all servers", func(t *testing.T) {
		registry := &AgentRegistry{
			agents: map[string]*AgentInstance{
				"main":     {ID: "main", MCPServerAllowlist: nil},
				"research": {ID: "research", MCPServerAllowlist: map[string]struct{}{"github": {}}},
			},
		}

		if allowed := registry.allowedMCPServers(); allowed != nil {
			t.Fatalf("expected nil union when one agent allows all, got %v", allowed)
		}
	})

	t.Run("returns union of explicit allowlists", func(t *testing.T) {
		registry := &AgentRegistry{
			agents: map[string]*AgentInstance{
				"main":     {ID: "main", MCPServerAllowlist: map[string]struct{}{"github": {}}},
				"research": {ID: "research", MCPServerAllowlist: map[string]struct{}{"filesystem": {}}},
			},
		}

		allowed := registry.allowedMCPServers()
		if len(allowed) != 2 {
			t.Fatalf("len(allowed) = %d, want 2", len(allowed))
		}
		if _, ok := allowed["github"]; !ok {
			t.Fatal("expected github in allowed MCP server union")
		}
		if _, ok := allowed["filesystem"]; !ok {
			t.Fatal("expected filesystem in allowed MCP server union")
		}
	})
}

func TestFilterMCPConfigServers(t *testing.T) {
	mcpCfg := config.MCPConfig{
		ToolConfig: config.ToolConfig{Enabled: true},
		Servers: map[string]config.MCPServerConfig{
			"github":     {Enabled: true},
			"filesystem": {Enabled: true},
		},
	}

	filtered := filterMCPConfigServers(mcpCfg, map[string]struct{}{"github": {}})
	if len(filtered.Servers) != 1 {
		t.Fatalf("len(filtered.Servers) = %d, want 1", len(filtered.Servers))
	}
	if _, ok := filtered.Servers["github"]; !ok {
		t.Fatal("expected github server to remain after filtering")
	}
	if _, ok := filtered.Servers["filesystem"]; ok {
		t.Fatal("expected filesystem server to be removed by filtering")
	}
}
