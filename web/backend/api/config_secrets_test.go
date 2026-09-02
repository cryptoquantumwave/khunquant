package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

// secretProbes are the credential values planted in the test config. Each is
// unique so a leak names the exact field that leaked it.
var secretProbes = map[string]string{
	"devmcp":     "PROBE-devmcp-token",
	"sandbox":    "PROBE-sandbox-token",
	"brave":      "PROBE-brave-key",
	"braveList":  "PROBE-brave-key-in-list",
	"tavily":     "PROBE-tavily-key",
	"perplexity": "PROBE-perplexity-key",
	"provider":   "PROBE-provider-key",
	"channel":    "PROBE-telegram-token",
	"model":      "PROBE-model-key",
}

// writeConfigWithSecrets plants a known credential in every field that reaches
// GET /api/config, across both storage styles: SecureString fields (which
// withhold themselves) and the plain strings in the yaml:"-" subtrees.
func writeConfigWithSecrets(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := config.DefaultConfig()

	cfg.Channels.Telegram.Token.Set(secretProbes["channel"])
	cfg.Debug.DevMCP.Token = secretProbes["devmcp"]
	cfg.Debug.Sandbox.Token = secretProbes["sandbox"]
	cfg.Tools.Web.Brave.APIKey = secretProbes["brave"]
	cfg.Tools.Web.Brave.APIKeys = []string{secretProbes["braveList"]}
	cfg.Tools.Web.Tavily.APIKey = secretProbes["tavily"]
	cfg.Tools.Web.Perplexity.APIKey = secretProbes["perplexity"]
	cfg.Providers.Anthropic.APIKey = secretProbes["provider"]
	if len(cfg.ModelList) > 0 {
		cfg.ModelList[0].APIKey = *config.NewSecureString(secretProbes["model"])
	}

	if err := config.SaveConfig(path, cfg); err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}
	return path
}

func getConfigBody(t *testing.T, h *Handler) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.handleGetConfig(rec, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/config status = %d, want 200", rec.Code)
	}
	return rec.Body.String()
}

// TestHandleGetConfig_WithholdsEveryCredential is the regression test for the
// leak: provider and tool API keys, and the dev-MCP and sandbox tokens, were
// served verbatim because those subtrees are yaml:"-" and so cannot use
// SecureString.
func TestHandleGetConfig_WithholdsEveryCredential(t *testing.T) {
	path := writeConfigWithSecrets(t)
	body := getConfigBody(t, NewHandler(path))

	for name, secret := range secretProbes {
		if strings.Contains(body, secret) {
			t.Errorf("GET /api/config leaked the %s credential (%q)", name, secret)
		}
	}
}

// TestHandleGetConfig_RemainsUsable guards the other direction: a response that
// redacts everything, including structure the dashboard needs, is not a fix.
func TestHandleGetConfig_RemainsUsable(t *testing.T) {
	path := writeConfigWithSecrets(t)
	body := getConfigBody(t, NewHandler(path))

	var tree map[string]any
	if err := json.Unmarshal([]byte(body), &tree); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if _, ok := tree["channels"]; !ok {
		t.Error("response lost the channels section")
	}
	if _, ok := tree["providers"]; !ok {
		t.Error("response lost the providers section")
	}
}

// TestRedactSecrets_KeepsUnsetCredentialsDistinguishable asserts an unset
// credential is not disguised as a set one, so the dashboard can still tell
// "not configured" from "configured but withheld".
func TestRedactSecrets_KeepsUnsetCredentialsDistinguishable(t *testing.T) {
	tree := map[string]any{
		"set":    map[string]any{"api_key": "actual-value"},
		"unset":  map[string]any{"api_key": ""},
		"absent": map[string]any{"api_keys": []any{}},
	}
	redactSecrets(tree)

	if got := tree["set"].(map[string]any)["api_key"]; got != redactedSentinel {
		t.Errorf("set api_key = %v, want %s", got, redactedSentinel)
	}
	if got := tree["unset"].(map[string]any)["api_key"]; got != "" {
		t.Errorf("unset api_key = %v, want empty string", got)
	}
	if got := tree["absent"].(map[string]any)["api_keys"]; len(got.([]any)) != 0 {
		t.Errorf("empty api_keys = %v, want left alone", got)
	}
}

// TestRedactSecrets_MatchesFieldNamesExactly guards against a substring match
// swallowing ordinary settings such as max_tokens.
func TestRedactSecrets_MatchesFieldNamesExactly(t *testing.T) {
	tree := map[string]any{
		"max_tokens":     float64(4096),
		"token_budget":   float64(10),
		"secret_sharing": "not-a-credential",
		"token":          "a-real-credential",
	}
	redactSecrets(tree)

	if tree["max_tokens"] != float64(4096) {
		t.Errorf("max_tokens was redacted: %v", tree["max_tokens"])
	}
	if tree["token_budget"] != float64(10) {
		t.Errorf("token_budget was redacted: %v", tree["token_budget"])
	}
	if tree["secret_sharing"] != "not-a-credential" {
		t.Errorf("secret_sharing was redacted: %v", tree["secret_sharing"])
	}
	if tree["token"] != redactedSentinel {
		t.Errorf("token = %v, want redacted", tree["token"])
	}
}

// TestHandleUpdateConfig_PreservesWithheldCredentials is the destructive case:
// the dashboard reads a redacted config, changes one unrelated field, and PUTs
// the whole document back. Without restoration every credential in the
// yaml:"-" subtrees would be overwritten with the sentinel string.
func TestHandleUpdateConfig_PreservesWithheldCredentials(t *testing.T) {
	path := writeConfigWithSecrets(t)
	h := NewHandler(path)

	body := getConfigBody(t, h)
	var doc map[string]any
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("unmarshal GET body: %v", err)
	}
	// Confirm the round-trip actually carries sentinels, or this test proves nothing.
	providers := doc["providers"].(map[string]any)["anthropic"].(map[string]any)
	if providers["api_key"] != redactedSentinel {
		t.Fatalf("precondition failed: provider api_key = %v, want sentinel", providers["api_key"])
	}

	payload, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	req := httptest.NewRequest(http.MethodPut, "/api/config", bytes.NewReader(payload))
	req.Header.Set("Origin", "http://launcher.local")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Host = "launcher.local"
	rec := httptest.NewRecorder()
	h.handleUpdateConfig(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	saved, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig() after save: %v", err)
	}
	checks := map[string]string{
		"providers.anthropic.api_key": saved.Providers.Anthropic.APIKey,
		"tools.web.brave.api_key":     saved.Tools.Web.Brave.APIKey,
		"tools.web.tavily.api_key":    saved.Tools.Web.Tavily.APIKey,
		"debug.dev_mcp.token":         saved.Debug.DevMCP.Token,
		"debug.sandbox.token":         saved.Debug.Sandbox.Token,
	}
	want := map[string]string{
		"providers.anthropic.api_key": secretProbes["provider"],
		"tools.web.brave.api_key":     secretProbes["brave"],
		"tools.web.tavily.api_key":    secretProbes["tavily"],
		"debug.dev_mcp.token":         secretProbes["devmcp"],
		"debug.sandbox.token":         secretProbes["sandbox"],
	}
	for field, got := range checks {
		if got == redactedSentinel {
			t.Errorf("%s was overwritten with the redaction sentinel", field)
			continue
		}
		if got != want[field] {
			t.Errorf("%s = %q, want %q", field, got, want[field])
		}
	}
}

// TestHandlePatchConfig_PreservesWithheldCredentials covers the same hazard on
// the PATCH path, where the sentinel arrives inside a partial document.
func TestHandlePatchConfig_PreservesWithheldCredentials(t *testing.T) {
	path := writeConfigWithSecrets(t)
	h := NewHandler(path)

	patch := map[string]any{
		"providers": map[string]any{
			"anthropic": map[string]any{
				"api_key":  redactedSentinel,
				"api_base": "https://example.invalid",
			},
		},
	}
	payload, err := json.Marshal(patch)
	if err != nil {
		t.Fatalf("marshal patch: %v", err)
	}
	req := httptest.NewRequest(http.MethodPatch, "/api/config", bytes.NewReader(payload))
	req.Header.Set("Origin", "http://launcher.local")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Host = "launcher.local"
	rec := httptest.NewRecorder()
	h.handlePatchConfig(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	saved, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig() after patch: %v", err)
	}
	if saved.Providers.Anthropic.APIKey != secretProbes["provider"] {
		t.Errorf("api_key = %q, want the original %q",
			saved.Providers.Anthropic.APIKey, secretProbes["provider"])
	}
	if saved.Providers.Anthropic.APIBase != "https://example.invalid" {
		t.Errorf("api_base = %q, want the patched value", saved.Providers.Anthropic.APIBase)
	}
}

// TestRedactSecrets_PreservesListType asserts a list-valued credential stays a
// list. Collapsing api_keys to a scalar changes the field's JSON type, which
// makes the document fail to decode on the way back through PUT /api/config.
func TestRedactSecrets_PreservesListType(t *testing.T) {
	tree := map[string]any{
		"tool": map[string]any{"api_keys": []any{"one", "two"}},
	}
	redactSecrets(tree)

	got, ok := tree["tool"].(map[string]any)["api_keys"].([]any)
	if !ok {
		t.Fatalf("api_keys became %T, want []any", tree["tool"].(map[string]any)["api_keys"])
	}
	if len(got) != 2 {
		t.Errorf("api_keys length = %d, want 2", len(got))
	}
	for i, item := range got {
		if item != redactedSentinel {
			t.Errorf("api_keys[%d] = %v, want %s", i, item, redactedSentinel)
		}
	}
}
