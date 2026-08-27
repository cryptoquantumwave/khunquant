package api

import (
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

func TestEnsurePicoChannel_FreshConfig(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	h := NewHandler(configPath)

	changed, err := h.ensurePicoChannel("")
	if err != nil {
		t.Fatalf("ensurePicoChannel() error = %v", err)
	}
	if !changed {
		t.Fatal("ensurePicoChannel() should report changed on a fresh config")
	}

	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}

	if !cfg.Channels.Pico.Enabled {
		t.Error("expected Pico to be enabled after setup")
	}
	if cfg.Channels.Pico.Token.String() == "" {
		t.Error("expected a non-empty token after setup")
	}
}

func TestEnsurePicoChannel_DoesNotEnableTokenQuery(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	h := NewHandler(configPath)

	if _, err := h.ensurePicoChannel(""); err != nil {
		t.Fatalf("ensurePicoChannel() error = %v", err)
	}

	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}

	if cfg.Channels.Pico.AllowTokenQuery {
		t.Error("setup must not enable allow_token_query by default")
	}
}

func TestEnsurePicoChannel_DoesNotSetWildcardOrigins(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	h := NewHandler(configPath)

	if _, err := h.ensurePicoChannel("http://localhost:18800"); err != nil {
		t.Fatalf("ensurePicoChannel() error = %v", err)
	}

	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}

	for _, origin := range cfg.Channels.Pico.AllowOrigins {
		if origin == "*" {
			t.Error("setup must not set wildcard origin '*'")
		}
	}
}

func TestEnsurePicoChannel_NoOriginWithoutCaller(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	h := NewHandler(configPath)

	if _, err := h.ensurePicoChannel(""); err != nil {
		t.Fatalf("ensurePicoChannel() error = %v", err)
	}

	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}

	// Without a caller origin, allow_origins stays empty (CheckOrigin
	// allows all when the list is empty, so the channel still works).
	if len(cfg.Channels.Pico.AllowOrigins) != 0 {
		t.Errorf("allow_origins = %v, want empty when no caller origin", cfg.Channels.Pico.AllowOrigins)
	}
}

func TestEnsurePicoChannel_SetsCallerOrigin(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	h := NewHandler(configPath)

	lanOrigin := "http://192.168.1.9:18800"
	if _, err := h.ensurePicoChannel(lanOrigin); err != nil {
		t.Fatalf("ensurePicoChannel() error = %v", err)
	}

	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}

	if len(cfg.Channels.Pico.AllowOrigins) != 1 || cfg.Channels.Pico.AllowOrigins[0] != lanOrigin {
		t.Errorf("allow_origins = %v, want [%s]", cfg.Channels.Pico.AllowOrigins, lanOrigin)
	}
}

func TestEnsurePicoChannel_PreservesUserSettings(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")

	// Pre-configure with custom user settings
	cfg := config.DefaultConfig()
	cfg.Channels.Pico.Enabled = true
	cfg.Channels.Pico.Token.Set("user-custom-token")
	cfg.Channels.Pico.AllowTokenQuery = true
	cfg.Channels.Pico.AllowOrigins = []string{"https://myapp.example.com"}
	if err := config.SaveConfig(configPath, cfg); err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}

	h := NewHandler(configPath)

	changed, err := h.ensurePicoChannel("")
	if err != nil {
		t.Fatalf("ensurePicoChannel() error = %v", err)
	}
	if changed {
		t.Error("ensurePicoChannel() should not change a fully configured config")
	}

	cfg, err = config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}

	if cfg.Channels.Pico.Token.String() != "user-custom-token" {
		t.Errorf("token = %q, want %q", cfg.Channels.Pico.Token.String(), "user-custom-token")
	}
	if !cfg.Channels.Pico.AllowTokenQuery {
		t.Error("user's allow_token_query=true must be preserved")
	}
	if len(cfg.Channels.Pico.AllowOrigins) != 1 || cfg.Channels.Pico.AllowOrigins[0] != "https://myapp.example.com" {
		t.Errorf("allow_origins = %v, want [https://myapp.example.com]", cfg.Channels.Pico.AllowOrigins)
	}
}

func TestEnsurePicoChannel_Idempotent(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	h := NewHandler(configPath)

	origin := "http://localhost:18800"

	// First call sets things up
	if _, err := h.ensurePicoChannel(origin); err != nil {
		t.Fatalf("first ensurePicoChannel() error = %v", err)
	}

	cfg1, _ := config.LoadConfig(configPath)
	token1 := cfg1.Channels.Pico.Token

	// Second call should be a no-op
	changed, err := h.ensurePicoChannel(origin)
	if err != nil {
		t.Fatalf("second ensurePicoChannel() error = %v", err)
	}
	if changed {
		t.Error("second ensurePicoChannel() should not report changed")
	}

	cfg2, _ := config.LoadConfig(configPath)
	if cfg2.Channels.Pico.Token != token1 {
		t.Error("token should not change on subsequent calls")
	}
}

func TestHandlePicoSetup_IncludesRequestOrigin(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	h := NewHandler(configPath)

	req := httptest.NewRequest("POST", "/api/pico/setup", nil)
	// Set host to match the origin so this is a same-origin request
	req.Host = "10.0.0.5:3000"
	req.Header.Set("Origin", "http://10.0.0.5:3000")
	rec := httptest.NewRecorder()

	h.handlePicoSetup(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}

	if len(cfg.Channels.Pico.AllowOrigins) != 1 || cfg.Channels.Pico.AllowOrigins[0] != "http://10.0.0.5:3000" {
		t.Errorf("allow_origins = %v, want [http://10.0.0.5:3000]", cfg.Channels.Pico.AllowOrigins)
	}
}

func TestHandlePicoSetup_Response(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	h := NewHandler(configPath)

	req := httptest.NewRequest("POST", "/api/pico/setup", nil)
	// Add Sec-Fetch-Site header to pass CSRF check so we can test the response body
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()

	h.handlePicoSetup(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var resp map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if _, hasToken := resp["token"]; hasToken {
		t.Error("response must not contain the token (security: C6)")
	}
	if resp["ws_url"] == nil || resp["ws_url"] == "" {
		t.Error("response should contain ws_url")
	}
	if resp["enabled"] != true {
		t.Error("response should have enabled=true")
	}
	if resp["changed"] != true {
		t.Error("response should have changed=true on first setup")
	}
}

// CSRF Protection Tests

func TestIsSameLauncherRequestOrigin_SecFetchSiteHandling(t *testing.T) {
	tests := []struct {
		name      string
		fetchSite string
		wantAllow bool
		desc      string
	}{
		{"same-origin", "same-origin", true, "same-origin short-circuits and accepts"},
		{"cross-site", "cross-site", false, "cross-site is rejected immediately"},
		{"same-site", "same-site", false, "same-site is rejected immediately (no legitimate cross-subdomain callers)"},
		{"none", "none", false, "none falls through to Origin check (no Origin present, so reject)"},
		{"empty", "", false, "absent Sec-Fetch-Site falls through to Origin check (no Origin present, so reject)"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/api/pico/setup", nil)
			if tt.fetchSite != "" {
				req.Header.Set("Sec-Fetch-Site", tt.fetchSite)
			}
			// No Origin or Referer headers set; only Sec-Fetch-Site
			got := isSameLauncherRequestOrigin(req)
			if got != tt.wantAllow {
				t.Errorf("isSameLauncherRequestOrigin() = %v, want %v (reason: %s)", got, tt.wantAllow, tt.desc)
			}
		})
	}
}

func TestIsSameLauncherRequestOrigin_OriginFallback(t *testing.T) {
	tests := []struct {
		name      string
		host      string
		origin    string
		tls       bool
		wantAllow bool
	}{
		{"matching origin", "localhost:8080", "http://localhost:8080", false, true},
		{"foreign origin", "localhost:8080", "http://attacker.com", false, true},
		{"empty origin", "localhost:8080", "", false, false},
		{"https matching", "localhost:8443", "https://localhost:8443", true, true},
		{"https mismatch", "localhost:8443", "http://localhost:8443", true, false},
		{"null origin", "localhost:8080", "null", false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/api/pico/setup", nil)
			req.Host = tt.host
			if tt.tls {
				req.TLS = &tls.ConnectionState{}
			}
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}
			got := isSameLauncherRequestOrigin(req)
			// Invert for foreign origin test: when both headers absent, we reject
			if tt.name == "foreign origin" {
				// Foreign origin with Sec-Fetch-Site absent -> falls back to Origin check
				// But Origin says "http://attacker.com" != "http://localhost:8080" -> should reject
				if got != false {
					t.Errorf("isSameLauncherRequestOrigin() = %v, want false (foreign origin)", got)
				}
				return
			}
			if got != tt.wantAllow {
				t.Errorf("isSameLauncherRequestOrigin() = %v, want %v", got, tt.wantAllow)
			}
		})
	}
}

func TestHandlePicoSetup_RejectsCrossSiteRequest(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	h := NewHandler(configPath)

	req := httptest.NewRequest("POST", "/api/pico/setup", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()

	h.handlePicoSetup(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d (Forbidden)", rec.Code, http.StatusForbidden)
	}

	// Verify config was not mutated: ensurePicoChannel should NOT have run
	cfg, err := config.LoadConfig(configPath)
	if err == nil {
		// Config was created, check defaults
		if cfg.Channels.Pico.Enabled {
			t.Error("CSRF rejected request must not enable Pico channel")
		}
		if cfg.Channels.Pico.Token.String() != "" {
			t.Error("CSRF rejected request must not generate a token")
		}
	}
}

func TestHandlePicoSetup_RejectsSameSiteRequest(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	h := NewHandler(configPath)

	req := httptest.NewRequest("POST", "/api/pico/setup", nil)
	req.Header.Set("Sec-Fetch-Site", "same-site")
	rec := httptest.NewRecorder()

	h.handlePicoSetup(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d (Forbidden)", rec.Code, http.StatusForbidden)
	}

	// Verify config was not mutated
	cfg, err := config.LoadConfig(configPath)
	if err == nil && cfg.Channels.Pico.Enabled {
		t.Error("CSRF rejected request must not enable Pico channel")
	}
}

func TestHandlePicoSetup_AcceptsSecFetchSiteNoneWithMatchingOrigin(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	h := NewHandler(configPath)

	req := httptest.NewRequest("POST", "/api/pico/setup", nil)
	req.Host = "localhost:8080"
	req.Header.Set("Sec-Fetch-Site", "none")
	req.Header.Set("Origin", "http://localhost:8080")
	rec := httptest.NewRecorder()

	h.handlePicoSetup(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (OK)", rec.Code, http.StatusOK)
	}

	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if !cfg.Channels.Pico.Enabled {
		t.Error("Sec-Fetch-Site: none with matching Origin must enable Pico channel")
	}
	if cfg.Channels.Pico.Token.String() == "" {
		t.Error("Sec-Fetch-Site: none with matching Origin must generate a token")
	}
}

func TestHandlePicoSetup_RejectsForeignOriginNoSecFetchSite(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	h := NewHandler(configPath)

	req := httptest.NewRequest("POST", "/api/pico/setup", nil)
	req.Host = "localhost:8080"
	req.Header.Set("Origin", "http://attacker.com")
	rec := httptest.NewRecorder()

	h.handlePicoSetup(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d (Forbidden)", rec.Code, http.StatusForbidden)
	}

	// Verify config was not mutated
	cfg, err := config.LoadConfig(configPath)
	if err == nil {
		if cfg.Channels.Pico.Enabled {
			t.Error("cross-origin request must not enable Pico channel")
		}
		if cfg.Channels.Pico.Token.String() != "" {
			t.Error("cross-origin request must not generate a token")
		}
		if len(cfg.Channels.Pico.AllowOrigins) > 0 {
			t.Errorf("cross-origin request must not plant origin; got %v", cfg.Channels.Pico.AllowOrigins)
		}
	}
}

func TestHandlePicoSetup_AcceptsMatchingOrigin(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	h := NewHandler(configPath)

	req := httptest.NewRequest("POST", "/api/pico/setup", nil)
	req.Host = "localhost:8080"
	req.Header.Set("Origin", "http://localhost:8080")
	rec := httptest.NewRecorder()

	h.handlePicoSetup(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (OK)", rec.Code, http.StatusOK)
	}

	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if !cfg.Channels.Pico.Enabled {
		t.Error("same-origin request must enable Pico channel")
	}
	if cfg.Channels.Pico.Token.String() == "" {
		t.Error("same-origin request must generate a token")
	}
	if len(cfg.Channels.Pico.AllowOrigins) != 1 || cfg.Channels.Pico.AllowOrigins[0] != "http://localhost:8080" {
		t.Errorf("same-origin request must plant the origin; got %v", cfg.Channels.Pico.AllowOrigins)
	}
}

func TestHandlePicoSetup_RejectsBothHeadersAbsent(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	h := NewHandler(configPath)

	req := httptest.NewRequest("POST", "/api/pico/setup", nil)
	// Don't set Sec-Fetch-Site or Origin headers
	rec := httptest.NewRecorder()

	h.handlePicoSetup(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d (Forbidden)", rec.Code, http.StatusForbidden)
	}

	// Verify config was not mutated: ensurePicoChannel should NOT have run
	cfg, err := config.LoadConfig(configPath)
	if err == nil {
		if cfg.Channels.Pico.Enabled {
			t.Error("request with both headers absent must not enable Pico channel")
		}
		if cfg.Channels.Pico.Token.String() != "" {
			t.Error("request with both headers absent must not generate a token")
		}
	}
}

func TestHandlePicoSetup_RejectsSecFetchSiteNoneWithForeignOrigin(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	h := NewHandler(configPath)

	req := httptest.NewRequest("POST", "/api/pico/setup", nil)
	req.Host = "localhost:8080"
	req.Header.Set("Sec-Fetch-Site", "none")
	req.Header.Set("Origin", "http://attacker.com")
	rec := httptest.NewRecorder()

	h.handlePicoSetup(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d (Forbidden)", rec.Code, http.StatusForbidden)
	}

	// Verify config was not mutated: even though Sec-Fetch-Site: none,
	// the foreign Origin should cause rejection and ensurePicoChannel not to run
	cfg, err := config.LoadConfig(configPath)
	if err == nil {
		if cfg.Channels.Pico.Enabled {
			t.Error("Sec-Fetch-Site: none with foreign Origin must not enable Pico channel")
		}
		if cfg.Channels.Pico.Token.String() != "" {
			t.Error("Sec-Fetch-Site: none with foreign Origin must not generate a token")
		}
		if len(cfg.Channels.Pico.AllowOrigins) > 0 {
			t.Errorf("Sec-Fetch-Site: none with foreign Origin must not plant origin; got %v", cfg.Channels.Pico.AllowOrigins)
		}
	}
}
