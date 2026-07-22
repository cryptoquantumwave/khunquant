package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

func TestHandleUpdateConfig_PreservesExecAllowRemoteDefaultWhenOmitted(t *testing.T) {
	configPath, cleanup := setupOAuthTestEnv(t)
	defer cleanup()

	h := NewHandler(configPath)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodPut, "/api/config", bytes.NewBufferString(`{
		"agents": {
			"defaults": {
				"workspace": "~/.khunquant/workspace"
			}
		},
		"model_list": [
			{
				"model_name": "custom-default",
				"model": "openai/gpt-4o",
				"api_key": "sk-default"
			}
		]
	}`))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if cfg.Tools.Exec.AllowRemote {
		t.Fatal("tools.exec.allow_remote should default to false when omitted from PUT /api/config")
	}
}

func TestHandleUpdateConfig_DoesNotInheritDefaultModelFields(t *testing.T) {
	configPath, cleanup := setupOAuthTestEnv(t)
	defer cleanup()

	h := NewHandler(configPath)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodPut, "/api/config", bytes.NewBufferString(`{
		"agents": {
			"defaults": {
				"workspace": "~/.khunquant/workspace"
			}
		},
		"model_list": [
			{
				"model_name": "custom-default",
				"model": "openai/gpt-4o",
				"api_key": "sk-default"
			}
		]
	}`))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if got := cfg.ModelList[0].APIBase; got != "" {
		t.Fatalf("model_list[0].api_base = %q, want empty string", got)
	}
}

func TestHandlePatchConfig_RejectsInvalidExecRegexPatterns(t *testing.T) {
	configPath, cleanup := setupOAuthTestEnv(t)
	defer cleanup()

	h := NewHandler(configPath)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodPatch, "/api/config", bytes.NewBufferString(`{
		"tools": {
			"exec": {
				"custom_deny_patterns": ["("]
			}
		}
	}`))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("custom_deny_patterns")) {
		t.Fatalf("expected validation error mentioning custom_deny_patterns, body=%s", rec.Body.String())
	}
}


func TestHandlePatchConfig_AllowsInvalidDenyRegexPatternsWhenDenyPatternsDisabled(t *testing.T) {
	configPath, cleanup := setupOAuthTestEnv(t)
	defer cleanup()

	h := NewHandler(configPath)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodPatch, "/api/config", bytes.NewBufferString(`{
		"tools": {
			"exec": {
				"enabled": true,
				"enable_deny_patterns": false,
				"custom_deny_patterns": ["("]
			}
		}
	}`))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
}

// testCommandPatterns is a helper that sets up a handler and sends a test-command-patterns request.

func testCommandPatterns(t *testing.T, configPath string, body string) *httptest.ResponseRecorder {
	t.Helper()
	h := NewHandler(configPath)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	req := httptest.NewRequest(http.MethodPost, "/api/config/test-command-patterns", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}


func TestHandleTestCommandPatterns_MatchesWhitelist(t *testing.T) {
	configPath, cleanup := setupOAuthTestEnv(t)
	defer cleanup()

	rec := testCommandPatterns(t, configPath, `{
		"allow_patterns": ["^echo\\s+hello"],
		"deny_patterns": ["^rm\\s+-rf"],
		"command": "echo hello world"
	}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"allowed":true`)) {
		t.Fatalf("expected allowed=true, body=%s", rec.Body.String())
	}
	if bytes.Contains(rec.Body.Bytes(), []byte(`"blocked":true`)) {
		t.Fatalf("expected blocked=false when whitelist matches, body=%s", rec.Body.String())
	}
}


func TestHandleTestCommandPatterns_MatchesBlacklistNotWhitelist(t *testing.T) {
	configPath, cleanup := setupOAuthTestEnv(t)
	defer cleanup()

	rec := testCommandPatterns(t, configPath, `{
		"allow_patterns": ["^echo\\s+hello"],
		"deny_patterns": ["^rm\\s+-rf"],
		"command": "rm -rf /tmp"
	}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"blocked":true`)) {
		t.Fatalf("expected blocked=true, body=%s", rec.Body.String())
	}
	if bytes.Contains(rec.Body.Bytes(), []byte(`"allowed":true`)) {
		t.Fatalf("expected allowed=false when blacklist matches but not whitelist, body=%s", rec.Body.String())
	}
}


func TestHandleTestCommandPatterns_MatchesNeither(t *testing.T) {
	configPath, cleanup := setupOAuthTestEnv(t)
	defer cleanup()

	rec := testCommandPatterns(t, configPath, `{
		"allow_patterns": ["^echo\\s+hello"],
		"deny_patterns": ["^rm\\s+-rf"],
		"command": "ls -la"
	}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if bytes.Contains(rec.Body.Bytes(), []byte(`"allowed":true`)) {
		t.Fatalf("expected allowed=false, body=%s", rec.Body.String())
	}
	if bytes.Contains(rec.Body.Bytes(), []byte(`"blocked":true`)) {
		t.Fatalf("expected blocked=false, body=%s", rec.Body.String())
	}
}


func TestHandleTestCommandPatterns_CaseInsensitiveWithGoFlag(t *testing.T) {
	configPath, cleanup := setupOAuthTestEnv(t)
	defer cleanup()

	rec := testCommandPatterns(t, configPath, `{
		"allow_patterns": ["(?i)^ECHO"],
		"deny_patterns": [],
		"command": "echo hello"
	}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"allowed":true`)) {
		t.Fatalf("expected allowed=true with Go (?i) flag, body=%s", rec.Body.String())
	}
}


func TestHandleTestCommandPatterns_EmptyPatterns(t *testing.T) {
	configPath, cleanup := setupOAuthTestEnv(t)
	defer cleanup()

	rec := testCommandPatterns(t, configPath, `{
		"allow_patterns": [],
		"deny_patterns": [],
		"command": "rm -rf /tmp"
	}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if bytes.Contains(rec.Body.Bytes(), []byte(`"allowed":true`)) {
		t.Fatalf("expected allowed=false with empty patterns, body=%s", rec.Body.String())
	}
	if bytes.Contains(rec.Body.Bytes(), []byte(`"blocked":true`)) {
		t.Fatalf("expected blocked=false with empty patterns, body=%s", rec.Body.String())
	}
}


func TestHandleTestCommandPatterns_InvalidRegexSkipped(t *testing.T) {
	configPath, cleanup := setupOAuthTestEnv(t)
	defer cleanup()

	rec := testCommandPatterns(t, configPath, `{
		"allow_patterns": ["([[", "^echo"],
		"deny_patterns": [],
		"command": "echo hello"
	}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"allowed":true`)) {
		t.Fatalf("expected allowed=true, invalid pattern skipped and valid one matched, body=%s", rec.Body.String())
	}
}


func TestHandleTestCommandPatterns_ReturnsMatchedPattern(t *testing.T) {
	configPath, cleanup := setupOAuthTestEnv(t)
	defer cleanup()

	rec := testCommandPatterns(t, configPath, `{
		"allow_patterns": [],
		"deny_patterns": ["\\$(?i)[a-zA-Z_]*(SECRET|KEY|PASSWORD|TOKEN|AUTH)[a-zA-Z0-9_]*"],
		"command": "echo $GITHUB_API_KEY"
	}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"blocked":true`)) {
		t.Fatalf("expected blocked=true, body=%s", rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`matched_blacklist`)) {
		t.Fatalf("expected matched_blacklist field, body=%s", rec.Body.String())
	}
}


func TestHandleTestCommandPatterns_InvalidJSON(t *testing.T) {
	configPath, cleanup := setupOAuthTestEnv(t)
	defer cleanup()

	h := NewHandler(configPath)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/config/test-command-patterns",
		bytes.NewBufferString(`{invalid json}`),
	)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

func TestHandlePatchConfig_UpdatesExchangeCredential(t *testing.T) {
	configPath, cleanup := setupOAuthTestEnv(t)
	defer cleanup()

	// Setup: save initial config with a Bitkub account.
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	cfg.Exchanges.Bitkub.Enabled = true
	cfg.Exchanges.Bitkub.Accounts = []config.ExchangeAccount{
		{Name: "main", APIKey: *config.NewSecureString("old_api_key"), Secret: *config.NewSecureString("old_secret")},
	}
	if err := config.SaveConfig(configPath, cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	secPath := filepath.Join(filepath.Dir(configPath), ".security.yml")
	initial, err := os.ReadFile(secPath)
	if err != nil {
		t.Fatalf("ReadFile .security.yml (initial): %v", err)
	}

	h := NewHandler(configPath)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	// PATCH with new api_key; secret stays "[NOT_HERE]" (unchanged).
	body := `{"exchanges":{"bitkub":{"enabled":true,"accounts":[{"name":"main","api_key":"new_api_key","secret":"[NOT_HERE]","proxy":""}]}}}`
	req := httptest.NewRequest(http.MethodPatch, "/api/config", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH /api/config: status=%d body=%s", rec.Code, rec.Body.String())
	}

	updated, err := os.ReadFile(secPath)
	if err != nil {
		t.Fatalf("ReadFile .security.yml (updated): %v", err)
	}

	if bytes.Equal(initial, updated) {
		t.Fatalf(".security.yml unchanged after credential update\ncontent: %s", updated)
	}

	newCfg, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig after PATCH: %v", err)
	}
	if got := newCfg.Exchanges.Bitkub.Accounts[0].APIKey.String(); got != "new_api_key" {
		t.Errorf("APIKey = %q, want new_api_key", got)
	}
	if got := newCfg.Exchanges.Bitkub.Accounts[0].Secret.String(); got != "old_secret" {
		t.Errorf("Secret = %q, want old_secret (should be preserved)", got)
	}
}


// TestValidateConfigWebullRegion covers the config-form side of the
// wrong-region bug: an unusable Webull region must be reported inline here
// rather than escaping to runtime, where the only symptom is an opaque 401
// from the wrong regional broker.
func TestValidateConfigWebullRegion(t *testing.T) {
	newCfg := func(region string) *config.Config {
		cfg := config.DefaultConfig()
		cfg.Exchanges.Webull.Accounts = []config.WebullExchangeAccount{{
			ExchangeAccount: config.ExchangeAccount{Name: "main"},
			Region:          region,
		}}
		return cfg
	}

	// Supported, unset, and the legacy "us" default all normalize to "th".
	for _, region := range []string{"th", "", "us"} {
		cfg := newCfg(region)
		if errs := validateConfig(cfg); len(errs) != 0 {
			t.Errorf("validateConfig(region=%q) = %v, want no errors", region, errs)
			continue
		}
		if got := cfg.Exchanges.Webull.Accounts[0].Region; got != "th" {
			t.Errorf("validateConfig(region=%q) left region %q, want normalized to th", region, got)
		}
	}

	// A region that exists but isn't wired up, and an outright typo, both fail.
	for _, region := range []string{"hk", "thailand"} {
		cfg := newCfg(region)
		errs := validateConfig(cfg)
		if len(errs) == 0 {
			t.Errorf("validateConfig(region=%q) = no errors, want a rejection", region)
			continue
		}
		if !strings.Contains(errs[0], "exchanges.webull.accounts[main]") {
			t.Errorf("validateConfig(region=%q) error %q does not name the account", region, errs[0])
		}
	}
}
