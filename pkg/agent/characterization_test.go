package agent

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

// Characterization tests for the agent turn loop, asserted at the STABLE SEAM
// (the public ProcessDirect API → provider/tool fakes), NOT at internal
// functions. These behaviors must hold across any agent-core architecture, so
// this suite is the green→red→green guard for a future "rebase onto upstream's
// pipeline architecture" effort: it should keep compiling and pass on both the
// current monolithic loop.go and a future pipeline_*/adapters layout.
//
// What we deliberately do NOT assert here: internal types, function names, file
// layout, or event-bus/hook plumbing — those are expected to change.

// newCharLoop builds an AgentLoop wired to the given provider (the seam under
// test). Mirrors newTestAgentLoop but lets the test supply the LLM provider.
func newCharLoop(t *testing.T, provider providers.LLMProvider) (*AgentLoop, func()) {
	t.Helper()
	tmpDir, err := os.MkdirTemp("", "agent-char-*")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	cfg := &config.Config{
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				Workspace:         tmpDir,
				Model:             "char-model",
				MaxTokens:         4096,
				MaxToolIterations: 10,
			},
		},
	}
	al := NewAgentLoop(cfg, bus.NewMessageBus(), provider)
	return al, func() { os.RemoveAll(tmpDir) }
}

// charScriptedProvider returns a scripted sequence of LLM responses and records
// the messages + tool definitions it received on each Chat call, so tests can
// assert what the turn loop fed back to the model (history, tool results, system
// prompt) without reaching into internals.
type charScriptedProvider struct {
	responses []providers.LLMResponse
	calls     int
	gotMsgs   [][]providers.Message
	gotTools  [][]providers.ToolDefinition
}

func (m *charScriptedProvider) Chat(
	_ context.Context,
	messages []providers.Message,
	toolDefs []providers.ToolDefinition,
	_ string,
	_ map[string]any,
) (*providers.LLMResponse, error) {
	m.gotMsgs = append(m.gotMsgs, messages)
	m.gotTools = append(m.gotTools, toolDefs)
	idx := m.calls
	m.calls++
	if idx >= len(m.responses) {
		return &providers.LLMResponse{Content: "done"}, nil
	}
	r := m.responses[idx]
	return &r, nil
}

func (m *charScriptedProvider) GetDefaultModel() string { return "char-model" }

// charRecordingTool records the args it was invoked with and returns a marker
// the model is expected to see on the next turn.
type charRecordingTool struct {
	name      string
	gotArgs   []map[string]any
	llmOutput string
}

func (t *charRecordingTool) Name() string       { return t.name }
func (t *charRecordingTool) Description() string { return "characterization recording tool" }
func (t *charRecordingTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"value": map[string]any{"type": "string"},
		},
	}
}

func (t *charRecordingTool) Execute(_ context.Context, args map[string]any) *tools.ToolResult {
	t.gotArgs = append(t.gotArgs, args)
	return tools.SilentResult(t.llmOutput)
}

func anyMessageContains(msgs []providers.Message, substr string) bool {
	for _, m := range msgs {
		if strings.Contains(m.Content, substr) {
			return true
		}
		for _, p := range m.SystemParts {
			if strings.Contains(p.Text, substr) {
				return true
			}
		}
	}
	return false
}

// CHAR-1: a plain (no-tool) turn returns the model's content verbatim.
func TestCharacterization_PlainResponse(t *testing.T) {
	al, cleanup := newCharLoop(t, &charScriptedProvider{
		responses: []providers.LLMResponse{{Content: "hello world"}},
	})
	defer cleanup()

	got, err := al.ProcessDirect(context.Background(), "hi", "sess-plain")
	if err != nil {
		t.Fatalf("ProcessDirect: %v", err)
	}
	if got != "hello world" {
		t.Fatalf("response = %q, want %q", got, "hello world")
	}
}

// CHAR-2 (spike core): a tool-execution turn. The model requests a tool on call
// 1; the loop must (a) invoke the tool with the model's args, (b) feed the tool
// output back to the model, and (c) return the model's final content. This is
// the deepest core behavior and the one most affected by an architecture swap.
func TestCharacterization_ToolExecutionLoop(t *testing.T) {
	prov := &charScriptedProvider{responses: []providers.LLMResponse{
		{ // call 1: request the tool
			Content: "calling tool",
			ToolCalls: []providers.ToolCall{{
				ID:        "call_1",
				Type:      "function",
				Name:      "char_tool",
				Arguments: map[string]any{"value": "ping"},
			}},
		},
		{Content: "final answer"}, // call 2: terminal
	}}
	al, cleanup := newCharLoop(t, prov)
	defer cleanup()
	tool := &charRecordingTool{name: "char_tool", llmOutput: "TOOL_RESULT_MARKER_42"}
	al.RegisterTool(tool)

	got, err := al.ProcessDirect(context.Background(), "use the tool", "sess-tool")
	if err != nil {
		t.Fatalf("ProcessDirect: %v", err)
	}

	// (a) tool invoked exactly once, with the model's args
	if len(tool.gotArgs) != 1 {
		t.Fatalf("tool invoked %d times, want 1", len(tool.gotArgs))
	}
	if v, _ := tool.gotArgs[0]["value"].(string); v != "ping" {
		t.Fatalf("tool arg value = %q, want %q", v, "ping")
	}
	// (b) the model saw the tool output on a subsequent call
	if prov.calls < 2 {
		t.Fatalf("provider calls = %d, want >= 2 (tool round-trip)", prov.calls)
	}
	if !anyMessageContains(prov.gotMsgs[prov.calls-1], "TOOL_RESULT_MARKER_42") {
		t.Fatalf("final LLM call did not include the tool result in its messages")
	}
	// (c) final content returned
	if got != "final answer" {
		t.Fatalf("response = %q, want %q", got, "final answer")
	}
}

// CHAR-3: session continuity — a second turn on the same session key must feed
// the prior exchange back to the model (history persistence).
func TestCharacterization_SessionContinuity(t *testing.T) {
	prov := &charScriptedProvider{responses: []providers.LLMResponse{
		{Content: "first reply UNIQUE_ASSISTANT_TOKEN"},
		{Content: "second reply"},
	}}
	al, cleanup := newCharLoop(t, prov)
	defer cleanup()

	if _, err := al.ProcessDirect(context.Background(), "FIRST_USER_TOKEN", "sess-cont"); err != nil {
		t.Fatalf("ProcessDirect 1: %v", err)
	}
	if _, err := al.ProcessDirect(context.Background(), "second message", "sess-cont"); err != nil {
		t.Fatalf("ProcessDirect 2: %v", err)
	}
	if prov.calls < 2 {
		t.Fatalf("provider calls = %d, want >= 2", prov.calls)
	}
	second := prov.gotMsgs[prov.calls-1]
	if !anyMessageContains(second, "FIRST_USER_TOKEN") {
		t.Errorf("second turn missing prior user message (history not persisted)")
	}
	if !anyMessageContains(second, "UNIQUE_ASSISTANT_TOKEN") {
		t.Errorf("second turn missing prior assistant reply (history not persisted)")
	}
}

// newCharLoopMaxIter is like newCharLoop but sets MaxToolIterations, for the cap test.
func newCharLoopMaxIter(t *testing.T, provider providers.LLMProvider, maxIter int) (*AgentLoop, func()) {
	t.Helper()
	tmpDir, err := os.MkdirTemp("", "agent-char-*")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	cfg := &config.Config{
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				Workspace:         tmpDir,
				Model:             "char-model",
				MaxTokens:         4096,
				MaxToolIterations: maxIter,
			},
		},
	}
	al := NewAgentLoop(cfg, bus.NewMessageBus(), provider)
	return al, func() { os.RemoveAll(tmpDir) }
}

// alwaysToolProvider always requests a tool, never terminating — to exercise the
// iteration cap.
type alwaysToolProvider struct{ calls int }

func (m *alwaysToolProvider) Chat(_ context.Context, _ []providers.Message, _ []providers.ToolDefinition, _ string, _ map[string]any) (*providers.LLMResponse, error) {
	m.calls++
	return &providers.LLMResponse{
		Content: "again",
		ToolCalls: []providers.ToolCall{{
			ID: "c", Type: "function", Name: "char_tool", Arguments: map[string]any{"value": "x"},
		}},
	}, nil
}
func (m *alwaysToolProvider) GetDefaultModel() string { return "char-model" }

// CHAR-5: the tool-iteration cap is enforced — a model stuck calling tools is
// stopped at MaxToolIterations and gets the documented limit response, not an
// infinite loop.
func TestCharacterization_ToolIterationCap(t *testing.T) {
	prov := &alwaysToolProvider{}
	al, cleanup := newCharLoopMaxIter(t, prov, 3)
	defer cleanup()
	tool := &charRecordingTool{name: "char_tool", llmOutput: "ok"}
	al.RegisterTool(tool)

	got, err := al.ProcessDirect(context.Background(), "loop forever", "sess-cap")
	if err != nil {
		t.Fatalf("ProcessDirect: %v", err)
	}
	if len(tool.gotArgs) > 3 {
		t.Fatalf("tool invoked %d times, want <= cap 3", len(tool.gotArgs))
	}
	if got != toolLimitResponse {
		t.Fatalf("response = %q, want toolLimitResponse", got)
	}
}

// CHAR-6: NoHistory turns are isolated — they do not carry prior history.
func TestCharacterization_NoHistoryIsolation(t *testing.T) {
	prov := &charScriptedProvider{responses: []providers.LLMResponse{
		{Content: "r1"}, {Content: "r2"},
	}}
	al, cleanup := newCharLoop(t, prov)
	defer cleanup()

	if _, err := al.ProcessDirectWithChannel(context.Background(), "FIRST_NOHIST_TOKEN", "sess-nh", "cli", "direct", true); err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	if _, err := al.ProcessDirectWithChannel(context.Background(), "second", "sess-nh", "cli", "direct", true); err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	if anyMessageContains(prov.gotMsgs[prov.calls-1], "FIRST_NOHIST_TOKEN") {
		t.Errorf("NoHistory turn leaked prior message into a later turn")
	}
}

// CHAR-7: multiple tool calls in a single model response are all executed and
// all results fed back.
func TestCharacterization_MultipleToolCalls(t *testing.T) {
	prov := &charScriptedProvider{responses: []providers.LLMResponse{
		{
			Content: "two tools",
			ToolCalls: []providers.ToolCall{
				{ID: "a", Type: "function", Name: "char_tool", Arguments: map[string]any{"value": "one"}},
				{ID: "b", Type: "function", Name: "char_tool", Arguments: map[string]any{"value": "two"}},
			},
		},
		{Content: "done"},
	}}
	al, cleanup := newCharLoop(t, prov)
	defer cleanup()
	tool := &charRecordingTool{name: "char_tool", llmOutput: "okk"}
	al.RegisterTool(tool)

	if _, err := al.ProcessDirect(context.Background(), "do both", "sess-multi"); err != nil {
		t.Fatalf("ProcessDirect: %v", err)
	}
	if len(tool.gotArgs) != 2 {
		t.Fatalf("tool invoked %d times, want 2 (one per tool call)", len(tool.gotArgs))
	}
}

// CHAR-8: a tool error is surfaced to the model (not swallowed), so it can
// recover — the error content reaches the next LLM call and the turn completes.
func TestCharacterization_ToolErrorPropagation(t *testing.T) {
	prov := &charScriptedProvider{responses: []providers.LLMResponse{
		{
			Content:   "calling failing tool",
			ToolCalls: []providers.ToolCall{{ID: "e", Type: "function", Name: "boom_tool", Arguments: map[string]any{}}},
		},
		{Content: "recovered"},
	}}
	al, cleanup := newCharLoop(t, prov)
	defer cleanup()
	al.RegisterTool(&charErrorTool{})

	got, err := al.ProcessDirect(context.Background(), "fail please", "sess-err")
	if err != nil {
		t.Fatalf("ProcessDirect: %v", err)
	}
	if prov.calls < 2 || !anyMessageContains(prov.gotMsgs[prov.calls-1], "BOOM_ERR") {
		t.Fatalf("tool error not surfaced to the model on the next turn")
	}
	if got != "recovered" {
		t.Fatalf("response = %q, want %q", got, "recovered")
	}
}

type charErrorTool struct{}

func (t *charErrorTool) Name() string               { return "boom_tool" }
func (t *charErrorTool) Description() string         { return "always errors" }
func (t *charErrorTool) Parameters() map[string]any { return map[string]any{"type": "object"} }
func (t *charErrorTool) Execute(_ context.Context, _ map[string]any) *tools.ToolResult {
	return tools.ErrorResult("BOOM_ERR")
}

// blockingProvider blocks on its first call until the context is cancelled,
// signalling when it has started so the test can cancel deterministically.
type blockingProvider struct{ started chan struct{} }

func (m *blockingProvider) Chat(ctx context.Context, _ []providers.Message, _ []providers.ToolDefinition, _ string, _ map[string]any) (*providers.LLMResponse, error) {
	close(m.started)
	<-ctx.Done() // block until the turn's context is cancelled
	return nil, ctx.Err()
}
func (m *blockingProvider) GetDefaultModel() string { return "char-model" }

// CHAR-9: a turn respects context cancellation — the seam-level mechanism that
// hard-abort/interrupt relies on. Cancelling the ctx unblocks a stuck turn
// promptly instead of hanging.
func TestCharacterization_ContextCancellationAbortsTurn(t *testing.T) {
	prov := &blockingProvider{started: make(chan struct{})}
	al, cleanup := newCharLoop(t, prov)
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_, _ = al.ProcessDirect(ctx, "hang", "sess-cancel")
		close(done)
	}()

	<-prov.started // turn is now blocked inside the provider call
	cancel()

	select {
	case <-done: // returned promptly after cancellation — good
	case <-time.After(5 * time.Second):
		t.Fatal("turn did not return after context cancellation (cancellation not propagated)")
	}
}

// CHAR-4: the model receives a non-empty system prompt / instructions.
func TestCharacterization_SystemPromptPresent(t *testing.T) {
	prov := &charScriptedProvider{responses: []providers.LLMResponse{{Content: "ok"}}}
	al, cleanup := newCharLoop(t, prov)
	defer cleanup()

	if _, err := al.ProcessDirect(context.Background(), "hi", "sess-sys"); err != nil {
		t.Fatalf("ProcessDirect: %v", err)
	}
	if prov.calls == 0 {
		t.Fatal("provider never called")
	}
	hasSystem := false
	for _, m := range prov.gotMsgs[0] {
		if m.Role == "system" || len(m.SystemParts) > 0 {
			hasSystem = true
			break
		}
	}
	if !hasSystem {
		t.Errorf("first LLM call had no system message/parts (system prompt not delivered)")
	}
}
