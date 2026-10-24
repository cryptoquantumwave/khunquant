package api

import (
	"crypto/tls"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/web/backend/launcherconfig"
)

// TestGatewayBindHostIgnoresLauncherPublic pins H-10, inverting the assertion
// this test used to make. Running the launcher with -public forced
// KHUNQUANT_GATEWAY_HOST=0.0.0.0 onto the gateway child process, putting
// webhooks and pico on the LAN whatever gateway.host said. The launcher's own
// listen address is now its own business.
func TestGatewayBindHostIgnoresLauncherPublic(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	launcherPath := launcherconfig.PathForAppConfig(configPath)
	if err := launcherconfig.Save(launcherPath, launcherconfig.Config{
		Port:   18800,
		Public: true,
	}); err != nil {
		t.Fatalf("launcherconfig.Save() error = %v", err)
	}

	h := NewHandler(configPath)
	h.SetServerOptions(18800, true, true, nil)

	cfg := config.DefaultConfig()
	cfg.Gateway.Host = "127.0.0.1"
	if got := h.effectiveGatewayBindHost(cfg); got != "127.0.0.1" {
		t.Fatalf("effectiveGatewayBindHost() = %q, want %q (launcher -public must not override it)",
			got, "127.0.0.1")
	}

	// An unset gateway.host stays unset rather than becoming 0.0.0.0.
	cfg.Gateway.Host = ""
	if got := h.effectiveGatewayBindHost(cfg); got != "" {
		t.Fatalf("effectiveGatewayBindHost() = %q, want \"\" for an unset gateway.host", got)
	}
}

func TestBuildWsURLUsesRequestHostWhenLauncherPublicSaved(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	launcherPath := launcherconfig.PathForAppConfig(configPath)
	if err := launcherconfig.Save(launcherPath, launcherconfig.Config{
		Port:   18800,
		Public: true,
	}); err != nil {
		t.Fatalf("launcherconfig.Save() error = %v", err)
	}

	h := NewHandler(configPath)
	h.SetServerOptions(18800, false, false, nil)

	req := httptest.NewRequest("GET", "http://launcher.local/api/pico/token", nil)
	req.Host = "192.168.1.9:18800"

	if got := h.buildWsURL(req); got != "ws://192.168.1.9:18800/pico/ws" {
		t.Fatalf("buildWsURL() = %q, want %q", got, "ws://192.168.1.9:18800/pico/ws")
	}
}

func TestGatewayProbeHostUsesLoopbackForWildcardBind(t *testing.T) {
	if got := gatewayProbeHost("0.0.0.0"); got != "127.0.0.1" {
		t.Fatalf("gatewayProbeHost() = %q, want %q", got, "127.0.0.1")
	}
}

func TestBuildWsURLUsesWSSWhenForwardedProtoIsHTTPS(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	h := NewHandler(configPath)

	req := httptest.NewRequest("GET", "http://launcher.local/api/pico/token", nil)
	req.Host = "chat.example.com"
	req.Header.Set("X-Forwarded-Proto", "https")

	if got := h.buildWsURL(req); got != "wss://chat.example.com:443/pico/ws" {
		t.Fatalf("buildWsURL() = %q, want %q", got, "wss://chat.example.com:443/pico/ws")
	}
}

func TestBuildWsURLUsesWSSWhenRequestIsTLS(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	h := NewHandler(configPath)

	req := httptest.NewRequest("GET", "https://launcher.local/api/pico/token", nil)
	req.Host = "secure.example.com"
	req.TLS = &tls.ConnectionState{}

	if got := h.buildWsURL(req); got != "wss://secure.example.com:443/pico/ws" {
		t.Fatalf("buildWsURL() = %q, want %q", got, "wss://secure.example.com:443/pico/ws")
	}
}

func TestBuildWsURLPrefersForwardedHTTPOverTLS(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	h := NewHandler(configPath)

	req := httptest.NewRequest("GET", "https://launcher.local/api/pico/token", nil)
	req.Host = "chat.example.com"
	req.TLS = &tls.ConnectionState{}
	req.Header.Set("X-Forwarded-Proto", "http")

	if got := h.buildWsURL(req); got != "ws://chat.example.com:80/pico/ws" {
		t.Fatalf("buildWsURL() = %q, want %q", got, "ws://chat.example.com:80/pico/ws")
	}
}

func TestBuildWsURLDoesNotTrustOriginWhenProxyOmitsForwardedProto(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	h := NewHandler(configPath)

	req := httptest.NewRequest("GET", "http://launcher.local/api/pico/info", nil)
	req.Host = "fs-952210-xwj.picoclaw.lan.sipeed.com"
	req.Header.Set("Origin", "https://fs-952210-xwj.picoclaw.lan.sipeed.com")

	if got := h.buildWsURL(req); got != "ws://fs-952210-xwj.picoclaw.lan.sipeed.com:80/pico/ws" {
		t.Fatalf(
			"buildWsURL() = %q, want %q",
			got,
			"ws://fs-952210-xwj.picoclaw.lan.sipeed.com:80/pico/ws",
		)
	}
}
