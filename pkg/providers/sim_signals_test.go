package providers

import (
	"context"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/providers/sim"
)

type optionRecorder struct{ options map[string]any }

func (r *optionRecorder) Chat(_ context.Context, _ []Message, _ []ToolDefinition, _ string, o map[string]any) (*LLMResponse, error) {
	r.options = o
	return &LLMResponse{Content: "x"}, nil
}

func (r *optionRecorder) GetDefaultModel() string { return "m" }

// The simulated model reads these Chat options to classify a call; sim cannot import
// this package, so it holds the names as literals and this test pins them.
func TestCompleteOptionsMatchSimSignals(t *testing.T) {
	rec := &optionRecorder{}
	_, _, err := Complete(context.Background(), rec, "m", "p", CompleteOptions{MaxTokens: 5, CacheKey: "k", NoNativeTools: true})
	if err != nil {
		t.Fatal(err)
	}
	if rec.options[sim.OptionCacheKey] != "k" || rec.options[sim.OptionNoNativeTools] != true {
		t.Errorf("Complete options = %v, want %s and %s set", rec.options, sim.OptionCacheKey, sim.OptionNoNativeTools)
	}
	rec = &optionRecorder{}
	if _, _, err := Complete(context.Background(), rec, "m", "p", CompleteOptions{MaxTokens: 5}); err != nil {
		t.Fatal(err)
	}
	if _, has := rec.options[sim.OptionCacheKey]; has {
		t.Error("a Complete call without CacheKey must send no prompt_cache_key")
	}
}
