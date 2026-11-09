package providers

import (
	"fmt"
	"os"
	"strings"

	anthropicmessages "github.com/cryptoquantumwave/khunquant/pkg/providers/anthropic_messages"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/sim"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

// newSimInner builds the real model behind sim/record:<inner> and sim/live:<inner>.
//
// It does not go through CreateProviderFromConfig: the lock refuses every protocol but
// sim there. It is the one place a locked process builds a real provider, so it applies
// the whole of the lock's live rules, and fails (with an error that never holds the key)
// unless every one holds:
//   - the protocol is one of the HTTP clients on http.DefaultTransport, which the egress
//     guard replaces (anthropic-messages, openai, openrouter); never an agent CLI;
//   - <protocol>/<model> is on the KQ_SANDBOX_LLM_MODELS allowlist as written (the
//     protocol picks the host the key goes to) and has a known price;
//   - a budget is set (live without one is refused) and a key is given;
//   - the API host is the one host the lock allows (LockOptions.AllowHosts), no more.
//
// The key comes from the process environment only, is passed straight to the client, and
// is never put into a config, a journal entry or an error. No proxy is configured.
func newSimInner(mode, inner string) (*sim.LiveOptions, error) {
	return simInnerFrom(mode, inner, os.Getenv)
}

func simInnerFrom(mode, inner string, getenv func(string) string) (*sim.LiveOptions, error) {
	// First, before the environment is read: an unlocked process never builds a real model here.
	if !sandbox.Locked() {
		return nil, fmt.Errorf("sim/%s: the simulated model runs only in a sandbox-locked process", mode)
	}
	protocol, model, err := sandbox.SplitInnerModel(inner)
	if err != nil {
		return nil, fmt.Errorf("sim/%s: %w", mode, err)
	}
	env, err := sim.LiveEnvFrom(getenv)
	if err != nil {
		return nil, fmt.Errorf("sim/%s: %w", mode, err)
	}
	allowErr := sandbox.LLMModelAllowed(inner, env.Models)
	switch {
	case env.Key == "":
		return nil, fmt.Errorf("sim/%s: %s is not set", mode, sandbox.EnvLLMKey)
	case allowErr != nil:
		return nil, fmt.Errorf("sim/%s: %w", mode, allowErr)
	case env.BudgetUSD == 0 && env.BudgetTok == 0:
		return nil, fmt.Errorf("sim/%s: %s is not set; a live model never runs without a budget", mode, sandbox.EnvLLMBudget)
	}
	// A price for this protocol's model first (an aggregator may charge more), else the
	// model's list price.
	price, ok := env.Prices[protocol+"/"+model]
	if !ok {
		price, ok = env.Prices[model]
	}
	if !ok {
		return nil, fmt.Errorf("sim/%s: no price is known for model %q (add it with %s)", mode, model, sandbox.EnvLLMPrices)
	}
	host, err := sandbox.LLMInnerHost(inner, env.APIBase)
	if err != nil {
		return nil, fmt.Errorf("sim/%s: %w", mode, err)
	}
	if lk, _ := sandbox.LockInfo(); len(lk.AllowHosts) != 1 || !strings.EqualFold(lk.AllowHosts[0], host) {
		return nil, fmt.Errorf("sim/%s: the API host %q is not the one host the sandbox lock allows", mode, host)
	}

	base := env.APIBase
	if base == "" {
		base = sandbox.LLMDefaultBase[protocol]
	}
	var llm sim.LLM
	if protocol == "anthropic-messages" {
		llm = anthropicmessages.NewProviderWithTimeout(env.Key, base, 0)
	} else {
		llm = NewHTTPProviderWithMaxTokensFieldAndRequestTimeout(env.Key, base, "", "", 0)
	}
	return &sim.LiveOptions{Inner: llm, InnerModel: model, Price: price, Key: env.Key}, nil
}
