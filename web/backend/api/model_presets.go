package api

import "github.com/cryptoquantumwave/khunquant/pkg/config"

// ─────────────────────────────────────────────────────────────────────────────
// Credential-card model presets
//
// UPDATE THIS FILE when new models are released (frontier models change ~monthly).
// Each provider block is independent — just edit labels and model IDs.
//
// ModelID format: {provider-prefix}/{api-model-id}
//   openai/       → OpenAI API (Codex CLI uses ChatGPT session auth)
//   anthropic/    → Anthropic API
//   antigravity/  → Google Antigravity (cloudcode-pa.googleapis.com)
//
// Last verified: 2026-10-19
// ─────────────────────────────────────────────────────────────────────────────

// oauthProviderModelPresets defines the clickable model chips shown on each
// credential card. Order matters — first entry is shown leftmost/topmost.
//
// Sources checked when last updated:
//
//	OpenAI:      developers.openai.com/codex/models
//	Anthropic:   platform.claude.com/docs/en/about-claude/models/overview
//	Antigravity: official Antigravity CLI (agy models)
var oauthProviderModelPresets = map[string][]oauthModelPreset{
	// ── OpenAI ────────────────────────────────────────────────────────────────
	// GPT-6 family released late 2026, succeeding GPT-5.6.
	// gpt-6-astra  = flagship / most capable reasoning, $10/$50 per MTok
	// gpt-6.1-sol  = recommended workhorse default for production coding, $2/$10 per MTok
	// gpt-6-luna   = fast, cheap, high-volume economy tier, $0.10/$0.50 per MTok
	oauthProviderOpenAI: {
		{Label: "GPT-6.1 Sol", ModelID: config.DefaultOpenAIModel},
		{Label: "GPT-6 Astra", ModelID: "openai/gpt-6-astra"},
		{Label: "GPT-6 Luna", ModelID: "openai/gpt-6-luna"},
	},

	// ── Anthropic ─────────────────────────────────────────────────────────────
	// Claude 5.5 generation, released late 2026.
	// claude-opus-5-5   = flagship long-running agentic coding & knowledge work ($5/$25/MTok)
	// claude-sonnet-5-5 = recommended production default workhorse ($2/$10/MTok)
	// claude-haiku-5-5  = high-volume, low-latency, fast sub-agents ($1/$5/MTok)
	oauthProviderAnthropic: {
		{Label: "Claude Opus 5.5", ModelID: "anthropic/claude-opus-5-5"},
		{Label: "Claude Sonnet 5.5", ModelID: config.DefaultAnthropicModel},
		{Label: "Claude Haiku 5.5", ModelID: "anthropic/claude-haiku-5-5"},
	},

	// ── Google Antigravity ────────────────────────────────────────────────────
	// Model IDs MUST match what v1internal:fetchAvailableModels returns for the
	// account — the backend 404s ("NOT_FOUND: Requested entity was not found")
	// on any ID it doesn't expose. The provider strips the "antigravity/" prefix.
	// Labels mirror the Antigravity app's model picker; the IDs are the API IDs.
	// Verified live against official Antigravity CLI (agy models).
	oauthProviderGoogleAntigravity: {
		{Label: "Gemini 3.8 Flash (Medium)", ModelID: config.DefaultAntigravityModel},
		{Label: "Gemini 3.8 Flash (High)", ModelID: "antigravity/gemini-3.8-flash-high"},
		{Label: "Gemini 3.8 Flash (Low)", ModelID: "antigravity/gemini-3.8-flash-low"},
		{Label: "Gemini 4 Argon (High)", ModelID: "antigravity/gemini-4-argon-high"},
		{Label: "Gemini 4 Argon (Medium)", ModelID: "antigravity/gemini-4-argon-medium"},
		{Label: "Gemini 4 Argon (Low)", ModelID: "antigravity/gemini-4-argon-low"},
		{Label: "Claude Sonnet 5.5 (High)", ModelID: "antigravity/claude-sonnet-5-5-high"},
		{Label: "Claude Sonnet 5.5 (Medium)", ModelID: "antigravity/claude-sonnet-5-5-medium"},
		{Label: "Claude Opus 5.5 (High)", ModelID: "antigravity/claude-opus-5-5-high"},
		{Label: "Claude Opus 5.5 (Medium)", ModelID: "antigravity/claude-opus-5-5-medium"},
		{Label: "GPT-OSS 120B (Medium)", ModelID: "antigravity/gpt-oss-120b-medium"},
	},
}

// defaultModelForProvider returns the model ID to use when a provider is first
// connected and no existing model_list entry exists for it.
// Keep in sync with the first entry of each provider block above.
func defaultModelForProvider(provider string) string {
	switch provider {
	case oauthProviderOpenAI:
		return config.DefaultOpenAIModel
	case oauthProviderAnthropic:
		return config.DefaultAnthropicModel // sonnet is recommended production default
	case oauthProviderGoogleAntigravity:
		return config.DefaultAntigravityModel
	case oauthProviderGoogleGemini:
		return config.DefaultGeminiModel
	default:
		return ""
	}
}
