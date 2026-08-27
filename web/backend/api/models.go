package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/providers"
)

// fetchableProviders lists providers that support OpenAI-compatible /models listing.
var fetchableProviders = map[string]bool{
	"openai": true, "deepseek": true, "openrouter": true,
	"qwen-portal": true, "qwen-intl": true, "moonshot": true,
	"volcengine": true, "zhipu": true, "groq": true,
	"mistral": true, "nvidia": true, "cerebras": true,
	"venice": true, "shengsuanyun": true, "vivgrid": true,
	"minimax": true, "longcat": true, "modelscope": true,
	"mimo": true, "avian": true, "zai": true, "novita": true,
	"litellm": true, "vllm": true, "lmstudio": true, "ollama": true,
}

// registerModelRoutes binds model list management endpoints to the ServeMux.
func (h *Handler) registerModelRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/models", h.handleListModels)
	mux.HandleFunc("POST /api/models", h.handleAddModel)
	mux.HandleFunc("POST /api/models/default", h.handleSetDefaultModel)
	mux.HandleFunc("PUT /api/models/{index}", h.handleUpdateModel)
	mux.HandleFunc("DELETE /api/models/{index}", h.handleDeleteModel)
	mux.HandleFunc("POST /api/models/fetch", h.handleFetchModels)
	mux.HandleFunc("GET /api/models/catalog", h.handleListCatalogs)
	mux.HandleFunc("DELETE /api/models/catalog/{id}", h.handleDeleteCatalog)
	mux.HandleFunc("POST /api/models/{index}/test", h.handleTestModel)
	mux.HandleFunc("POST /api/models/test-inline", h.handleTestInlineModel)
}

// modelResponse is the JSON structure returned for each model in the list.
// All ModelConfig fields are included so the frontend can display and edit them.
type modelResponse struct {
	Index      int    `json:"index"`
	ModelName  string `json:"model_name"`
	Model      string `json:"model"`
	APIBase    string `json:"api_base,omitempty"`
	APIKey     string `json:"api_key"`
	Proxy      string `json:"proxy,omitempty"`
	AuthMethod string `json:"auth_method,omitempty"`
	// Advanced fields
	ConnectMode    string `json:"connect_mode,omitempty"`
	Workspace      string `json:"workspace,omitempty"`
	RPM            int    `json:"rpm,omitempty"`
	MaxTokensField string `json:"max_tokens_field,omitempty"`
	RequestTimeout int    `json:"request_timeout,omitempty"`
	ThinkingLevel  string `json:"thinking_level,omitempty"`
	// Meta
	Configured bool `json:"configured"`
	IsDefault  bool `json:"is_default"`
}

// handleListModels returns all model_list entries with masked API keys.
//
//	GET /api/models
func (h *Handler) handleListModels(w http.ResponseWriter, r *http.Request) {
	cfg, err := config.LoadConfig(h.configPath)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to load config: %v", err), http.StatusInternalServerError)
		return
	}

	defaultModel := cfg.Agents.Defaults.GetModelName()
	configured := make([]bool, len(cfg.ModelList))

	var wg sync.WaitGroup
	wg.Add(len(cfg.ModelList))
	for i, m := range cfg.ModelList {
		go func(i int, m config.ModelConfig) {
			defer wg.Done()
			configured[i] = isModelConfigured(m)
		}(i, m)
	}
	wg.Wait()

	models := make([]modelResponse, 0, len(cfg.ModelList))
	for i, m := range cfg.ModelList {
		models = append(models, modelResponse{
			Index:          i,
			ModelName:      m.ModelName,
			Model:          m.Model,
			APIBase:        m.APIBase,
			APIKey:         maskAPIKey(m.APIKey.String()),
			Proxy:          m.Proxy,
			AuthMethod:     m.AuthMethod,
			ConnectMode:    m.ConnectMode,
			Workspace:      m.Workspace,
			RPM:            m.RPM,
			MaxTokensField: m.MaxTokensField,
			RequestTimeout: m.RequestTimeout,
			ThinkingLevel:  m.ThinkingLevel,
			Configured:     configured[i],
			IsDefault:      m.ModelName == defaultModel,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"models":        models,
		"total":         len(models),
		"default_model": defaultModel,
	})
}

// handleAddModel appends a new model configuration entry.
//
//	POST /api/models
func (h *Handler) handleAddModel(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	var mc config.ModelConfig
	if err = json.Unmarshal(body, &mc); err != nil {
		http.Error(w, fmt.Sprintf("Invalid JSON: %v", err), http.StatusBadRequest)
		return
	}

	if err = mc.Validate(); err != nil {
		http.Error(w, fmt.Sprintf("Validation error: %v", err), http.StatusBadRequest)
		return
	}

	cfg, err := config.LoadConfig(h.configPath)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to load config: %v", err), http.StatusInternalServerError)
		return
	}

	cfg.ModelList = append(cfg.ModelList, mc)

	if err := config.SaveConfig(h.configPath, cfg); err != nil {
		http.Error(w, fmt.Sprintf("Failed to save config: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"status": "ok",
		"index":  len(cfg.ModelList) - 1,
	})
}

// handleUpdateModel replaces a model configuration entry at the given index.
// If the request body omits api_key (or sends an empty string), the existing
// stored key is preserved so callers can update only api_base / proxy without
// exposing or clearing the secret.
//
//	PUT /api/models/{index}
func (h *Handler) handleUpdateModel(w http.ResponseWriter, r *http.Request) {
	idx, err := strconv.Atoi(r.PathValue("index"))
	if err != nil {
		http.Error(w, "Invalid index", http.StatusBadRequest)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	type custom struct {
		config.ModelConfig
		APIKey string `json:"api_key"`
	}

	var mc custom
	if err = json.Unmarshal(body, &mc); err != nil {
		http.Error(w, fmt.Sprintf("Invalid JSON: %v", err), http.StatusBadRequest)
		return
	}

	if err = mc.Validate(); err != nil {
		http.Error(w, fmt.Sprintf("Validation error: %v", err), http.StatusBadRequest)
		return
	}

	cfg, err := config.LoadConfig(h.configPath)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to load config: %v", err), http.StatusInternalServerError)
		return
	}

	if idx < 0 || idx >= len(cfg.ModelList) {
		http.Error(w, fmt.Sprintf("Index %d out of range (0-%d)", idx, len(cfg.ModelList)-1), http.StatusNotFound)
		return
	}

	// Preserve the existing API key when the caller omits it (empty string).
	// This lets the UI update api_base / proxy without clearing the stored secret.
	if mc.APIKey == "" {
		mc.ModelConfig.APIKey = cfg.ModelList[idx].APIKey
	} else {
		mc.ModelConfig.APIKey = *config.NewSecureString(mc.APIKey)
	}

	cfg.ModelList[idx] = mc.ModelConfig

	if err := config.SaveConfig(h.configPath, cfg); err != nil {
		http.Error(w, fmt.Sprintf("Failed to save config: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// handleDeleteModel removes a model configuration entry at the given index.
//
//	DELETE /api/models/{index}
func (h *Handler) handleDeleteModel(w http.ResponseWriter, r *http.Request) {
	idx, err := strconv.Atoi(r.PathValue("index"))
	if err != nil {
		http.Error(w, "Invalid index", http.StatusBadRequest)
		return
	}

	cfg, err := config.LoadConfig(h.configPath)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to load config: %v", err), http.StatusInternalServerError)
		return
	}

	if idx < 0 || idx >= len(cfg.ModelList) {
		http.Error(w, fmt.Sprintf("Index %d out of range (0-%d)", idx, len(cfg.ModelList)-1), http.StatusNotFound)
		return
	}

	deletedModelName := cfg.ModelList[idx].ModelName

	cfg.ModelList = append(cfg.ModelList[:idx], cfg.ModelList[idx+1:]...)

	// If the deleted model was the default, clear it.
	if cfg.Agents.Defaults.ModelName == deletedModelName {
		cfg.Agents.Defaults.ModelName = ""
	}
	if cfg.Agents.Defaults.Model == deletedModelName {
		cfg.Agents.Defaults.Model = ""
	}

	if err := config.SaveConfig(h.configPath, cfg); err != nil {
		http.Error(w, fmt.Sprintf("Failed to save config: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// handleSetDefaultModel sets the default model for all agents.
//
//	POST /api/models/default
func (h *Handler) handleSetDefaultModel(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	var req struct {
		ModelName string `json:"model_name"`
	}
	if err = json.Unmarshal(body, &req); err != nil {
		http.Error(w, fmt.Sprintf("Invalid JSON: %v", err), http.StatusBadRequest)
		return
	}

	if req.ModelName == "" {
		http.Error(w, "model_name is required", http.StatusBadRequest)
		return
	}

	cfg, err := config.LoadConfig(h.configPath)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to load config: %v", err), http.StatusInternalServerError)
		return
	}

	// Verify the model_name exists in model_list
	found := false
	for _, m := range cfg.ModelList {
		if m.ModelName == req.ModelName {
			found = true
			break
		}
	}
	if !found {
		http.Error(w, fmt.Sprintf("Model %q not found in model_list", req.ModelName), http.StatusNotFound)
		return
	}

	cfg.Agents.Defaults.ModelName = req.ModelName

	if err := config.SaveConfig(h.configPath, cfg); err != nil {
		http.Error(w, fmt.Sprintf("Failed to save config: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status":        "ok",
		"default_model": req.ModelName,
	})
}

// maskAPIKey returns a masked version of an API key for safe display.
// Keys longer than 12 chars show prefix + last 4 chars: "sk-****abcd".
// Keys 9-12 chars show prefix + last 2 chars: "sk-****cd".
// Shorter keys are fully masked as "****".
// Empty keys return empty string.
// Ensure at least 40% of the key is masked.
func maskAPIKey(key string) string {
	if key == "" {
		return ""
	}

	if len(key) <= 8 {
		return "****"
	}

	// Show first 3 chars and last 2 chars
	if len(key) <= 12 {
		return key[:3] + "****" + key[len(key)-2:]
	}

	// Show first 3 chars and last 4 chars
	return key[:3] + "****" + key[len(key)-4:]
}

// handleFetchModels fetches available models from an upstream provider.
//
//	POST /api/models/fetch
func (h *Handler) handleFetchModels(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	var req struct {
		Provider   string `json:"provider"`
		APIKey     string `json:"api_key"`
		APIBase    string `json:"api_base"`
		ModelIndex *int   `json:"model_index,omitempty"`
	}
	if err = json.Unmarshal(body, &req); err != nil {
		http.Error(w, fmt.Sprintf("Invalid JSON: %v", err), http.StatusBadRequest)
		return
	}

	if req.Provider == "" {
		http.Error(w, "provider is required", http.StatusBadRequest)
		return
	}

	if !fetchableProviders[strings.ToLower(req.Provider)] {
		http.Error(w, fmt.Sprintf("provider %q does not support model listing", req.Provider), http.StatusBadRequest)
		return
	}

	apiKey := strings.TrimSpace(req.APIKey)
	apiBase := strings.TrimSpace(req.APIBase)

	if apiKey == "" && req.ModelIndex != nil {
		if stored := h.lookupStoredAPIKey(*req.ModelIndex, req.Provider, apiBase); stored != "" {
			apiKey = stored
		}
	}

	if apiBase == "" {
		// Resolve default API base for the provider
		tempConfig := &config.ModelConfig{Model: req.Provider + "/dummy"}
		apiBase = providers.ResolveAPIBase(tempConfig)
	}
	if apiBase == "" {
		http.Error(w, fmt.Sprintf("No default API base for provider %q", req.Provider), http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	models, err := fetchUpstreamModels(ctx, req.Provider, apiBase, apiKey)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to fetch models: %v", err), http.StatusBadGateway)
		return
	}

	// Auto-save fetched models to catalog
	catalogModels := make([]CatalogModel, len(models))
	for i, m := range models {
		catalogModels[i] = CatalogModel{ID: m.ID, OwnedBy: m.OwnedBy}
	}
	if saveErr := SaveCatalog(req.Provider, apiBase, apiKey, catalogModels); saveErr != nil {
		// Log but don't fail the request — saving catalog is non-critical
		logger.WarnF("Failed to save model catalog: %v", map[string]any{"error": saveErr})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"models": models,
		"total":  len(models),
	})
}

func (h *Handler) lookupStoredAPIKey(index int, reqProvider, reqAPIBase string) string {
	cfg, err := config.LoadConfig(h.configPath)
	if err != nil || index < 0 || index >= len(cfg.ModelList) {
		return ""
	}
	stored := cfg.ModelList[index]
	storedProvider, _ := providers.ExtractProtocol(stored.Model)
	if providers.NormalizeProvider(reqProvider) != providers.NormalizeProvider(storedProvider) {
		return ""
	}
	effectiveReqBase := strings.TrimSpace(reqAPIBase)
	if effectiveReqBase == "" {
		tempConfig := &config.ModelConfig{Model: reqProvider + "/dummy"}
		effectiveReqBase = providers.ResolveAPIBase(tempConfig)
	}
	effectiveStoredBase := strings.TrimSpace(stored.APIBase)
	if effectiveStoredBase == "" {
		effectiveStoredBase = providers.ResolveAPIBase(&stored)
	}
	if normalizeAPIBaseForCompare(effectiveReqBase) != normalizeAPIBaseForCompare(effectiveStoredBase) {
		return ""
	}
	return stored.APIKey.String()
}

// normalizeAPIBaseForCompare normalizes an API base URL for equality comparison
// by trimming trailing slashes and lowering the scheme/host.
func normalizeAPIBaseForCompare(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	raw = strings.TrimRight(raw, "/")
	u, err := url.Parse(raw)
	if err != nil {
		return strings.ToLower(raw)
	}
	if u.Host == "" {
		u, err = url.Parse("//" + raw)
		if err != nil {
			return strings.ToLower(raw)
		}
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host) + strings.TrimRight(u.Path, "/")
}

type upstreamModel struct {
	ID      string `json:"id"`
	OwnedBy string `json:"owned_by,omitempty"`
}

func fetchUpstreamModels(ctx context.Context, provider, apiBase, apiKey string) ([]upstreamModel, error) {
	apiBase = strings.TrimRight(strings.TrimSpace(apiBase), "/")

	var fetchURL string
	switch strings.ToLower(provider) {
	case "ollama":
		// Strip /v1 suffix if present to get the Ollama root
		root := apiBase
		if strings.HasSuffix(root, "/v1") {
			root = root[:len(root)-3]
		}
		root = strings.TrimRight(root, "/")
		fetchURL = root + "/api/tags"
		return fetchOllamaModels(ctx, fetchURL)
	default:
		// OpenAI-compatible: /v1/models
		fetchURL = apiBase + "/models"
		return fetchOpenAICompatibleModels(ctx, fetchURL, apiKey)
	}
}

func fetchOpenAICompatibleModels(ctx context.Context, fetchURL, apiKey string) ([]upstreamModel, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fetchURL, nil)
	if err != nil {
		return nil, err
	}
	if apiKey = strings.TrimSpace(apiKey); apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstream returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	type modelItem struct {
		ID      string `json:"id"`
		OwnedBy string `json:"owned_by"`
	}

	// {"data": [...]} envelope. Distinguish "envelope shape with empty list"
	// from "object without a data key" via Data being non-nil after unmarshal:
	// json.Unmarshal sets Data to []modelItem{} for `{"data":[]}` but leaves
	// it as nil when "data" is absent or null.
	var envelope struct {
		Data []modelItem `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil && envelope.Data != nil {
		models := make([]upstreamModel, 0, len(envelope.Data))
		for _, m := range envelope.Data {
			if m.ID != "" {
				models = append(models, upstreamModel{ID: m.ID, OwnedBy: m.OwnedBy})
			}
		}
		return models, nil
	}

	// Bare-array shape, including `[]`.
	var arr []modelItem
	if err := json.Unmarshal(body, &arr); err == nil {
		models := make([]upstreamModel, 0, len(arr))
		for _, m := range arr {
			if m.ID != "" {
				models = append(models, upstreamModel{ID: m.ID, OwnedBy: m.OwnedBy})
			}
		}
		return models, nil
	}

	preview := body
	if len(preview) > 256 {
		preview = preview[:256]
	}
	return nil, fmt.Errorf("decode response: unrecognized shape: %s", strings.TrimSpace(string(preview)))
}

func fetchOllamaModels(ctx context.Context, fetchURL string) ([]upstreamModel, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fetchURL, nil)
	if err != nil {
		return nil, err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ollama returned status %d", resp.StatusCode)
	}

	var parsed struct {
		Models []struct {
			Name  string `json:"name"`
			Model string `json:"model"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}

	models := make([]upstreamModel, 0, len(parsed.Models))
	for _, m := range parsed.Models {
		id := m.Name
		if id == "" {
			id = m.Model
		}
		if id != "" {
			models = append(models, upstreamModel{ID: id})
		}
	}
	return models, nil
}

// handleTestModel tests connectivity to a model endpoint.
//
//	POST /api/models/{index}/test
func (h *Handler) handleTestModel(w http.ResponseWriter, r *http.Request) {
	idx, err := strconv.Atoi(r.PathValue("index"))
	if err != nil {
		http.Error(w, "Invalid index", http.StatusBadRequest)
		return
	}

	cfg, err := config.LoadConfig(h.configPath)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to load config: %v", err), http.StatusInternalServerError)
		return
	}

	if idx < 0 || idx >= len(cfg.ModelList) {
		http.Error(w, fmt.Sprintf("Index %d out of range (0-%d)", idx, len(cfg.ModelList)-1), http.StatusNotFound)
		return
	}

	m := cfg.ModelList[idx]
	start := time.Now()
	summary := modelConfigurationStatus(m)
	latency := time.Since(start).Milliseconds()

	result := map[string]any{
		"success":    summary.Available,
		"latency_ms": latency,
		"status":     summary.Status,
	}

	if !summary.Available {
		if summary.Status == modelStatusUnconfigured {
			result["error"] = "API key not configured"
		} else {
			result["error"] = "Endpoint unreachable"
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// handleTestInlineModel tests connectivity using inline (unsaved) parameters.
// Unlike handleTestModel which only checks saved config, this endpoint performs
// a real network probe (e.g. GET /models) to verify the endpoint is reachable.
//
//	POST /api/models/test-inline
func (h *Handler) handleTestInlineModel(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	var req struct {
		Provider   string `json:"provider"`
		Model      string `json:"model"`
		APIBase    string `json:"api_base"`
		APIKey     string `json:"api_key"`
		AuthMethod string `json:"auth_method"`
		ModelIndex *int   `json:"model_index"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	// Build the Model field from provider and model ID
	modelStr := strings.TrimSpace(req.Model)
	providerStr := strings.TrimSpace(req.Provider)
	if providerStr != "" && !strings.Contains(modelStr, "/") {
		modelStr = providerStr + "/" + modelStr
	}

	m := &config.ModelConfig{
		Model:      modelStr,
		APIBase:    strings.TrimSpace(req.APIBase),
		AuthMethod: strings.TrimSpace(req.AuthMethod),
	}
	if req.APIKey != "" {
		m.APIKey = *config.NewSecureString(req.APIKey)
	}

	// When api_key is empty and model_index is provided, fall back to stored credentials.
	// This lets the edit form test unsaved field changes while using the saved key.
	// Only reuse the stored key when the provider and effective API base match
	// the saved model, to prevent attaching a credential to a different endpoint.
	if req.APIKey == "" && req.ModelIndex != nil {
		cfg, err := config.LoadConfig(h.configPath)
		if err == nil && *req.ModelIndex >= 0 && *req.ModelIndex < len(cfg.ModelList) {
			stored := cfg.ModelList[*req.ModelIndex]
			storedProvider, _ := providers.ExtractProtocol(stored.Model)
			reqProvider, _ := providers.ExtractProtocol(m.Model)
			providerMatch := reqProvider == "" || reqProvider == providers.NormalizeProvider(storedProvider)

			effectiveReqBase := strings.TrimSpace(m.APIBase)
			if effectiveReqBase == "" {
				effectiveReqBase = providers.ResolveAPIBase(m)
			}
			effectiveStoredBase := strings.TrimSpace(stored.APIBase)
			if effectiveStoredBase == "" {
				effectiveStoredBase = providers.ResolveAPIBase(&stored)
			}
			baseMatch := normalizeAPIBaseForCompare(effectiveReqBase) == normalizeAPIBaseForCompare(effectiveStoredBase)

			if providerMatch && baseMatch {
				if stored.APIKey.String() != "" {
					m.APIKey = stored.APIKey
				}
				if m.APIBase == "" && stored.APIBase != "" {
					m.APIBase = stored.APIBase
				}
			}
		}
	}

	// Check if configuration exists
	if !hasModelConfiguration(*m) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"success":    false,
			"latency_ms": 0,
			"status":     modelStatusUnconfigured,
			"error":      "API key not configured",
		})
		return
	}

	// Perform a real network probe
	start := time.Now()
	available := probeModelConnectivity(m)
	latency := time.Since(start).Milliseconds()

	result := map[string]any{
		"success":    available,
		"latency_ms": latency,
	}
	if available {
		result["status"] = modelStatusAvailable
	} else {
		result["status"] = modelStatusUnreachable
		result["error"] = "Endpoint unreachable"
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// probeModelConnectivity performs a real network probe to verify model endpoint reachability.
func probeModelConnectivity(m *config.ModelConfig) bool {
	apiBase := modelProbeAPIBase(*m)
	protocol, modelID := splitModel(m.Model)

	switch protocol {
	case "ollama":
		return probeOllamaModelFunc(apiBase, modelID)
	case "vllm", "lmstudio":
		return probeOpenAICompatibleModelFunc(apiBase, modelID, m.APIKey.String())
	case "github-copilot", "copilot":
		return probeTCPServiceFunc(apiBase)
	case "claude-cli", "claudecli", "codex-cli", "codexcli":
		// CLI-based providers are always reachable if installed
		return true
	default:
		// For remote providers (OpenAI, Anthropic, Gemini, DeepSeek, etc.),
		// make a real GET /models request to verify connectivity and credentials.
		if apiBase != "" {
			return probeOpenAICompatibleModelFunc(apiBase, modelID, m.APIKey.String())
		}
		return false
	}
}
