package gateway

import (
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/channels"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

// TestStartAndRegisterSandboxStopsPreviousServer covers the reload path where
// handleConfigReload fails to build the new provider and calls restartServices
// with the old config: that path never passes through the enable/disable toggle,
// so a second start must stop the first server rather than strand its listener.
func TestStartAndRegisterSandboxStopsPreviousServer(t *testing.T) {
	t.Cleanup(func() { sandbox.SetGlobalState(false, "") })

	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = t.TempDir()
	cfg.Debug.Sandbox.Enabled = true
	// Non-empty so startAndRegisterSandbox does not persist a generated token
	// into the developer's real config file during the test.
	cfg.Debug.Sandbox.Token = "test-token"

	manager, err := channels.NewManager(cfg, bus.NewMessageBus(), nil)
	if err != nil {
		t.Fatalf("failed to create channel manager: %v", err)
	}
	services := &gatewayServices{ChannelManager: manager}

	if err := startAndRegisterSandbox(manager, cfg, services); err != nil {
		t.Fatalf("first start failed: %v", err)
	}
	first := services.SandboxServer
	if first == nil || !first.IsRunning() {
		t.Fatal("expected a running sandbox server after the first start")
	}
	firstAddr := first.Addr()

	if err := startAndRegisterSandbox(manager, cfg, services); err != nil {
		t.Fatalf("second start failed: %v", err)
	}
	t.Cleanup(func() { services.SandboxServer.Stop() })

	if first.IsRunning() {
		t.Errorf("first sandbox server still running on %s; its listener leaked", firstAddr)
	}
	second := services.SandboxServer
	if second == first {
		t.Fatal("expected a fresh sandbox server after the second start")
	}
	if !second.IsRunning() {
		t.Error("expected the second sandbox server to be running")
	}
}

// TestStartAndRegisterSandboxWithoutFixtures documents that a missing fixtures
// directory is not a startup error: the store loads empty and the gateway comes
// up (with a warning), rather than failing the whole reload.
func TestStartAndRegisterSandboxWithoutFixtures(t *testing.T) {
	t.Cleanup(func() { sandbox.SetGlobalState(false, "") })

	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = t.TempDir() // no sandbox/ subdirectory
	cfg.Debug.Sandbox.Enabled = true
	cfg.Debug.Sandbox.Token = "test-token"

	manager, err := channels.NewManager(cfg, bus.NewMessageBus(), nil)
	if err != nil {
		t.Fatalf("failed to create channel manager: %v", err)
	}
	services := &gatewayServices{ChannelManager: manager}

	if err := startAndRegisterSandbox(manager, cfg, services); err != nil {
		t.Fatalf("start with no fixtures should succeed, got: %v", err)
	}
	t.Cleanup(func() { services.SandboxServer.Stop() })

	if venues := services.SandboxServer.GetVenues(); len(venues) != 0 {
		t.Errorf("expected no venues, got %v", venues)
	}
}

// A reload that keeps the sandbox on leaves the sandbox state armed with no server between
// the old server and the next one (130-3, AS-3). It used to switch the state off there, so
// a tool call during the reload went to the real venue, and an exchange client built then
// kept real URLs in the cache.
func TestReloadKeepingTheSandbox_StaysArmedBetweenServers(t *testing.T) {
	t.Cleanup(func() { sandbox.SetGlobalState(false, "") })

	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = t.TempDir()
	cfg.Debug.Sandbox.Enabled = true
	cfg.Debug.Sandbox.Token = "test-token"
	manager, err := channels.NewManager(cfg, bus.NewMessageBus(), nil)
	if err != nil {
		t.Fatal(err)
	}
	services := &gatewayServices{ChannelManager: manager}
	if err := startAndRegisterSandbox(manager, cfg, services); err != nil {
		t.Fatal(err)
	}
	old := services.SandboxServer
	if enabled, base := sandbox.GlobalState(); !enabled || base == "" {
		t.Fatalf("with the server up: enabled=%v base=%q", enabled, base)
	}

	stopSandboxForRestart(services) // what handleConfigReload does when the sandbox stays on

	if old.IsRunning() || services.SandboxServer != nil || services.SandboxToken != "" {
		t.Errorf("the old server was not retired: running=%v server=%v token=%q", old.IsRunning(), services.SandboxServer, services.SandboxToken)
	}
	if enabled, base := sandbox.GlobalState(); !enabled || base != "" {
		t.Fatalf("between the servers: enabled=%v base=%q, want armed with no server (fail closed)", enabled, base)
	}

	// The next server takes over.
	if err := startAndRegisterSandbox(manager, cfg, services); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { services.SandboxServer.Stop() })
	if enabled, base := sandbox.GlobalState(); !enabled || base == "" {
		t.Errorf("after the restart: enabled=%v base=%q", enabled, base)
	}
}
