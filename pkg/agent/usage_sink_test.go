package agent

import (
	"context"
	"sync"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/providers"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

func usageResp(r providers.LLMResponse, in, out int) providers.LLMResponse {
	r.Usage = &providers.UsageInfo{PromptTokens: in, CompletionTokens: out, TotalTokens: in + out}
	return r
}

func toolCallResp(name string) providers.LLMResponse {
	return providers.LLMResponse{ToolCalls: []providers.ToolCall{
		{ID: "c1", Type: "function", Name: name, Arguments: map[string]any{}},
	}}
}

type sinkCalls struct {
	mu  sync.Mutex
	got []providers.UsageInfo
}

func (s *sinkCalls) sink(u providers.UsageInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, u)
}

// A turn of two LLM calls with a tool between them: the sink is called once with
// the sum.
func TestUsageSink_AggregatesAcrossIterations(t *testing.T) {
	prov := script(
		usageResp(toolCallResp("noop"), 10, 5),
		usageResp(providers.LLMResponse{Content: "final"}, 20, 8),
	)
	al, _ := ownerLoop(t, prov)
	agent := al.GetRegistry().GetDefaultAgent()
	agent.Tools.Register(&funcTool{name: "noop", fn: func(context.Context, map[string]any) *tools.ToolResult {
		return tools.SilentResult("ok")
	}})

	var sc sinkCalls
	if _, err := al.runAgentLoop(context.Background(), agent, processOptions{
		SessionKey: "sess-usage", Channel: "cli", ChatID: "direct", UserMessage: "hi",
		DefaultResponse: "x", UsageSink: sc.sink,
	}); err != nil {
		t.Fatalf("runAgentLoop: %v", err)
	}
	if len(sc.got) != 1 {
		t.Fatalf("sink called %d times, want exactly 1", len(sc.got))
	}
	want := providers.UsageInfo{PromptTokens: 30, CompletionTokens: 13, TotalTokens: 43}
	if sc.got[0] != want {
		t.Fatalf("usage = %+v, want %+v", sc.got[0], want)
	}
}

// A sub-turn run inside the turn adds its calls to the turn's total.
func TestUsageSink_IncludesSubTurns(t *testing.T) {
	prov := script(
		usageResp(toolCallResp("spawn_it"), 10, 5),
		usageResp(providers.LLMResponse{Content: "child done"}, 7, 3),
		usageResp(providers.LLMResponse{Content: "final"}, 20, 8),
	)
	al, _ := ownerLoop(t, prov)
	agent := al.GetRegistry().GetDefaultAgent()
	var spawnErr error
	agent.Tools.Register(&funcTool{name: "spawn_it", fn: func(ctx context.Context, _ map[string]any) *tools.ToolResult {
		_, spawnErr = SpawnSubTurn(ctx, SubTurnConfig{Model: "char-model", SystemPrompt: "child task", MaxTokens: 256})
		return tools.SilentResult("spawned")
	}})

	var sc sinkCalls
	if _, err := al.runAgentLoop(context.Background(), agent, processOptions{
		SessionKey: "sess-usage-sub", Channel: "cli", ChatID: "direct", UserMessage: "hi",
		DefaultResponse: "x", UsageSink: sc.sink,
	}); err != nil {
		t.Fatalf("runAgentLoop: %v", err)
	}
	if spawnErr != nil {
		t.Fatalf("SpawnSubTurn: %v", spawnErr)
	}
	if prov.callCount() != 3 {
		t.Fatalf("provider calls = %d, want 3 (parent, child, parent)", prov.callCount())
	}
	if len(sc.got) != 1 {
		t.Fatalf("sink called %d times, want exactly 1", len(sc.got))
	}
	if got, want := sc.got[0].PromptTokens+sc.got[0].CompletionTokens, 10+5+7+3+20+8; got != want {
		t.Fatalf("total tokens = %d, want %d (sub-turn included)", got, want)
	}
}

// With no sink the turn behaves as before; a sink on a provider that reports no
// usage gets one zero call.
func TestUsageSink_NilSafe(t *testing.T) {
	al, _ := ownerLoop(t, script(providers.LLMResponse{Content: "plain"}))
	agent := al.GetRegistry().GetDefaultAgent()
	got, err := al.runAgentLoop(context.Background(), agent, processOptions{
		SessionKey: "sess-nil", Channel: "cli", ChatID: "direct", UserMessage: "hi", DefaultResponse: "x",
	})
	if err != nil || got != "plain" {
		t.Fatalf("runAgentLoop = %q, %v; want plain, nil", got, err)
	}

	var sc sinkCalls
	al2, _ := ownerLoop(t, script(providers.LLMResponse{Content: "plain"}))
	agent2 := al2.GetRegistry().GetDefaultAgent()
	if _, err := al2.runAgentLoop(context.Background(), agent2, processOptions{
		SessionKey: "sess-nil2", Channel: "cli", ChatID: "direct", UserMessage: "hi",
		DefaultResponse: "x", UsageSink: sc.sink,
	}); err != nil {
		t.Fatal(err)
	}
	if len(sc.got) != 1 || sc.got[0] != (providers.UsageInfo{}) {
		t.Fatalf("sink = %+v, want one zero call", sc.got)
	}
}
