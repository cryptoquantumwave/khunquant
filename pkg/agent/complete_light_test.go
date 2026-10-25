package agent

import (
	"context"
	"sync"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/providers"
	"github.com/cryptoquantumwave/khunquant/pkg/routing"
)

// oneShotRecorder records exactly what a provider is asked, including the tools.
type oneShotRecorder struct {
	mu       sync.Mutex
	calls    int
	messages []providers.Message
	tools    []providers.ToolDefinition
	model    string
	options  map[string]any
	reply    string
}

func (r *oneShotRecorder) Chat(_ context.Context, m []providers.Message, tools []providers.ToolDefinition, model string, o map[string]any) (*providers.LLMResponse, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.messages, r.tools, r.model, r.options = m, tools, model, o
	return &providers.LLMResponse{Content: r.reply, Usage: &providers.UsageInfo{PromptTokens: 7, CompletionTokens: 3}}, nil
}

func (r *oneShotRecorder) GetDefaultModel() string { return "recorder" }

// CompleteLight is a plain completion: a system message and a user message, no tools,
// the cap and temperature asked for, on the light model when the agent has one.
func TestCompleteLight_NoToolsOnTheLightModel(t *testing.T) {
	primary := &oneShotRecorder{reply: "from primary"}
	al, _ := defaultWorkspaceLoop(t, primary)

	text, usage, err := al.CompleteLight(context.Background(), "SYS", "USER", 400)
	if err != nil || text != "from primary" || usage.PromptTokens != 7 {
		t.Fatalf("%q %+v %v", text, usage, err)
	}
	if primary.tools != nil {
		t.Errorf("tools were passed to the model: %v", primary.tools)
	}
	if len(primary.messages) != 2 || primary.messages[0].Role != "system" || primary.messages[0].Content != "SYS" ||
		primary.messages[1].Role != "user" || primary.messages[1].Content != "USER" {
		t.Errorf("messages = %+v", primary.messages)
	}
	if primary.options["max_tokens"] != 400 || primary.options["temperature"] != 0.0 ||
		primary.options[providers.OptionNoNativeTools] != true {
		t.Errorf("options = %v", primary.options)
	}
	if primary.model == "" {
		t.Error("no model")
	}

	// With a light model configured, it is the light provider that is called.
	light := &oneShotRecorder{reply: "from light"}
	def := al.GetRegistry().GetDefaultAgent()
	def.LightProvider = light
	def.LightCandidates = []providers.FallbackCandidate{{Provider: "light-prov", Model: "light-model-1"}}
	def.Router = routing.New(routing.RouterConfig{LightModel: "light-alias", Threshold: 0.5})

	text, _, err = al.CompleteLight(context.Background(), "SYS", "USER", 400)
	if err != nil || text != "from light" || light.model != "light-model-1" || light.tools != nil {
		t.Fatalf("light: %q model %q tools %v err %v", text, light.model, light.tools, err)
	}
	if primary.calls != 1 {
		t.Errorf("the primary model was called %d times, want only the first call", primary.calls)
	}
}
