package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/auth"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

func resetModelProbeHooks(t *testing.T) {
	t.Helper()

	origTCPProbe := probeTCPServiceFunc
	origOllamaProbe := probeOllamaModelFunc
	origOpenAIProbe := probeOpenAICompatibleModelFunc
	origLlamaCppProbe := probeLlamaCppHealthFunc
	origMLXLMProbe := probeMLXLMHealthFunc
	origNow := modelProbeNowFunc
	resetModelProbeCache()
	t.Cleanup(func() {
		probeTCPServiceFunc = origTCPProbe
		probeOllamaModelFunc = origOllamaProbe
		probeOpenAICompatibleModelFunc = origOpenAIProbe
		probeLlamaCppHealthFunc = origLlamaCppProbe
		probeMLXLMHealthFunc = origMLXLMProbe
		modelProbeNowFunc = origNow
		resetModelProbeCache()
	})
}

func TestHandleListModels_ConfiguredStatusUsesRuntimeProbesForLocalModels(t *testing.T) {
	configPath, cleanup := setupOAuthTestEnv(t)
	defer cleanup()
	resetOAuthHooks(t)
	resetModelProbeHooks(t)

	var mu sync.Mutex
	var openAIProbes []string
	var ollamaProbes []string
	var tcpProbes []string

	probeOpenAICompatibleModelFunc = func(apiBase, modelID, apiKey string) bool {
		mu.Lock()
		openAIProbes = append(openAIProbes, apiBase+"|"+modelID+"|"+apiKey)
		mu.Unlock()
		return apiBase == "http://127.0.0.1:8000/v1" && modelID == "custom-model" && apiKey == ""
	}
	probeOllamaModelFunc = func(apiBase, modelID string) bool {
		mu.Lock()
		ollamaProbes = append(ollamaProbes, apiBase+"|"+modelID)
		mu.Unlock()
		return apiBase == "http://localhost:11434/v1" && modelID == "llama3"
	}
	probeTCPServiceFunc = func(apiBase string) bool {
		mu.Lock()
		tcpProbes = append(tcpProbes, apiBase)
		mu.Unlock()
		return apiBase == "http://127.0.0.1:4321"
	}

	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	cfg.ModelList = []config.ModelConfig{
		{
			ModelName:  "openai-oauth",
			Model:      "openai/gpt-5.4",
			AuthMethod: "oauth",
		},
		{
			ModelName: "vllm-local",
			Model:     "vllm/custom-model",
			APIBase:   "http://127.0.0.1:8000/v1",
		},
		{
			ModelName: "ollama-default",
			Model:     "ollama/llama3",
		},
		{
			ModelName: "vllm-remote",
			Model:     "vllm/custom-model",
			APIBase:   "https://models.example.com/v1",
			APIKey:    *config.NewSecureString("remote-key"),
		},
		{
			ModelName:  "copilot-gpt-5.4",
			Model:      "github-copilot/gpt-5.4",
			APIBase:    "http://127.0.0.1:4321",
			AuthMethod: "oauth",
		},
	}
	cfg.Agents.Defaults.ModelName = "openai-oauth"
	if err := config.SaveConfig(configPath, cfg); err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}

	h := NewHandler(configPath)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/models", nil)
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp struct {
		Models []modelResponse `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	got := make(map[string]bool, len(resp.Models))
	for _, model := range resp.Models {
		got[model.ModelName] = model.Configured
	}

	if got["openai-oauth"] {
		t.Fatalf("openai oauth model configured = true, want false without stored credential")
	}
	if !got["vllm-local"] {
		t.Fatalf("vllm local model configured = false, want true when local probe succeeds")
	}
	if !got["ollama-default"] {
		t.Fatalf("ollama default model configured = false, want true when default local probe succeeds")
	}
	if !got["vllm-remote"] {
		t.Fatalf("remote vllm model configured = false, want true with api_key")
	}
	if !got["copilot-gpt-5.4"] {
		t.Fatalf("copilot model configured = false, want true when local bridge probe succeeds")
	}
	if len(openAIProbes) != 1 || openAIProbes[0] != "http://127.0.0.1:8000/v1|custom-model|" {
		t.Fatalf("openAI probes = %#v, want only local vllm probe", openAIProbes)
	}
	if len(ollamaProbes) != 1 || ollamaProbes[0] != "http://localhost:11434/v1|llama3" {
		t.Fatalf("ollama probes = %#v, want default local probe", ollamaProbes)
	}
	if len(tcpProbes) != 1 || tcpProbes[0] != "http://127.0.0.1:4321" {
		t.Fatalf("tcp probes = %#v, want only local copilot probe", tcpProbes)
	}
}

func TestHandleListModels_ConfiguredStatusForOAuthModelWithCredential(t *testing.T) {
	configPath, cleanup := setupOAuthTestEnv(t)
	defer cleanup()
	resetOAuthHooks(t)
	resetModelProbeHooks(t)

	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	cfg.ModelList = []config.ModelConfig{{
		ModelName:  "claude-oauth",
		Model:      "anthropic/claude-sonnet-4.6",
		AuthMethod: "oauth",
	}}
	cfg.Agents.Defaults.ModelName = "claude-oauth"
	if err := config.SaveConfig(configPath, cfg); err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}

	if err := auth.SetCredential(oauthProviderAnthropic, &auth.AuthCredential{
		AccessToken: "anthropic-token",
		Provider:    oauthProviderAnthropic,
		AuthMethod:  "oauth",
	}); err != nil {
		t.Fatalf("SetCredential() error = %v", err)
	}

	h := NewHandler(configPath)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/models", nil)
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp struct {
		Models []modelResponse `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if len(resp.Models) != 1 {
		t.Fatalf("len(models) = %d, want 1", len(resp.Models))
	}
	if !resp.Models[0].Configured {
		t.Fatalf("oauth model configured = false, want true with stored credential")
	}
}

func TestHandleListModels_ProbesLocalModelsConcurrently(t *testing.T) {
	configPath, cleanup := setupOAuthTestEnv(t)
	defer cleanup()
	resetOAuthHooks(t)
	resetModelProbeHooks(t)

	started := make(chan string, 2)
	release := make(chan struct{})

	probeOpenAICompatibleModelFunc = func(apiBase, modelID, apiKey string) bool {
		started <- apiBase + "|" + modelID
		<-release
		return true
	}

	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	cfg.ModelList = []config.ModelConfig{
		{
			ModelName: "local-vllm-a",
			Model:     "vllm/custom-a",
			APIBase:   "http://127.0.0.1:8000/v1",
		},
		{
			ModelName: "local-vllm-b",
			Model:     "vllm/custom-b",
			APIBase:   "http://127.0.0.1:8001/v1",
		},
	}
	if err := config.SaveConfig(configPath, cfg); err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}

	h := NewHandler(configPath)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	recCh := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/models", nil)
		mux.ServeHTTP(rec, req)
		recCh <- rec
	}()

	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(200 * time.Millisecond):
			t.Fatal("expected both local probes to start before the first one completed")
		}
	}
	close(release)

	rec := <-recCh
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
}

func TestHandleListModels_NormalizesWildcardLocalAPIBaseForProbe(t *testing.T) {
	configPath, cleanup := setupOAuthTestEnv(t)
	defer cleanup()
	resetOAuthHooks(t)
	resetModelProbeHooks(t)

	var gotProbe string
	probeOpenAICompatibleModelFunc = func(apiBase, modelID, apiKey string) bool {
		gotProbe = apiBase + "|" + modelID + "|" + apiKey
		return apiBase == "http://127.0.0.1:8000/v1" && modelID == "custom-model" && apiKey == ""
	}

	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	cfg.ModelList = []config.ModelConfig{{
		ModelName: "vllm-local",
		Model:     "vllm/custom-model",
		APIBase:   "http://0.0.0.0:8000/v1",
	}}
	if err := config.SaveConfig(configPath, cfg); err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}

	h := NewHandler(configPath)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/models", nil)
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp struct {
		Models []modelResponse `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if len(resp.Models) != 1 {
		t.Fatalf("len(models) = %d, want 1", len(resp.Models))
	}
	if !resp.Models[0].Configured {
		t.Fatal("wildcard-bound local model configured = false, want true after probe host normalization")
	}
	if gotProbe != "http://127.0.0.1:8000/v1|custom-model|" {
		t.Fatalf("probe api base = %q, want %q", gotProbe, "http://127.0.0.1:8000/v1|custom-model|")
	}
}

func TestMaskAPIKey(t *testing.T) {
	tests := []struct {
		name string
		key  string
		want string
	}{
		{
			name: "empty key",
			key:  "",
			want: "",
		},
		{
			name: "short key fully masked",
			key:  "abcd",
			want: "****",
		},
		{
			name: "length 8 boundary fully masked",
			key:  "12345678",
			want: "****",
		},
		{
			name: "length 9 boundary shows last 2",
			key:  "123456789",
			want: "123****89",
		},
		{
			name: "length 12 boundary shows last 2",
			key:  "abcdefghijkl",
			want: "abc****kl",
		},
		{
			name: "length 13 boundary shows last 4",
			key:  "abcdefghijklm",
			want: "abc****jklm",
		},
		{
			name: "typical api key",
			key:  "sk-1234567890abcd",
			want: "sk-****abcd",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := maskAPIKey(tc.key)
			if got != tc.want {
				t.Fatalf("maskAPIKey(%q) = %q, want %q", tc.key, got, tc.want)
			}

			if tc.key != "" {
				displayed := strings.Replace(tc.want, "****", "", 1)
				if len(tc.key) <= 8 {
					if displayed != "" {
						t.Fatalf("maskAPIKey(%q) displayed part = %q, want empty", tc.key, displayed)
					}
				} else {
					if len(displayed)*10 > len(tc.key)*6 {
						t.Fatalf(
							"maskAPIKey(%q) displayed length = %d, want at most 60%% of %d",
							tc.key,
							len(displayed),
							len(tc.key),
						)
					}
				}
			}
		})
	}
}

func TestHandleUpdateModel_UpdatesAPIKey(t *testing.T) {
	configPath, cleanup := setupOAuthTestEnv(t)
	defer cleanup()

	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	cfg.ModelList = []config.ModelConfig{{
		ModelName: "gpt-4",
		Model:     "openai/gpt-4o",
		APIKey:    *config.NewSecureString("old_api_key"),
	}}
	cfg.Agents.Defaults.ModelName = "gpt-4"
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

	body := `{"model_name":"gpt-4","model":"openai/gpt-4o","api_key":"new_api_key"}`
	req := httptest.NewRequest(http.MethodPut, "/api/models/0", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("PUT /api/models/0: status=%d body=%s", rec.Code, rec.Body.String())
	}

	updated, err := os.ReadFile(secPath)
	if err != nil {
		t.Fatalf("ReadFile .security.yml (updated): %v", err)
	}

	if bytes.Equal(initial, updated) {
		t.Fatalf(".security.yml unchanged after model API key update\ncontent: %s", updated)
	}

	newCfg, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig after PUT: %v", err)
	}
	if got := newCfg.ModelList[0].APIKey.String(); got != "new_api_key" {
		t.Errorf("APIKey = %q, want new_api_key", got)
	}
}

func TestFetchOpenAICompatibleModels_ResponseShapes(t *testing.T) {
	tests := []struct {
		name      string
		response  string
		apiKey    string
		wantLen   int
		wantFirst struct {
			id, ownedBy string
		}
		wantSecond struct {
			id, ownedBy string
		}
	}{
		{
			name:     "envelope shape",
			response: `{"data":[{"id":"gpt-4o","owned_by":"openai"},{"id":"gpt-4o-mini","owned_by":"openai"}]}`,
			apiKey:   "test-key",
			wantLen:  2,
			wantFirst: struct {
				id, ownedBy string
			}{id: "gpt-4o", ownedBy: "openai"},
			wantSecond: struct {
				id, ownedBy string
			}{id: "gpt-4o-mini", ownedBy: "openai"},
		},
		{
			name:     "bare array shape",
			response: `[{"id":"qwen-max","owned_by":"qwen"},{"id":"qwen-plus","owned_by":"qwen"}]`,
			apiKey:   "",
			wantLen:  2,
			wantFirst: struct {
				id, ownedBy string
			}{id: "qwen-max", ownedBy: "qwen"},
			wantSecond: struct {
				id, ownedBy string
			}{id: "qwen-plus", ownedBy: "qwen"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, tt.response)
			}))
			defer srv.Close()

			models, err := fetchOpenAICompatibleModels(t.Context(), srv.URL+"/models", tt.apiKey)
			if err != nil {
				t.Fatalf("error = %v", err)
			}
			if len(models) != tt.wantLen {
				t.Fatalf("len(models) = %d, want %d", len(models), tt.wantLen)
			}
			if models[0].ID != tt.wantFirst.id || models[0].OwnedBy != tt.wantFirst.ownedBy {
				t.Fatalf("models[0] = %+v, want {ID:%s OwnedBy:%s}", models[0], tt.wantFirst.id, tt.wantFirst.ownedBy)
			}
			if models[1].ID != tt.wantSecond.id || models[1].OwnedBy != tt.wantSecond.ownedBy {
				t.Fatalf("models[1] = %+v, want {ID:%s OwnedBy:%s}", models[1], tt.wantSecond.id, tt.wantSecond.ownedBy)
			}
		})
	}
}

func TestFetchOpenAICompatibleModels_EmptyEnvelopeReturnsEmptySlice(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[]}`)
	}))
	defer srv.Close()

	models, err := fetchOpenAICompatibleModels(t.Context(), srv.URL+"/models", "k")
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if len(models) != 0 {
		t.Fatalf("len(models) = %d, want 0", len(models))
	}
}

func TestFetchOpenAICompatibleModels_EmptyBareArrayReturnsEmptySlice(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[]`)
	}))
	defer srv.Close()

	models, err := fetchOpenAICompatibleModels(t.Context(), srv.URL+"/models", "k")
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if len(models) != 0 {
		t.Fatalf("len(models) = %d, want 0", len(models))
	}
}

func TestFetchOpenAICompatibleModels_UnrecognizedShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"models":[],"error":"unsupported"}`)
	}))
	defer srv.Close()

	_, err := fetchOpenAICompatibleModels(t.Context(), srv.URL+"/models", "k")
	if err == nil {
		t.Fatal("error = nil, want unrecognized shape error")
	}
	if !strings.Contains(err.Error(), "unrecognized shape") {
		t.Fatalf("error = %q, want it to contain 'unrecognized shape'", err.Error())
	}
}

func TestFetchOpenAICompatibleModels_FiltersEmptyIDs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[`+
			`{"id":"gpt-4o","owned_by":"openai"},`+
			`{"id":"","owned_by":"openai"},`+
			`{"id":"gpt-4o-mini"}]}`)
	}))
	defer srv.Close()

	models, err := fetchOpenAICompatibleModels(t.Context(), srv.URL+"/models", "k")
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("len(models) = %d, want 2 (empty IDs should be filtered)", len(models))
	}
	if models[0].ID != "gpt-4o" {
		t.Fatalf("models[0].ID = %q, want %q", models[0].ID, "gpt-4o")
	}
	if models[1].ID != "gpt-4o-mini" {
		t.Fatalf("models[1].ID = %q, want %q", models[1].ID, "gpt-4o-mini")
	}
}

func TestFetchOpenAICompatibleModels_SetsAuthorizationHeader(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"id":"m1"}]}`)
	}))
	defer srv.Close()

	if _, err := fetchOpenAICompatibleModels(t.Context(), srv.URL+"/models", "my-secret-key"); err != nil {
		t.Fatalf("error = %v", err)
	}
	if gotAuth != "Bearer my-secret-key" {
		t.Fatalf("Authorization = %q, want %q", gotAuth, "Bearer my-secret-key")
	}
}

func TestFetchOpenAICompatibleModels_NoAuthHeaderWhenKeyEmpty(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[{"id":"m1"}]`)
	}))
	defer srv.Close()

	if _, err := fetchOpenAICompatibleModels(t.Context(), srv.URL+"/models", ""); err != nil {
		t.Fatalf("error = %v", err)
	}
	if gotAuth != "" {
		t.Fatalf("Authorization = %q, want empty", gotAuth)
	}
}

func TestHandleFetchModels_ModelIndexUsesStoredKey(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"id":"gpt-4o","owned_by":"openai"}]}`)
	}))
	defer srv.Close()

	configPath, cleanup := setupOAuthTestEnv(t)
	defer cleanup()

	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	cfg.ModelList = []config.ModelConfig{
		{
			ModelName: "my-openai",
			Model:     "openai/gpt-4o",
			APIBase:   srv.URL,
			APIKey:    *config.NewSecureString("sk-stored-secret"),
		},
	}
	if err := config.SaveConfig(configPath, cfg); err != nil {
		t.Fatalf("SaveConfig error: %v", err)
	}

	h := NewHandler(configPath)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	idx := 0
	body := fmt.Sprintf(`{"provider":"openai","api_base":"%s","model_index":%d}`, srv.URL, idx)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/models/fetch", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if gotAuth != "Bearer sk-stored-secret" {
		t.Fatalf("Authorization = %q, want stored key to be used", gotAuth)
	}

	var resp struct {
		Models []upstreamModel `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}
	if len(resp.Models) != 1 || resp.Models[0].ID != "gpt-4o" {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

func TestHandleFetchModels_ModelIndexProviderMismatchRejectsKey(t *testing.T) {
	requestReceivedWithAuth := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			requestReceivedWithAuth = true
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[]}`)
	}))
	defer srv.Close()

	configPath, cleanup := setupOAuthTestEnv(t)
	defer cleanup()

	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	cfg.ModelList = []config.ModelConfig{
		{
			ModelName: "my-openai",
			Model:     "openai/gpt-4o",
			APIBase:   "https://api.openai.com/v1",
			APIKey:    *config.NewSecureString("sk-openai-secret"),
		},
	}
	if err := config.SaveConfig(configPath, cfg); err != nil {
		t.Fatalf("SaveConfig error: %v", err)
	}

	h := NewHandler(configPath)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	body := fmt.Sprintf(`{"provider":"groq","api_base":"%s","model_index":0}`, srv.URL)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/models/fetch", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	if requestReceivedWithAuth {
		t.Fatal("stored openai key should NOT be sent to mismatched provider (groq)")
	}
}
