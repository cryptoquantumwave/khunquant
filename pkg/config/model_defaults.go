// KhunQuant - Ultra-lightweight personal AI agent
// License: MIT
//
// Copyright (c) 2026 KhunQuant contributors

package config

// Default model IDs used when a provider is first connected via OAuth/token
// login and no matching entry exists yet in ModelList. Frontier models change
// often — this is the single place to update when a provider ships a new
// default, so cmd/khunquant, cmd/khunquant-launcher-tui, and the web launcher
// backend all pick up the change without hunting down scattered literals.
const (
	DefaultOpenAIModelName      = "gpt-6.1-sol"
	DefaultOpenAIModel          = "openai/" + DefaultOpenAIModelName
	DefaultAnthropicModelName   = "claude-sonnet-5.5"
	DefaultAnthropicModel       = "anthropic/claude-sonnet-5-5"
	DefaultAntigravityModelName = "gemini-3.8-flash-medium"
	DefaultAntigravityModel     = "antigravity/" + DefaultAntigravityModelName
	DefaultGeminiModelName      = "gemini-flash-codeassist"
	DefaultGeminiModel          = "gemini-code-assist/gemini-2.5-flash"
)

// DefaultModelForProvider returns the default "provider/model" ID for a given
// auth provider key (as used by `khunquant auth login --provider <key>`).
// Returns "" for an unrecognized provider.
func DefaultModelForProvider(provider string) string {
	switch provider {
	case "openai":
		return DefaultOpenAIModel
	case "anthropic":
		return DefaultAnthropicModel
	case "google-antigravity", "antigravity":
		return DefaultAntigravityModel
	case "google-gemini", "gemini-code-assist", "gemini-cli":
		return DefaultGeminiModel
	default:
		return ""
	}
}
