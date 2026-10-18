package providers

import (
	"context"
	"errors"
	"fmt"
)

// CompleteOptions tunes a one-shot completion.
type CompleteOptions struct {
	// MaxTokens caps the reply. It must be positive: there is no default, so a
	// caller that forgets it fails loudly instead of silently getting a
	// provider's own fallback (the Anthropic provider would use 4,096).
	MaxTokens   int
	Temperature float64
	// CacheKey, when non-empty, is sent as prompt_cache_key for providers that
	// support prefix caching.
	CacheKey string
	// System, when non-empty, is sent as a leading system message.
	System string
}

// Complete is the plain "prompt in, text out" LLM call: one user message (plus
// an optional system message), no tools, and no tool loop. It lives here, with
// no build tags, so every target can reuse it.
//
// max_tokens is passed as an int on purpose: the Anthropic provider only honours
// an int and the Messages-API provider rejects a missing value. The context's
// deadline is passed through to the provider and its error is returned as is.
// usage is the zero value when the provider reports none.
func Complete(
	ctx context.Context,
	p LLMProvider,
	model, prompt string,
	o CompleteOptions,
) (text string, usage UsageInfo, err error) {
	if p == nil {
		return "", UsageInfo{}, errors.New("providers.Complete: nil provider")
	}
	if o.MaxTokens <= 0 {
		return "", UsageInfo{}, fmt.Errorf("providers.Complete: MaxTokens must be positive, got %d", o.MaxTokens)
	}

	messages := make([]Message, 0, 2)
	if o.System != "" {
		messages = append(messages, Message{Role: "system", Content: o.System})
	}
	messages = append(messages, Message{Role: "user", Content: prompt})

	options := map[string]any{
		"max_tokens":  o.MaxTokens,
		"temperature": o.Temperature,
	}
	if o.CacheKey != "" {
		options["prompt_cache_key"] = o.CacheKey
	}

	resp, err := p.Chat(ctx, messages, nil, model, options)
	if err != nil {
		return "", UsageInfo{}, err
	}
	if resp == nil {
		return "", UsageInfo{}, errors.New("providers.Complete: provider returned no response")
	}
	if resp.Usage != nil {
		usage = *resp.Usage
	}
	return resp.Content, usage, nil
}
