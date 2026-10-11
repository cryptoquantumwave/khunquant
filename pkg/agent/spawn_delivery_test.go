package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/providers"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

// Tests for what becomes of the result of a task started with the spawn tool:
// it runs to completion whether or not the turn that started it is still
// running, reaches the user exactly once, and a failure is reported.
//
// They swap package-level state (MockEventBus.Emit, the logger's writer), so
// none of them may run in parallel.

// ====================== scaffolding ======================

// spawnLogBuffer collects what the logger writes while a test runs.
type spawnLogBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *spawnLogBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *spawnLogBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func captureLogs(t *testing.T) *spawnLogBuffer {
	t.Helper()
	buf := &spawnLogBuffer{}
	logger.SetAdditionalWriter(buf)
	t.Cleanup(func() { logger.SetAdditionalWriter(nil) })
	return buf
}

// logLine returns the first logged line containing substr, or "".
func logLine(l *spawnLogBuffer, substr string) string {
	for _, line := range strings.Split(l.String(), "\n") {
		if strings.Contains(line, substr) {
			return line
		}
	}
	return ""
}

// watchSubTurnEvents records sub-turn events and signals each sub-turn's end.
type subTurnWatch struct {
	mu      sync.Mutex
	spawned []SubTurnSpawnEvent
	ended   *signal
}

func watchSubTurnEvents(t *testing.T) *subTurnWatch {
	t.Helper()
	w := &subTurnWatch{ended: newSignal()}
	original := MockEventBus.Emit
	MockEventBus.Emit = func(e any) {
		switch ev := e.(type) {
		case SubTurnSpawnEvent:
			w.mu.Lock()
			w.spawned = append(w.spawned, ev)
			w.mu.Unlock()
		case SubTurnEndEvent:
			w.ended.fire()
		}
	}
	t.Cleanup(func() { MockEventBus.Emit = original })
	return w
}

func nextInbound(t *testing.T, msgBus *bus.MessageBus) bus.InboundMessage {
	t.Helper()
	select {
	case msg := <-msgBus.InboundChan():
		return msg
	case <-time.After(testWait):
		t.Fatal("timed out waiting for an inbound message")
		return bus.InboundMessage{}
	}
}

func noInbound(t *testing.T, msgBus *bus.MessageBus) {
	t.Helper()
	select {
	case msg := <-msgBus.InboundChan():
		t.Fatalf("unexpected inbound message: %+v", msg)
	default:
	}
}

func noOutbound(t *testing.T, msgBus *bus.MessageBus) {
	t.Helper()
	select {
	case msg := <-msgBus.OutboundChan():
		t.Fatalf("unexpected outbound message: %+v", msg)
	default:
	}
}

// spawnParent scripts a parent turn that spawns one task and then ends.
func spawnParent(args map[string]any) turnHandler {
	return func(_ context.Context, call int, _ []providers.Message) (*providers.LLMResponse, error) {
		if call == 1 {
			return toolCallResponse(tools.NameSpawn, args)
		}
		return textResponse("parent done")
	}
}

// ====================== A: runs to completion ======================

// The parent turn ends while the child still has iterations to go. The child
// has to go on and deliver its final answer; it used to stop at its next
// iteration with nothing to say.
func TestSpawn_ChildCompletesAfterParentFinishes(t *testing.T) {
	release := newSignal()
	var toolRuns atomic.Int32

	prov := newRoutedProvider().
		on("PARENT_TURN", spawnParent(map[string]any{"task": "CHILD_JOB"})).
		on("CHILD_JOB", func(ctx context.Context, call int, _ []providers.Message) (*providers.LLMResponse, error) {
			switch call {
			case 1:
				select {
				case <-release.fired(): // the parent has returned
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				return toolCallResponse("child_step", map[string]any{})
			case 2:
				return toolCallResponse("child_step", map[string]any{})
			default:
				return textResponse("CHILD_FINAL")
			}
		})
	al, msgBus := newTurnTestLoop(t, prov, true)
	al.RegisterTool(&funcTool{name: "child_step", fn: func(context.Context, map[string]any) *tools.ToolResult {
		toolRuns.Add(1)
		return tools.SilentResult("step done")
	}})

	res := awaitTurn(t, "the parent turn", startTurn(context.Background(), al, "PARENT_TURN", "agent:main:spawn-completes"))
	if res.err != nil || res.response != "parent done" {
		t.Fatalf("parent turn = (%q, %v), want (\"parent done\", nil)", res.response, res.err)
	}
	release.fire()

	msg := nextInbound(t, msgBus)
	if msg.Channel != "system" || msg.Content != "CHILD_FINAL" {
		t.Fatalf("delivered %+v, want the child's final content as a system message", msg)
	}
	if n := toolRuns.Load(); n != 2 {
		t.Fatalf("the child ran %d tool step(s) after its parent ended, want 2", n)
	}
}

// An explicit cancel still stops a spawned task, however critical: it is the
// only thing that does besides the task's own timeout.
func TestSpawn_ExplicitCancelStillStopsCriticalChild(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cancel func(al *AgentLoop, session string) int
	}{
		{"CancelAllTurns", func(al *AgentLoop, _ string) int { return al.CancelAllTurns("test") }},
		{"CancelTurnsForSession", func(al *AgentLoop, session string) int { return al.CancelTurnsForSession(session) }},
		{"HardAbort", func(al *AgentLoop, session string) int {
			if err := al.HardAbort(session); err != nil {
				return 0
			}
			return 1
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const session = "agent:main:spawn-cancel"
			watch := watchSubTurnEvents(t)
			release := newSignal()
			childBlocked := newSignal()
			childErr := make(chan error, 1)

			prov := newRoutedProvider().
				on("PARENT_TURN", spawnParent(map[string]any{"task": "CHILD_JOB"})).
				on("CHILD_JOB", func(ctx context.Context, call int, _ []providers.Message) (*providers.LLMResponse, error) {
					if call == 1 {
						select {
						case <-release.fired():
						case <-ctx.Done():
							return nil, ctx.Err()
						}
						return toolCallResponse("child_step", map[string]any{})
					}
					// Past an iteration boundary after the parent ended: the
					// child is running on because it is critical.
					childBlocked.fire()
					<-ctx.Done()
					childErr <- ctx.Err()
					return nil, ctx.Err()
				})
			al, msgBus := newTurnTestLoop(t, prov, true)
			al.RegisterTool(&funcTool{name: "child_step", fn: func(context.Context, map[string]any) *tools.ToolResult {
				return tools.SilentResult("step done")
			}})

			res := awaitTurn(t, "the parent turn", startTurn(context.Background(), al, "PARENT_TURN", session))
			if res.err != nil || res.response != "parent done" {
				t.Fatalf("parent turn = (%q, %v), want (\"parent done\", nil)", res.response, res.err)
			}
			release.fire()
			childBlocked.wait(t, "the child to carry on after its parent ended")

			watch.mu.Lock()
			if len(watch.spawned) != 1 || !watch.spawned[0].Config.Critical {
				t.Errorf("sub-turns spawned = %+v, want one critical sub-turn", watch.spawned)
			}
			watch.mu.Unlock()

			if n := tc.cancel(al, session); n != 1 {
				t.Errorf("the cancel stopped %d turn(s), want 1", n)
			}
			select {
			case err := <-childErr:
				if !errors.Is(err, context.Canceled) {
					t.Errorf("child's context ended with %v, want context.Canceled", err)
				}
			case <-time.After(testWait):
				t.Fatal("the cancel did not stop the critical child")
			}
			awaitSpawnFailureNotice(t, msgBus)
			noInbound(t, msgBus)
		})
	}
}

// ====================== B: single delivery ======================

// One spawn, one message to the user. The result used to be queued on the
// parent turn as well, and appended to the parent's own final response.
func TestSpawn_SingleDelivery(t *testing.T) {
	watch := watchSubTurnEvents(t)

	prov := newRoutedProvider().
		on("PARENT_TURN", func(ctx context.Context, call int, _ []providers.Message) (*providers.LLMResponse, error) {
			if call == 1 {
				return toolCallResponse(tools.NameSpawn, map[string]any{"task": "CHILD_JOB"})
			}
			// Answer only once the child has finished, so that its result is
			// there for the parent's final poll if anything queued it.
			select {
			case <-watch.ended.fired():
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return textResponse("parent done")
		}).
		on("CHILD_JOB", func(context.Context, int, []providers.Message) (*providers.LLMResponse, error) {
			return textResponse("CHILD_RESULT")
		})
	al, msgBus := newTurnTestLoop(t, prov, true)

	res := awaitTurn(t, "the parent turn", startTurn(context.Background(), al, "PARENT_TURN", "agent:main:spawn-single"))
	if res.err != nil || res.response != "parent done" {
		t.Fatalf("parent turn = (%q, %v), want (\"parent done\", nil): the spawn's result leaked into it",
			res.response, res.err)
	}

	msg := nextInbound(t, msgBus)
	if msg.Channel != "system" || msg.Content != "CHILD_RESULT" {
		t.Fatalf("delivered %+v, want the result as a system message", msg)
	}
	noInbound(t, msgBus)
	noOutbound(t, msgBus)
}

// ====================== C: callback semantics ======================

func TestSpawn_FailureReported(t *testing.T) {
	childErr := errors.New("model exploded")
	prov := newRoutedProvider().
		on("PARENT_TURN", spawnParent(map[string]any{"task": "CHILD_JOB", "label": "nightly-news"})).
		on("CHILD_JOB", func(context.Context, int, []providers.Message) (*providers.LLMResponse, error) {
			return nil, childErr
		})
	al, msgBus := newTurnTestLoop(t, prov, true)

	res := awaitTurn(t, "the parent turn", startTurn(context.Background(), al, "PARENT_TURN", "agent:main:spawn-failure"))
	if res.err != nil {
		t.Fatalf("parent turn failed: %v", res.err)
	}

	select {
	case msg := <-msgBus.OutboundChan():
		if msg.Channel != "telegram" || msg.ChatID != "chat-1" {
			t.Errorf("failure sent to %s:%s, want the origin telegram:chat-1", msg.Channel, msg.ChatID)
		}
		const prefix = "⚠️ Background task 'nightly-news' failed: "
		if !strings.HasPrefix(msg.Content, prefix) || !strings.Contains(msg.Content, "model exploded") {
			t.Errorf("failure notice = %q, want %q followed by the error", msg.Content, prefix)
		}
	case <-time.After(testWait):
		t.Fatal("a failed spawn was never reported")
	}
	noInbound(t, msgBus)
	noOutbound(t, msgBus)
}

// A task that ends with nothing to say publishes nothing; it is logged.
func TestSpawn_EmptyOutputLogged(t *testing.T) {
	logs := captureLogs(t)
	prov := newRoutedProvider().
		on("PARENT_TURN", spawnParent(map[string]any{"task": "CHILD_JOB", "label": "quiet-task"})).
		on("CHILD_JOB", func(context.Context, int, []providers.Message) (*providers.LLMResponse, error) {
			return textResponse("")
		})
	al, msgBus := newTurnTestLoop(t, prov, true)

	awaitTurn(t, "the parent turn", startTurn(context.Background(), al, "PARENT_TURN", "agent:main:spawn-empty"))

	const msg = "subturn completed with no output"
	waitUntil(t, "the empty result to be logged", func() bool { return logLine(logs, msg) != "" })
	// The tool call itself is logged with the label too, so check the line.
	if line := logLine(logs, msg); !strings.Contains(line, "WRN") || !strings.Contains(line, "quiet-task") {
		t.Errorf("the warning is not a WARN naming the task: %s", line)
	}
	noInbound(t, msgBus)
	noOutbound(t, msgBus)
}

// A result whose origin is internal (a cli turn) has nobody to go to. It used
// to be dropped with an Info line; it is now a warning that names the task.
func TestSpawn_InternalOriginLoggedNotDropped(t *testing.T) {
	for _, tc := range []struct {
		name    string
		child   turnHandler
		outcome string
	}{
		{"completed", func(context.Context, int, []providers.Message) (*providers.LLMResponse, error) {
			return textResponse("CHILD_RESULT")
		}, "completed"},
		{"failed", func(context.Context, int, []providers.Message) (*providers.LLMResponse, error) {
			return nil, errors.New("model exploded")
		}, "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLogs(t)
			prov := newRoutedProvider().
				on("PARENT_TURN", spawnParent(map[string]any{"task": "CHILD_JOB", "label": "cron-task"})).
				on("CHILD_JOB", tc.child)
			al, msgBus := newTurnTestLoop(t, prov, true)

			_, err := al.ProcessDirectWithChannel(context.Background(), "PARENT_TURN", "agent:main:spawn-internal", "cli", "direct", false)
			if err != nil {
				t.Fatalf("parent turn failed: %v", err)
			}

			waitUntil(t, "the internal-origin result to be logged", func() bool {
				return logLine(logs, "origin is internal") != ""
			})
			line := logLine(logs, "origin is internal")
			for _, want := range []string{"WRN", "cron-task", tc.outcome} {
				if !strings.Contains(line, want) {
					t.Errorf("the warning lacks %q: %s", want, line)
				}
			}
			noInbound(t, msgBus)
			noOutbound(t, msgBus)
		})
	}
}

// ====================== acceptance: heartbeat ======================

// A heartbeat turn spawns a three-iteration task and ends. The task's result
// reaches the user once, and not through the heartbeat's own response.
func TestSpawn_HeartbeatDeliversOnce(t *testing.T) {
	released := newSignal()
	prov := newRoutedProvider().
		on("HEARTBEAT_TURN", spawnParent(map[string]any{"task": "CHILD_JOB"})).
		on("CHILD_JOB", func(ctx context.Context, call int, _ []providers.Message) (*providers.LLMResponse, error) {
			switch call {
			case 1:
				select {
				case <-released.fired(): // the heartbeat turn has ended
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				return toolCallResponse("child_step", map[string]any{})
			case 2:
				return toolCallResponse("child_step", map[string]any{})
			default:
				return textResponse("NEWS_DIGEST")
			}
		})
	al, msgBus := newTurnTestLoop(t, prov, true)
	al.RegisterTool(&funcTool{name: "child_step", fn: func(context.Context, map[string]any) *tools.ToolResult {
		return tools.SilentResult("step done")
	}})

	response, err := al.ProcessHeartbeat(context.Background(), "HEARTBEAT_TURN", "telegram", "chat-1")
	if err != nil || response != "parent done" {
		t.Fatalf("heartbeat turn = (%q, %v), want (\"parent done\", nil)", response, err)
	}
	released.fire()

	msg := nextInbound(t, msgBus)
	if msg.Channel != "system" || msg.ChatID != "telegram:chat-1" || msg.Content != "NEWS_DIGEST" {
		t.Fatalf("delivered %+v, want NEWS_DIGEST for telegram:chat-1", msg)
	}
	noInbound(t, msgBus)
}

// ====================== D, E: target agent, model, temperature ======================

// agentProbe is an LLM that answers every prompt with a fixed text, optionally
// calling a tool first, and records what each call was made with.
type agentProbe struct {
	mu    sync.Mutex
	calls []probeCall
	// toolOnFirst, when set, is called on the first Chat of each prompt token.
	script func(prompt string, call int) *providers.LLMResponse
}

type probeCall struct {
	prompt      string
	model       string
	temperature any
	toolNames   []string
	messages    []providers.Message
}

func (p *agentProbe) Chat(
	_ context.Context,
	msgs []providers.Message,
	defs []providers.ToolDefinition,
	model string,
	options map[string]any,
) (*providers.LLMResponse, error) {
	c := probeCall{prompt: lastUserContent(msgs), model: model, temperature: options["temperature"], messages: msgs}
	for _, d := range defs {
		c.toolNames = append(c.toolNames, d.Function.Name)
	}
	p.mu.Lock()
	p.calls = append(p.calls, c)
	n := 0
	for _, prev := range p.calls {
		if prev.prompt == c.prompt {
			n++
		}
	}
	p.mu.Unlock()
	if p.script != nil {
		if r := p.script(c.prompt, n); r != nil {
			return r, nil
		}
	}
	return &providers.LLMResponse{Content: "ok"}, nil
}

func (p *agentProbe) GetDefaultModel() string { return "char-model" }

func (p *agentProbe) callsFor(token string) []probeCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []probeCall
	for _, c := range p.calls {
		if strings.Contains(c.prompt, token) {
			out = append(out, c)
		}
	}
	return out
}

func hasName(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// newSpawnTwoAgentLoop builds a loop with a default agent "main" that may spawn
// "worker", which has a model and a tool of its own.
func newSpawnTwoAgentLoop(t *testing.T, provider providers.LLMProvider) (*AgentLoop, *bus.MessageBus) {
	t.Helper()
	cfg := &config.Config{Agents: config.AgentsConfig{
		Defaults: config.AgentDefaults{
			Workspace: t.TempDir(), Model: "char-model", MaxTokens: 4096, MaxToolIterations: 10,
		},
		List: []config.AgentConfig{
			{ID: "main", Default: true, Subagents: &config.SubagentsConfig{AllowAgents: []string{"worker"}}},
			{ID: "worker", Model: &config.AgentModelConfig{Primary: "worker-model"}},
		},
	}}
	cfg.Tools.Spawn.Enabled = true
	cfg.Tools.Subagent.Enabled = true
	msgBus := bus.NewMessageBus()
	al := NewAgentLoop(cfg, msgBus, provider)
	t.Cleanup(al.Close)
	return al, msgBus
}

// spawn with an agent_id runs the task as that agent: its model, its tools.
func TestSpawn_RunsAsTargetAgent(t *testing.T) {
	prov := &agentProbe{script: func(prompt string, call int) *providers.LLMResponse {
		if strings.Contains(prompt, "PARENT_TURN") && call == 1 {
			return &providers.LLMResponse{ToolCalls: []providers.ToolCall{{
				ID: "c1", Type: "function", Name: tools.NameSpawn,
				Arguments: map[string]any{"task": "CHILD_JOB", "agent_id": "worker"},
			}}}
		}
		if strings.Contains(prompt, "CHILD_JOB") {
			return &providers.LLMResponse{Content: "CHILD_RESULT"}
		}
		return nil
	}}
	al, msgBus := newSpawnTwoAgentLoop(t, prov)
	mainAgent, _ := al.GetRegistry().GetAgent("main")
	worker, _ := al.GetRegistry().GetAgent("worker")
	mainAgent.Tools.Register(&funcTool{name: "main_only", fn: nil})
	worker.Tools.Register(&funcTool{name: "worker_only", fn: nil})

	if _, err := al.ProcessDirectWithChannel(context.Background(), "PARENT_TURN", "agent:main:spawn-target", "telegram", "chat-1", false); err != nil {
		t.Fatalf("parent turn failed: %v", err)
	}
	if msg := nextInbound(t, msgBus); msg.Content != "CHILD_RESULT" {
		t.Fatalf("delivered %+v, want the child's result", msg)
	}

	child := prov.callsFor("CHILD_JOB")
	if len(child) == 0 {
		t.Fatal("the task never ran")
	}
	if child[0].model != "worker-model" {
		t.Errorf("task ran on model %q, want the target agent's worker-model", child[0].model)
	}
	if !hasName(child[0].toolNames, "worker_only") || hasName(child[0].toolNames, "main_only") {
		t.Errorf("task had tools %v, want the target agent's (worker_only) and not the spawner's (main_only)", child[0].toolNames)
	}
}

// delegate names an agent and nothing else: the model, the tools and the
// temperature all have to come from that agent.
func TestDelegate_ResolvesModelAndTarget(t *testing.T) {
	prov := &agentProbe{}
	al, _ := newSpawnTwoAgentLoop(t, prov)
	mainAgent, _ := al.GetRegistry().GetAgent("main")
	worker, _ := al.GetRegistry().GetAgent("worker")
	mainAgent.Temperature = 0.1
	worker.Temperature = 0.35
	worker.Tools.Register(&funcTool{name: "worker_only", fn: nil})

	delegate := tools.NewDelegateTool()
	delegate.SetSpawner(NewSubTurnSpawner(al, "main"))
	res := delegate.Execute(spawnCtx(), map[string]any{"agent_id": "worker", "task": "DELEGATED_JOB"})
	if res.IsError {
		t.Fatalf("delegate failed: %s", res.ForLLM)
	}

	calls := prov.callsFor("DELEGATED_JOB")
	if len(calls) == 0 {
		t.Fatal("the delegated task never ran")
	}
	if calls[0].model != "worker-model" {
		t.Errorf("model = %q, want the target agent's worker-model", calls[0].model)
	}
	if calls[0].temperature != 0.35 {
		t.Errorf("temperature = %v, want the target agent's 0.35", calls[0].temperature)
	}
	if !hasName(calls[0].toolNames, "worker_only") {
		t.Errorf("tools = %v, want the target agent's worker_only", calls[0].toolNames)
	}

	// An explicit temperature (the subagent tool passes one) beats the
	// agent's own.
	if _, err := NewSubTurnSpawner(al, "main").SpawnSubTurn(spawnCtx(), tools.SubTurnConfig{
		SystemPrompt: "EXPLICIT_JOB", TargetAgentID: "worker", Temperature: 0.8,
	}); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	explicit := prov.callsFor("EXPLICIT_JOB")
	if len(explicit) == 0 || explicit[0].temperature != 0.8 || explicit[0].model != "worker-model" {
		t.Errorf("explicit-temperature call = %+v, want temperature 0.8 on worker-model", explicit)
	}
}

// ====================== F: children cannot spawn ======================

// A child is built without spawn, subagent and delegate, however its tools
// were chosen, so the tree is one level deep. A child that asks for one gets
// the ordinary answer for a tool that does not exist.
func TestChildRegistry_NoSpawn(t *testing.T) {
	spawning := []string{tools.NameSpawn, "subagent", tools.NameDelegate}

	run := func(t *testing.T, explicit func(al *AgentLoop) []tools.Tool) {
		prov := &agentProbe{script: func(prompt string, call int) *providers.LLMResponse {
			if strings.Contains(prompt, "CHILD_JOB") && call == 1 {
				return &providers.LLMResponse{ToolCalls: []providers.ToolCall{{
					ID: "c1", Type: "function", Name: tools.NameSpawn,
					Arguments: map[string]any{"task": "GRANDCHILD_JOB"},
				}}}
			}
			return nil
		}}
		al, _ := newSpawnTwoAgentLoop(t, prov)
		mainAgent, _ := al.GetRegistry().GetAgent("main")
		for _, name := range spawning {
			if _, ok := mainAgent.Tools.Get(name); !ok {
				t.Fatalf("fixture broken: the parent agent has no %s tool", name)
			}
		}
		mainAgent.Tools.Register(&funcTool{name: "ordinary_tool", fn: nil})

		cfg := tools.SubTurnConfig{Model: "char-model", SystemPrompt: "CHILD_JOB"}
		if explicit != nil {
			cfg.Tools = explicit(al)
		}
		if _, err := NewSubTurnSpawner(al, "main").SpawnSubTurn(spawnCtx(), cfg); err != nil {
			t.Fatalf("spawn: %v", err)
		}

		calls := prov.callsFor("CHILD_JOB")
		if len(calls) != 2 {
			t.Fatalf("child made %d LLM call(s), want 2 (the refused call, then its answer)", len(calls))
		}
		if !hasName(calls[0].toolNames, "ordinary_tool") {
			t.Fatalf("child tools = %v, want its ordinary tool; the test would prove nothing", calls[0].toolNames)
		}
		for _, name := range spawning {
			if hasName(calls[0].toolNames, name) {
				t.Errorf("child was offered %q", name)
			}
		}
		// The refused call came back as an error result, and nothing ran.
		var refusal string
		for _, m := range calls[1].messages {
			if m.Role == "tool" {
				refusal = m.Content
			}
		}
		if !strings.Contains(refusal, "not found") {
			t.Errorf("child's spawn call got %q, want a tool-not-found result", refusal)
		}
		if got := prov.callsFor("GRANDCHILD_JOB"); len(got) != 0 {
			t.Errorf("the child started a grandchild (%d LLM call(s))", len(got))
		}
	}

	t.Run("tools inherited from the agent", func(t *testing.T) { run(t, nil) })
	t.Run("tools given explicitly", func(t *testing.T) {
		run(t, func(al *AgentLoop) []tools.Tool {
			mainAgent, _ := al.GetRegistry().GetAgent("main")
			return mainAgent.Tools.GetAll()
		})
	})
}
