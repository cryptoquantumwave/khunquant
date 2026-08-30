package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

// ─── pure helpers ────────────────────────────────────────────────────────────

func TestModelBelongsToProvider(t *testing.T) {
	cases := []struct {
		provider string
		model    string
		want     bool
	}{
		{oauthProviderOpenAI, "openai/gpt-5.5", true},
		{oauthProviderOpenAI, "openai/gpt-5.4", true},
		{oauthProviderOpenAI, "openai", true},
		{oauthProviderOpenAI, "OPENAI/GPT-5.5", true},
		{oauthProviderOpenAI, "anthropic/claude-sonnet-4.6", false},
		{oauthProviderAnthropic, "anthropic/claude-sonnet-4.6", true},
		{oauthProviderAnthropic, "anthropic", true},
		{oauthProviderAnthropic, "openai/gpt-5.5", false},
		{oauthProviderGoogleAntigravity, "antigravity/gemini-3.5-flash", true},
		{oauthProviderGoogleAntigravity, "antigravity", true},
		{oauthProviderGoogleAntigravity, "google-antigravity/model", true},
		{oauthProviderGoogleAntigravity, "openai/gpt-5.5", false},
		{oauthProviderGoogleGemini, "gemini-code-assist/gemini-2.5-flash", true},
		{oauthProviderGoogleGemini, "google-gemini/model", true},
		{oauthProviderGoogleGemini, "antigravity/gemini-3.5-flash", false},
		{"unknown", "openai/gpt-5.5", false},
	}
	for _, tc := range cases {
		t.Run(tc.provider+":"+tc.model, func(t *testing.T) {
			got := modelBelongsToProvider(tc.provider, tc.model)
			if got != tc.want {
				t.Errorf("modelBelongsToProvider(%q, %q) = %v, want %v", tc.provider, tc.model, got, tc.want)
			}
		})
	}
}

func TestModelNameFromPreset(t *testing.T) {
	cases := []struct {
		provider string
		modelID  string
		want     string
	}{
		// Known preset → label slug
		{oauthProviderOpenAI, "openai/gpt-5.5", "gpt-5.5"},
		{oauthProviderOpenAI, "openai/gpt-5.4-mini", "gpt-5.4-mini"},
		{oauthProviderAnthropic, "anthropic/claude-fable-5", "claude-fable-5"},
		{oauthProviderAnthropic, "anthropic/claude-sonnet-4.6", "claude-sonnet-4.6"},
		{oauthProviderGoogleAntigravity, "antigravity/gemini-3.5-flash", "gemini-3.5-flash"},
		// Not in presets → suffix after last /
		{oauthProviderOpenAI, "openai/future-model", "future-model"},
		// No / → returned as-is
		{oauthProviderOpenAI, "rawmodel", "rawmodel"},
	}
	for _, tc := range cases {
		t.Run(tc.modelID, func(t *testing.T) {
			got := modelNameFromPreset(tc.provider, tc.modelID)
			if got != tc.want {
				t.Errorf("modelNameFromPreset(%q, %q) = %q, want %q", tc.provider, tc.modelID, got, tc.want)
			}
		})
	}
}

func TestDefaultModelConfigForProvider(t *testing.T) {
	cases := []struct {
		provider   string
		authMethod string
		wantModel  string
		wantName   string
	}{
		{oauthProviderOpenAI, "oauth", "openai/gpt-5.6-sol", "gpt-5.6-sol"},
		{oauthProviderAnthropic, "token", "anthropic/claude-sonnet-5", "claude-sonnet-5"},
		{oauthProviderGoogleAntigravity, "oauth", "antigravity/gemini-3-flash", "gemini-3-flash"},
	}
	for _, tc := range cases {
		t.Run(tc.provider, func(t *testing.T) {
			mc := defaultModelConfigForProvider(tc.provider, tc.authMethod)
			if mc.Model != tc.wantModel {
				t.Errorf("Model = %q, want %q", mc.Model, tc.wantModel)
			}
			if mc.ModelName != tc.wantName {
				t.Errorf("ModelName = %q, want %q", mc.ModelName, tc.wantName)
			}
			if mc.AuthMethod != tc.authMethod {
				t.Errorf("AuthMethod = %q, want %q", mc.AuthMethod, tc.authMethod)
			}
		})
	}

	t.Run("unknown provider returns empty config", func(t *testing.T) {
		mc := defaultModelConfigForProvider("unknown", "oauth")
		if mc.Model != "" || mc.ModelName != "" {
			t.Errorf("expected empty ModelConfig, got %+v", mc)
		}
	})
}

// ─── handleSelectProviderModel ────────────────────────────────────────────────

func TestSelectProviderModelBadProvider(t *testing.T) {
	configPath, cleanup := setupOAuthTestEnv(t)
	defer cleanup()
	resetOAuthHooks(t)

	h := NewHandler(configPath)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/oauth/providers/noexist/select-model",
		strings.NewReader(`{"model_id":"openai/gpt-5.5"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

func TestSelectProviderModelEmptyModelID(t *testing.T) {
	configPath, cleanup := setupOAuthTestEnv(t)
	defer cleanup()
	resetOAuthHooks(t)

	h := NewHandler(configPath)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/oauth/providers/openai/select-model",
		strings.NewReader(`{"model_id":""}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

func TestSelectProviderModelWrongProvider(t *testing.T) {
	configPath, cleanup := setupOAuthTestEnv(t)
	defer cleanup()
	resetOAuthHooks(t)

	h := NewHandler(configPath)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	// anthropic model sent to openai endpoint
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/oauth/providers/openai/select-model",
		strings.NewReader(`{"model_id":"anthropic/claude-sonnet-4.6"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

// TestSelectProviderModelUpdatesExistingEntry verifies the "found" branch:
// when the provider already has a ModelList entry, its .Model is replaced and
// Agents.Defaults.ModelName is updated to the entry's ModelName.
func TestSelectProviderModelUpdatesExistingEntry(t *testing.T) {
	configPath, cleanup := setupOAuthTestEnv(t)
	defer cleanup()
	resetOAuthHooks(t)

	// Replace the ModelList entirely with a single controlled openai entry so we know
	// exactly which entry the handler will find and update.
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	cfg.ModelList = []config.ModelConfig{
		{ModelName: "gpt-5.5", Model: "openai/gpt-5.5", AuthMethod: "oauth"},
	}
	cfg.Agents.Defaults.ModelName = "gpt-5.5"
	if err := config.SaveConfig(configPath, cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	h := NewHandler(configPath)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/oauth/providers/openai/select-model",
		strings.NewReader(`{"model_id":"openai/gpt-5.4"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp["status"] != "ok" {
		t.Errorf("status = %v, want ok", resp["status"])
	}
	if resp["model_id"] != "openai/gpt-5.4" {
		t.Errorf("model_id = %v, want openai/gpt-5.4", resp["model_id"])
	}

	// Verify config was persisted correctly.
	updated, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig after select: %v", err)
	}
	found := false
	for _, mc := range updated.ModelList {
		if modelBelongsToProvider(oauthProviderOpenAI, mc.Model) {
			if mc.Model != "openai/gpt-5.4" {
				t.Errorf("ModelList Model = %q, want openai/gpt-5.4", mc.Model)
			}
			// ModelName must be updated to the new model's slug so the display is correct.
			if mc.ModelName != "gpt-5.4" {
				t.Errorf("ModelList ModelName = %q, want gpt-5.4 (updated to new model)", mc.ModelName)
			}
			found = true
		}
	}
	if !found {
		t.Error("no openai entry found in ModelList after select")
	}
	// Agents.Defaults.ModelName must be updated to the new model's name.
	if updated.Agents.Defaults.ModelName != "gpt-5.4" {
		t.Errorf("Agents.Defaults.ModelName = %q, want gpt-5.4", updated.Agents.Defaults.ModelName)
	}
}

// TestSelectProviderModelAppendsNewEntry verifies the "not found" branch:
// when there is no existing ModelList entry for the provider, a new one is appended.
func TestSelectProviderModelAppendsNewEntry(t *testing.T) {
	configPath, cleanup := setupOAuthTestEnv(t)
	defer cleanup()
	resetOAuthHooks(t)

	// Config from setupOAuthTestEnv has only "custom-default" / openai/gpt-4o (no openai/ model).
	// We clear ModelList to be sure there's no openai entry.
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	cfg.ModelList = []config.ModelConfig{{
		ModelName: "custom-default",
		Model:     "anthropic/claude-sonnet-4.6", // not openai
	}}
	if err := config.SaveConfig(configPath, cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	h := NewHandler(configPath)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/oauth/providers/openai/select-model",
		strings.NewReader(`{"model_id":"openai/gpt-5.5"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	updated, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig after select: %v", err)
	}
	found := false
	for _, mc := range updated.ModelList {
		if mc.Model == "openai/gpt-5.5" {
			found = true
			if mc.ModelName != "gpt-5.5" {
				t.Errorf("appended ModelName = %q, want gpt-5.5", mc.ModelName)
			}
		}
	}
	if !found {
		t.Error("openai/gpt-5.5 not appended to ModelList")
	}
	if updated.Agents.Defaults.ModelName != "gpt-5.5" {
		t.Errorf("Agents.Defaults.ModelName = %q, want gpt-5.5", updated.Agents.Defaults.ModelName)
	}
}

// ─── handleListOAuthProviders ─────────────────────────────────────────────────

func TestListOAuthProvidersIncludesModelPresets(t *testing.T) {
	configPath, cleanup := setupOAuthTestEnv(t)
	defer cleanup()
	resetOAuthHooks(t)

	h := NewHandler(configPath)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/oauth/providers", nil)
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp struct {
		Providers []oauthProviderStatus `json:"providers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	for _, item := range resp.Providers {
		if item.Provider == oauthProviderGoogleGemini {
			// google-gemini has no entry in oauthProviderModelPresets — presets should be nil/empty.
			continue
		}
		if len(item.ModelPresets) == 0 {
			t.Errorf("provider %q: ModelPresets is empty, expected presets to be populated", item.Provider)
		}
	}
}

func TestListOAuthProvidersActiveModelFromConfig(t *testing.T) {
	configPath, cleanup := setupOAuthTestEnv(t)
	defer cleanup()
	resetOAuthHooks(t)

	// Seed config with an antigravity model entry.
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	cfg.ModelList = append(cfg.ModelList, config.ModelConfig{
		ModelName: "gemini-3.5-flash",
		Model:     "antigravity/gemini-3.5-flash",
	})
	if err := config.SaveConfig(configPath, cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	h := NewHandler(configPath)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/oauth/providers", nil)
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp struct {
		Providers []oauthProviderStatus `json:"providers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	for _, item := range resp.Providers {
		if item.Provider == oauthProviderGoogleAntigravity {
			if item.ActiveModel != "antigravity/gemini-3.5-flash" {
				t.Errorf("ActiveModel = %q, want antigravity/gemini-3.5-flash", item.ActiveModel)
			}
			return
		}
	}
	t.Error("google-antigravity not found in providers list")
}
