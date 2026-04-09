// KhunQuant - Ultra-lightweight personal AI agent
// Inspired by and based on nanobot: https://github.com/HKUDS/nanobot
// License: MIT
//
// Copyright (c) 2026 KhunQuant contributors

package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/khunquant/khunquant/pkg/providers/openai_compat"
)

// MLXLMProvider wraps the OpenAI-compatible HTTP provider for mlx_lm servers.
// Because mlx_lm loads exactly one model at startup and uses the model field
// to decide whether to download a new model, we auto-discover the loaded model
// ID via GET /v1/models and use that in every chat request.
type MLXLMProvider struct {
	delegate *openai_compat.Provider
	apiBase  string
	proxy    string

	mu              sync.Mutex
	discoveredModel string
}

func NewMLXLMProvider(apiKey, apiBase, proxy string, requestTimeoutSeconds int) *MLXLMProvider {
	return &MLXLMProvider{
		delegate: openai_compat.NewProvider(
			apiKey,
			apiBase,
			proxy,
			openai_compat.WithRequestTimeout(time.Duration(requestTimeoutSeconds)*time.Second),
		),
		apiBase: apiBase,
		proxy:   proxy,
	}
}

// resolveModel always queries the server for the loaded model ID and caches it.
// mlx_lm serves exactly one model at startup; the configured model name is only
// a local alias and must not be sent to the server verbatim.
func (p *MLXLMProvider) resolveModel(ctx context.Context) string {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.discoveredModel != "" {
		return p.discoveredModel
	}

	model := p.fetchLoadedModel(ctx)
	if model != "" {
		p.discoveredModel = model
	}
	return model
}

func (p *MLXLMProvider) fetchLoadedModel(ctx context.Context) string {
	apiBase := strings.TrimRight(strings.TrimSpace(p.apiBase), "/")
	if apiBase == "" {
		apiBase = "http://localhost:8080/v1"
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBase+"/models", nil)
	if err != nil {
		return ""
	}

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return ""
	}

	var result struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return ""
	}

	if len(result.Data) > 0 {
		return result.Data[0].ID
	}
	return ""
}

func (p *MLXLMProvider) Chat(
	ctx context.Context,
	messages []Message,
	tools []ToolDefinition,
	model string,
	options map[string]any,
) (*LLMResponse, error) {
	resolved := p.resolveModel(ctx)
	if resolved == "" {
		return nil, fmt.Errorf("mlx_lm: could not determine loaded model (is the server running?)")
	}
	return p.delegate.Chat(ctx, messages, tools, resolved, options)
}

func (p *MLXLMProvider) GetDefaultModel() string {
	return ""
}
