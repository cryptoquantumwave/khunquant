package gateway

import (
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/channels"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

// The simulator starts before the shared HTTP server binds the gateway's port, so both
// ways of starting it must tell it that port: otherwise it can take it and the gateway
// dies with "address already in use" (CI run 41406353719).

// The unlocked path: startAndRegisterSandbox.
func TestStartAndRegisterSandbox_AvoidsTheGatewayPort(t *testing.T) {
	t.Cleanup(func() { sandbox.SetGlobalState(false, "") })
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = t.TempDir()
	cfg.Gateway.Port = 45678
	cfg.Debug.Sandbox.Enabled = true
	cfg.Debug.Sandbox.Token = "test-token" // so no token is persisted into a real config

	manager, err := channels.NewManager(cfg, bus.NewMessageBus(), nil)
	if err != nil {
		t.Fatal(err)
	}
	services := &gatewayServices{ChannelManager: manager}
	if err := startAndRegisterSandbox(manager, cfg, services); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { services.SandboxServer.Stop() })
	if got := services.SandboxServer.AvoidedPort(); got != 45678 {
		t.Fatalf("the sandbox server avoids port %d, want the gateway's 45678", got)
	}
}

// The locked path: buildLab, as the locked fixture (and gatewayCmd) call it.
func TestBuildLab_AvoidsTheGatewayPort(t *testing.T) {
	f := newLockedFixture(t)
	if f.port == 0 || f.initial.Gateway.Port != f.port {
		t.Fatalf("setup: fixture port %d, config port %d", f.port, f.initial.Gateway.Port)
	}
	if got := f.lab.Server.AvoidedPort(); got != f.port {
		t.Fatalf("the lab's server avoids port %d, want the gateway's %d", got, f.port)
	}
}
