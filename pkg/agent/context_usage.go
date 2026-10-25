package agent

import (
	"context"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/tokenizer"
)

// computeContextUsage estimates current context window consumption for the
// given agent and session. Includes history, system prompt (with dynamic context,
// summary, and skills — mirroring BuildMessages composition), and tool definitions.
// The output reserve (MaxTokens) is not counted as "used" but reduces the
// effective budget, matching isOverContextBudget's compression trigger:
//
//	compress when: history + system + tools + maxTokens > contextWindow
//	equivalent to: history + system + tools > contextWindow - maxTokens
//
// Returns nil when the agent or session is unavailable.
func computeContextUsage(agent *AgentInstance, sessionKey string) *bus.ContextUsage {
	return computeContextUsageFor(context.Background(), agent, sessionKey, BuildOptions{Mode: PromptFull})
}

// computeContextUsageFor is computeContextUsage for a running turn: the tool
// definitions it counts are the ones that turn is shown (see
// ToolRegistry.ToProviderDefsFor), so an autonomous turn's usage does not
// include tools it cannot see. The system prompt it counts is the one the turn
// is built with (promptOpts): a light turn is not charged for skill bodies it
// never sends.
func computeContextUsageFor(
	ctx context.Context,
	agent *AgentInstance,
	sessionKey string,
	promptOpts BuildOptions,
) *bus.ContextUsage {
	if agent == nil || agent.Sessions == nil {
		return nil
	}
	contextWindow := agent.ContextWindow
	if contextWindow <= 0 {
		return nil
	}

	// History tokens
	history := agent.Sessions.GetHistory(sessionKey)
	historyTokens := 0
	for _, m := range history {
		historyTokens += tokenizer.EstimateMessageTokens(m)
	}

	// System message tokens: uses EstimateSystemTokensFor which mirrors
	// the full system message composition in BuildMessages (static prompt,
	// dynamic context, active skills, summary with wrapping prefix).
	systemTokens := 0
	if agent.ContextBuilder != nil {
		summary := agent.Sessions.GetSummary(sessionKey)
		systemTokens = agent.ContextBuilder.EstimateSystemTokensFor(promptOpts, summary)
	}

	// Tool definition tokens
	toolTokens := 0
	if agent.Tools != nil {
		toolTokens = tokenizer.EstimateToolDefsTokens(agent.Tools.ToProviderDefsFor(ctx))
	}

	// Used = history + system (includes summary) + tools
	usedTokens := historyTokens + systemTokens + toolTokens

	// Effective budget = contextWindow minus output reserve (maxTokens)
	effectiveWindow := contextWindow - agent.MaxTokens
	if effectiveWindow < 0 {
		effectiveWindow = contextWindow
	}

	// compressAt = effectiveWindow: aligns with isOverContextBudget's
	// proactive trigger (msgTokens + toolTokens + maxTokens > contextWindow).
	compressAt := effectiveWindow

	usedPercent := 0
	if compressAt > 0 {
		usedPercent = usedTokens * 100 / compressAt
	}
	if usedPercent > 100 {
		usedPercent = 100
	}

	return &bus.ContextUsage{
		UsedTokens:       usedTokens,
		TotalTokens:      contextWindow,
		CompressAtTokens: compressAt,
		UsedPercent:      usedPercent,
	}
}
