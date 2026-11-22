package providers

import (
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/sim"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/sim/simtest"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

// A token budget needs no price: a local model has none, and the budget is counted in tokens.
// A USD budget cannot run for a model with no price.
func TestLiveTokenBudgetNeedsNoPrice(t *testing.T) {
	fake := simtest.New(t)
	// The price table is empty for this model: the default table has no entry for it either.
	liveEnv(t, fake.Base(), map[string]string{sandbox.EnvLLMPrices: "", sandbox.EnvLLMBudget: "20000000tok"})
	lockSandboxForTest(t, "127.0.0.1")
	attachLive(t, sim.ModeLive)
	p, _, err := CreateProviderFromConfig(&config.ModelConfig{ModelName: "sim", Model: "sim/live:openai/" + testModel})
	if err != nil || p == nil {
		t.Fatalf("a token budget with no price must start: (%v, %v)", p, err)
	}
	resp, err := chatOnce(p)
	if err != nil || resp.Content != "fake reply" {
		t.Fatalf("Chat = %v, %v", resp, err)
	}
}

func TestLiveUSDBudgetStillNeedsAPrice(t *testing.T) {
	fake := simtest.New(t)
	liveEnv(t, fake.Base(), map[string]string{sandbox.EnvLLMPrices: "", sandbox.EnvLLMBudget: "2USD"})
	lockSandboxForTest(t, "127.0.0.1")
	p, _, err := CreateProviderFromConfig(&config.ModelConfig{ModelName: "sim", Model: "sim/live:openai/" + testModel})
	if err == nil || p != nil || !strings.Contains(err.Error(), "no price") || !strings.Contains(err.Error(), "token budget") {
		t.Fatalf("a USD budget with no price must be refused and name the token budget: (%v, %v)", p, err)
	}
}
