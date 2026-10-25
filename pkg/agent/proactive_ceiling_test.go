package agent

import (
	"context"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

// stubTool keeps a real tool's name and nothing else: an empty-object schema, so
// calling it with no arguments reaches the policy check instead of failing
// argument validation first, and a counter that says whether it ran.
type stubTool struct {
	name string
	mu   *sync.Mutex
	ran  map[string]int
}

func (s stubTool) Name() string        { return s.name }
func (s stubTool) Description() string { return "stub " + s.name }
func (s stubTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

func (s stubTool) Execute(context.Context, map[string]any) *tools.ToolResult {
	s.mu.Lock()
	s.ran[s.name]++
	s.mu.Unlock()
	return tools.SilentResult("ran")
}

// mayCallInProactiveTurn is the ceiling written out independently of the code
// under test (turnMayCall is unexported): every read tool and respond_proactive,
// plus take_snapshot for the legacy heartbeat kind.
func mayCallInProactiveTurn(name string, kind proactive.TurnKind) bool {
	switch {
	case name == tools.NameRespondProactive:
		return true
	case name == tools.NameTakeSnapshot && kind == proactive.TurnHeartbeatLegacy:
		return true
	case name == tools.NameToolSearchRegex || name == tools.NameToolSearchBM25:
		return false // built over the shared registry, not the turn's clone
	case name == tools.NameWebFetch:
		return kind == proactive.TurnHeartbeatLegacy // a model-chosen URL
	}
	return tools.ClassOf(name) == tools.ClassRead
}

func sortedAllowed(names []string, kind proactive.TurnKind) []string {
	var out []string
	for _, n := range names {
		if mayCallInProactiveTurn(n, kind) {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// TestProactiveCeiling_EveryRegisteredTool is the ceiling of a proactive turn
// checked over the real registry, for both turn kinds: for EVERY registered tool
// the definition is shown exactly when the turn may call it, and every tool the
// turn may not call is refused at execution without running.
func TestProactiveCeiling_EveryRegisteredTool(t *testing.T) {
	cfg := &config.Config{Agents: config.AgentsConfig{Defaults: config.AgentDefaults{
		Workspace: t.TempDir(), Model: "char-model", MaxTokens: 4096, MaxToolIterations: 10,
	}}}
	enableEveryTool(cfg)
	al := NewAgentLoop(cfg, bus.NewMessageBus(), &charScriptedProvider{})
	t.Cleanup(al.Close)

	// The turn's registry: the agent's, cloned, with respond_proactive added.
	real := al.GetRegistry().GetDefaultAgent().Tools.Clone()
	real.Register(tools.NewRespondProactiveTool(nil, false))
	registered := real.List()
	if len(registered) < 60 {
		t.Fatalf("only %d tools registered; the fixture is not enabling them", len(registered))
	}

	// Names the real registry may not hold in this fixture, plus ones nothing
	// classifies (what an MCP tool looks like).
	named := []string{
		tools.NameMessage, tools.NameWriteFile, tools.NameEditFile, tools.NameSpawn,
		tools.NameExecuteDCAOrder, tools.NameCreateOrder, tools.NameFuturesClosePosition,
		tools.NameEmergencyStop, tools.NameUpdateProfile, tools.NameCron, tools.NameExec,
		tools.NameSendFile, tools.NameTakeSnapshot, tools.NameWebFetch, "mcp_github_create_issue",
	}
	all := append([]string(nil), registered...)
	for _, n := range named {
		if !hasName(all, n) {
			all = append(all, n)
		}
	}
	sort.Strings(all)

	for _, kind := range []proactive.TurnKind{proactive.TurnProactive, proactive.TurnHeartbeatLegacy} {
		t.Run(string(kind), func(t *testing.T) {
			ctx := tools.WithProactiveTurn(context.Background(), kind)

			// (a) definitions on the real registry equal the ceiling.
			shown := defNames(real.ToProviderDefsFor(ctx))
			sort.Strings(shown)
			if want := sortedAllowed(registered, kind); !equalStrings(shown, want) {
				t.Errorf("real registry shows %v\nwant %v", shown, want)
			}
			if !hasName(shown, tools.NameRespondProactive) {
				t.Error("respond_proactive is not shown")
			}

			// (b) and (a) again on stubs, which can be executed without arguments.
			var mu sync.Mutex
			ran := map[string]int{}
			stubs := tools.NewToolRegistry()
			for _, n := range all {
				stubs.Register(stubTool{name: n, mu: &mu, ran: ran})
			}
			stubShown := defNames(stubs.ToProviderDefsFor(ctx))
			sort.Strings(stubShown)
			if want := sortedAllowed(all, kind); !equalStrings(stubShown, want) {
				t.Errorf("stub registry shows %v\nwant %v", stubShown, want)
			}

			for _, n := range all {
				res := stubs.ExecuteWithContext(ctx, n, nil, "telegram", "424242", nil)
				mu.Lock()
				count := ran[n]
				mu.Unlock()
				if mayCallInProactiveTurn(n, kind) {
					if res.IsError || count != 1 {
						t.Errorf("%s (%s) should run: isError=%v ran=%d %q", n, tools.ClassOf(n), res.IsError, count, res.ForLLM)
					}
					continue
				}
				if !res.IsError || count != 0 {
					t.Errorf("%s (%s) must be refused: isError=%v ran=%d", n, tools.ClassOf(n), res.IsError, count)
				}
				if !strings.Contains(res.ForLLM, "not permitted in an autonomous (proactive) turn") {
					t.Errorf("%s refusal = %q", n, res.ForLLM)
				}
			}

			// The well-known dangerous ones, by name, so a classification edit
			// that quietly moved one cannot hide behind the table above.
			for _, n := range named {
				if (n == tools.NameTakeSnapshot || n == tools.NameWebFetch) && kind == proactive.TurnHeartbeatLegacy {
					continue
				}
				if hasName(shown, n) || hasName(stubShown, n) {
					t.Errorf("%s is shown to a %s turn", n, kind)
				}
			}
		})
	}
}
