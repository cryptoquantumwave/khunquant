package api

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

func TestAllowStateChange_SecFetchSiteHandling(t *testing.T) {
	tests := []struct {
		name      string
		fetchSite string
		wantAllow bool
	}{
		{"same-origin", "same-origin", true},
		{"cross-site", "cross-site", false},
		{"same-site", "same-site", false},
		{"none", "none", false},
		{"empty", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/api/config", nil)
			if tt.fetchSite != "" {
				req.Header.Set("Sec-Fetch-Site", tt.fetchSite)
			}
			rec := httptest.NewRecorder()

			got := allowStateChange(rec, req)
			if got != tt.wantAllow {
				t.Errorf("allowStateChange() = %v, want %v", got, tt.wantAllow)
			}
			if !got && rec.Code != http.StatusForbidden {
				t.Errorf("status code = %d, want %d (Forbidden) when check fails", rec.Code, http.StatusForbidden)
			}
		})
	}
}

func TestAllowStateChange_OriginFallback(t *testing.T) {
	tests := []struct {
		name      string
		host      string
		origin    string
		wantAllow bool
	}{
		{"matching origin", "localhost:8080", "http://localhost:8080", true},
		{"foreign origin", "localhost:8080", "http://attacker.com", false},
		{"empty origin with no referer", "localhost:8080", "", false},
		{"null origin", "localhost:8080", "null", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/api/config", nil)
			req.Host = tt.host
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}
			rec := httptest.NewRecorder()

			got := allowStateChange(rec, req)
			if got != tt.wantAllow {
				t.Errorf("allowStateChange() = %v, want %v", got, tt.wantAllow)
			}
		})
	}
}

func TestAllowStateChange_RejectsHeaderless(t *testing.T) {
	req := httptest.NewRequest("POST", "/api/config", nil)
	// No Sec-Fetch-Site, Origin, or Referer
	rec := httptest.NewRecorder()

	got := allowStateChange(rec, req)
	if got {
		t.Errorf("allowStateChange() = %v, want false (headerless request)", got)
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d (Forbidden)", rec.Code, http.StatusForbidden)
	}
}

// TestCSRFProtectedEndpoints is a table-driven test that verifies all state-changing endpoints
// reject cross-site requests and accept same-origin requests.
// This serves as a mutation test for the CSRF protection.
func TestCSRFProtectedEndpoints(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	h := NewHandler(configPath)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	// State-changing endpoints that are currently CSRF-protected.
	// This test will grow as more endpoints are protected.
	protectedRoutes := []struct {
		method   string
		path     string
		name     string
		bodyType string // json, form, or empty
	}{
		// Config
		{"PUT", "/api/config", "config_update", "json"},
		{"PATCH", "/api/config", "config_patch", "json"},
		{"POST", "/api/config/reset", "config_reset", "empty"},

		// Pico
		{"POST", "/api/pico/token", "pico_regen_token", "empty"},
		{"POST", "/api/pico/setup", "pico_setup", "empty"},

		// Gateway
		{"POST", "/api/gateway/start", "gateway_start", "empty"},
		{"POST", "/api/gateway/stop", "gateway_stop", "empty"},
		{"POST", "/api/gateway/restart", "gateway_restart", "empty"},
		{"POST", "/api/gateway/logs/clear", "gateway_logs_clear", "empty"},

		// Models
		{"POST", "/api/models", "models_add", "json"},
		{"POST", "/api/models/default", "models_default", "json"},
		{"DELETE", "/api/models/0", "models_delete", "empty"},

		// System
		{"PUT", "/api/system/launcher-config", "launcher_config_update", "json"},

		// Session
		{"DELETE", "/api/sessions/test-id", "session_delete", "empty"},

		// OAuth
		{"POST", "/api/oauth/login", "oauth_login", "json"},
		{"POST", "/api/oauth/logout", "oauth_logout", "json"},
		{"POST", "/api/oauth/flows/test-flow/poll", "oauth_poll", "empty"},
		{"POST", "/api/oauth/providers/openai/select-model", "oauth_select_model", "json"},

		// Skills
		{"POST", "/api/skills/import", "skills_import", "json"},
		{"DELETE", "/api/skills/test", "skills_delete", "empty"},

		// Tools
		{"PUT", "/api/tools/test/state", "tools_state", "json"},

		// Update
		{"POST", "/api/update/apply", "update_apply", "json"},
	}

	for _, route := range protectedRoutes {
		// Test 1: Cross-site request should be rejected
		t.Run(route.name+"_csrf_rejected", func(t *testing.T) {
			req := httptest.NewRequest(route.method, route.path, strings.NewReader("{}"))
			if route.bodyType == "json" {
				req.Header.Set("Content-Type", "application/json")
			}
			req.Header.Set("Sec-Fetch-Site", "cross-site")
			rec := httptest.NewRecorder()

			mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Errorf("%s: cross-site request status = %d, want %d (Forbidden)",
					route.name, rec.Code, http.StatusForbidden)
			}
		})

		// Test 2: Same-origin request should not return Forbidden
		t.Run(route.name+"_same_origin_allowed", func(t *testing.T) {
			req := httptest.NewRequest(route.method, route.path, strings.NewReader("{}"))
			if route.bodyType == "json" {
				req.Header.Set("Content-Type", "application/json")
			}
			req.Header.Set("Sec-Fetch-Site", "same-origin")
			rec := httptest.NewRecorder()

			mux.ServeHTTP(rec, req)

			if rec.Code == http.StatusForbidden {
				t.Errorf("%s: same-origin request got 403 Forbidden (should pass CSRF check)", route.name)
			}
		})
	}
}

// TestConfigResetMutationIsPrevented verifies that a CSRF-rejected config reset
// does not actually mutate the config file.
func TestConfigResetMutationIsPrevented(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")

	// Create initial config
	initialCfg := config.DefaultConfig()
	initialCfg.Agents.Defaults.Model = "test-model-1"
	if err := config.SaveConfig(configPath, initialCfg); err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}

	// Attempt reset via CSRF attack
	h := NewHandler(configPath)
	req := httptest.NewRequest("POST", "/api/config/reset", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site") // Attacker's request
	rec := httptest.NewRecorder()

	h.handleResetConfig(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (Forbidden)", rec.Code)
	}

	// Verify config was NOT mutated
	loadedCfg, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}

	if loadedCfg.Agents.Defaults.Model != "test-model-1" {
		t.Errorf("config was mutated! model = %q, want %q",
			loadedCfg.Agents.Defaults.Model, "test-model-1")
	}
}

// TestPicoTokenRegenerationMutationIsPrevented verifies that a CSRF-rejected token regen
// does not actually change the token.
func TestPicoTokenRegenerationMutationIsPrevented(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	h := NewHandler(configPath)

	// Set up initial token
	if _, err := h.ensurePicoChannel("http://localhost:8080"); err != nil {
		t.Fatalf("ensurePicoChannel() error = %v", err)
	}

	cfg, _ := config.LoadConfig(configPath)
	originalToken := cfg.Channels.Pico.Token.String()

	// Attempt token regen via CSRF attack
	req := httptest.NewRequest("POST", "/api/pico/token", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site") // Attacker's request
	rec := httptest.NewRecorder()

	h.handleRegenPicoToken(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (Forbidden)", rec.Code)
	}

	// Verify token was NOT mutated
	loadedCfg, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}

	if loadedCfg.Channels.Pico.Token.String() != originalToken {
		t.Errorf("token was mutated! new = %q, old = %q",
			loadedCfg.Channels.Pico.Token.String(), originalToken)
	}
}

// TestPicoSetupMutationStillPrevented ensures the already-protected endpoint
// still prevents mutation on CSRF rejection.
func TestPicoSetupMutationStillPrevented(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	h := NewHandler(configPath)

	// Attempt setup via CSRF attack (foreign origin)
	req := httptest.NewRequest("POST", "/api/pico/setup", nil)
	req.Host = "localhost:8080"
	req.Header.Set("Origin", "http://attacker.com")
	rec := httptest.NewRecorder()

	h.handlePicoSetup(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (Forbidden)", rec.Code)
	}

	// Verify Pico channel was NOT enabled
	cfg, err := config.LoadConfig(configPath)
	if err == nil && cfg.Channels.Pico.Enabled {
		t.Errorf("Pico channel was enabled despite CSRF rejection")
	}
	if err == nil && cfg.Channels.Pico.Token.String() != "" {
		t.Errorf("token was generated despite CSRF rejection")
	}
}

// TestUpdateApplyMutationIsPrevented verifies that a CSRF-rejected update apply
// does not proceed with the update (though in this case we just verify the handler
// was gated, not that the download didn't happen, since that's integration-level).
func TestUpdateApplyCSRFReject(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	h := NewHandler(configPath)

	// Attempt update apply via CSRF attack
	req := httptest.NewRequest("POST", "/api/update/apply", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()

	h.handleUpdateApply(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("update apply: status = %d, want 403 (Forbidden)", rec.Code)
	}
}

// TestIsSameLauncherRequestOrigin_RefererFallback ensures Referer header works
// as a fallback when Origin is absent.
func TestIsSameLauncherRequestOrigin_RefererFallback(t *testing.T) {
	tests := []struct {
		name      string
		host      string
		referer   string
		wantAllow bool
	}{
		{"matching referer", "localhost:8080", "http://localhost:8080/some/path", true},
		{"foreign referer", "localhost:8080", "http://attacker.com/path", false},
		{"empty referer", "localhost:8080", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/api/config", nil)
			req.Host = tt.host
			if tt.referer != "" {
				req.Header.Set("Referer", tt.referer)
			}

			got := isSameLauncherRequestOrigin(req)
			if got != tt.wantAllow {
				t.Errorf("isSameLauncherRequestOrigin() = %v, want %v", got, tt.wantAllow)
			}
		})
	}
}

// TestIsSameLauncherRequestOrigin_MalformedHeaders ensures malformed headers
// are rejected for defense in depth.
func TestIsSameLauncherRequestOrigin_MalformedHeaders(t *testing.T) {
	tests := []struct {
		name     string
		header   string
		value    string
		wantReject bool
	}{
		{"origin with spaces", "Origin", "http://localhost:8080 http://localhost:8081", true},
		{"origin with newline", "Origin", "http://localhost:8080\n", true},
		{"origin with tab", "Origin", "http://localhost:8080\t", true},
		{"referer with spaces", "Referer", "http://localhost:8080/path space", true},
		{"referer with newline", "Referer", "http://localhost:8080/path\n", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/api/config", nil)
			req.Host = "localhost:8080"
			req.Header.Set(tt.header, tt.value)

			got := isSameLauncherRequestOrigin(req)
			if got {
				t.Errorf("isSameLauncherRequestOrigin() = true, want false (should reject malformed %s)", tt.header)
			}
		})
	}
}

func TestCSRFProtectedEndpoints_ModelListUpdate(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	h := NewHandler(configPath)

	// Create initial config with one model
	cfg := config.DefaultConfig()
	cfg.ModelList = []config.ModelConfig{
		{ModelName: "original-model", Model: "original"},
	}
	if err := config.SaveConfig(configPath, cfg); err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}

	// Attempt to add model via CSRF attack
	body := `{"model_name":"attacker-model","model":"attacker"}`
	req := httptest.NewRequest("POST", "/api/models", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()

	h.handleAddModel(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (Forbidden)", rec.Code)
	}

	// Verify model list was NOT mutated
	loadedCfg, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}

	if len(loadedCfg.ModelList) != 1 {
		t.Errorf("model list was mutated! count = %d, want 1", len(loadedCfg.ModelList))
	}
	if len(loadedCfg.ModelList) > 0 && loadedCfg.ModelList[0].ModelName != "original-model" {
		t.Errorf("model list was mutated! first model = %q, want %q",
			loadedCfg.ModelList[0].ModelName, "original-model")
	}
}
