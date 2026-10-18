package agent

import (
	"context"
	"errors"

	"github.com/cryptoquantumwave/khunquant/pkg/providers"
)

// CompleteLight is a plain, tool-less completion on the default agent's light model
// (the routing light model when the agent has one, else its normal model, as a
// proactive turn pinned to the light tier does): one system message, one user
// message, no tools, no tool loop, no session and no memory. The reply is capped at
// maxTokens and drawn at temperature 0.
//
// NoNativeTools keeps a provider from adding tools of its own (the Codex provider's
// built-in web search), so the call is tool-less in fact and not only in what is passed.
//
// It is for work that reads text somebody else wrote and must not be able to act on
// it (the news source's article parser). The caller makes it a budgeted cognition job.
func (al *AgentLoop) CompleteLight(ctx context.Context, system, prompt string, maxTokens int) (string, providers.UsageInfo, error) {
	reg := al.GetRegistry()
	if reg == nil {
		return "", providers.UsageInfo{}, errors.New("agent: no agent registry")
	}
	agent := reg.GetDefaultAgent()
	if agent == nil {
		return "", providers.UsageInfo{}, errors.New("agent: no default agent")
	}
	provider, model := agent.Provider, resolvedCandidateModel(agent.Candidates, agent.Model)
	if len(agent.LightCandidates) > 0 && agent.LightProvider != nil && agent.Router != nil {
		provider, model = agent.LightProvider, resolvedCandidateModel(agent.LightCandidates, agent.Router.LightModel())
	}
	return providers.Complete(ctx, provider, model, prompt, providers.CompleteOptions{
		MaxTokens: maxTokens, Temperature: 0, System: system, NoNativeTools: true,
	})
}
