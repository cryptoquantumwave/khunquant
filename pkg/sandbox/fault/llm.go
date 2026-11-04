package fault

import (
	"context"
	"errors"
	"time"
)

// llmFailures are the provider failures a rule can inject. The text carries what the
// providers' error classifier reads (a status, "timeout"), so failover sees the real thing.
var llmFailures = map[string]string{
	"timeout":     "sim: injected llm timeout: context deadline exceeded",
	"rate_limit":  "sim: injected llm error: status: 429 rate limit exceeded",
	"server":      "sim: injected llm error: status: 503 service unavailable",
	"bad_request": "sim: injected llm error: status: 400 invalid request format",
}

// LLM applies the first active llm rule that matches a model call: the delay to wait
// (llm_latency, which the caller waits through Hold so a settle sees it) or the provider
// error to return (llm_error). turn is a turn kind or "complete"; caller is
// news|seahorse|complete for providers.Complete calls.
func (i *Injector) LLM(_ context.Context, turn, caller, _ string) (time.Duration, error) {
	f := i.pick(func(l layer) bool { return l == layerLLM }, &request{venue: venueLLM, turn: turn, caller: caller})
	if f == nil {
		return 0, nil
	}
	if e := f.rule.Effect; e.LLMError != "" {
		return 0, errors.New(llmFailures[e.LLMError])
	}
	return time.Duration(f.rule.Effect.LLMLatency), nil
}
