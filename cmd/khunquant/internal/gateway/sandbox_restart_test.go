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

	if err := startAndRegisterSandbox(cfg, services); err != nil {
		t.Fatalf("first start failed: %v", err)
	}
	first := services.SandboxServer
	if first == nil || !first.IsRunning() {
		t.Fatal("expected a running sandbox server after the first start")
	}
	firstAddr := first.Addr()

	if err := startAndRegisterSandbox(cfg, services); err != nil {
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

	if err := startAndRegisterSandbox(cfg, services); err != nil {
		t.Fatalf("start with no fixtures should succeed, got: %v", err)
	}
	t.Cleanup(func() { services.SandboxServer.Stop() })

	if venues := services.SandboxServer.GetVenues(); len(venues) != 0 {
		t.Errorf("expected no venues, got %v", venues)
	}
}
