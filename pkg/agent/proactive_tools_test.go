package agent

import (
	"context"
	"encoding/json"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/providers"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

var silentDecision = map[string]any{"decision": "silent"}

func sortedDefNames(defs []providers.ToolDefinition) []string {
	names := defNames(defs)
	sort.Strings(names)
	return names
}

// toolResultFor returns the content of the tool message answering callID in the
// provider's n-th call.
func toolResultFor(t *testing.T, prov *scriptProvider, n int, callID string) string {
	t.Helper()
	prov.mu.Lock()
	defer prov.mu.Unlock()
	for _, m := range prov.msgs[n] {
		if m.Role == "tool" && m.ToolCallID == callID {
			return m.Content
		}
	}
	t.Fatalf("no tool result for call %q in provider call %d", callID, n)
	return ""
}

func call(id, name string) providers.ToolCall {
	return providers.ToolCall{ID: id, Type: "function", Name: name, Arguments: map[string]any{}}
}

func TestProactiveToolFilter(t *testing.T) {
	has := func(f func(string) bool, names ...string) []bool {
		out := make([]bool, len(names))
		for i, n := range names {
			out[i] = f(n)
		}
		return out
	}
	eq := func(a, b []bool) bool { return slices.Equal(a, b) }

	if f := proactiveToolFilter(proactive.TurnProactive, []string{"x", "*"}); f != nil {
		t.Error(`a list containing "*" must mean no filter`)
	}
	if f := proactiveToolFilter(proactive.TurnHeartbeatLegacy, []string{" * "}); f != nil {
		t.Error(`a padded "*" must mean no filter`)
	}

	f := proactiveToolFilter(proactive.TurnProactive, []string{" get_ticker ", "", "web_search"})
	if !eq(has(f, "get_ticker", "web_search", "get_orderbook", ""), []bool{true, true, false, false}) {
		t.Error("an explicit list must keep exactly its (trimmed) names")
	}

	// Empty and blank lists are the default of the kind.
	for _, req := range [][]string{nil, {}, {" ", ""}} {
		hb := proactiveToolFilter(proactive.TurnHeartbeatLegacy, req)
		cd := proactiveToolFilter(proactive.TurnProactive, req)
		for _, n := range heartbeatTools {
			if !hb(n) {
				t.Errorf("heartbeat default lacks %s (request %q)", n, req)
			}
		}
		for _, n := range candidateTools {
			if !cd(n) {
				t.Errorf("candidate default lacks %s (request %q)", n, req)
			}
		}
		if hb(tools.NameGetOrderBook) || cd(tools.NameGetOrderBook) {
			t.Error("a default subset offers get_orderbook")
		}
		if cd(tools.NameTakeSnapshot) || cd(tools.NameWebFetch) {
			t.Error("the candidate default offers a heartbeat-only tool")
		}
	}
}

// The defaults only name tools a proactive turn could ever call: a name the
// ceiling denies would be dead text in the list.
func TestProactiveDefaultSubsets_AreWithinTheCeiling(t *testing.T) {
	for _, tc := range []struct {
		kind  proactive.TurnKind
		names []string
	}{
		{proactive.TurnHeartbeatLegacy, heartbeatTools},
		{proactive.TurnProactive, candidateTools},
	} {
		ctx := tools.WithProactiveTurn(context.Background(), tc.kind)
		seen := map[string]bool{}
		for _, n := range tc.names {
			if seen[n] {
				t.Errorf("%s: %s is listed twice", tc.kind, n)
			}
			seen[n] = true
			if n == tools.NameProposeOrder {
				// Granted by name to a trusted candidate turn only; its grant is
				// tested in pkg/tools (TestProposeOrder_TurnMatrix).
				continue
			}
			if !tools.TurnMayCall(ctx, n) {
				t.Errorf("%s: default subset names %s, which the proactive ceiling denies", tc.kind, n)
			}
			if n == tools.NameRespondProactive {
				t.Errorf("%s: respond_proactive is always present and does not belong in a list", tc.kind)
			}
		}
	}
}

func TestProactiveTools_OnlyListedAndPermittedAreDefinedAndCallable(t *testing.T) {
	prov := script(
		callsResponse(
			call("a", tools.NameGetTicker),
			call("b", tools.NameGetOrderBook), // a read tool, not in the subset
			call("c", tools.NameMessage),      // in the list, denied by the ceiling
			call("d", tools.NameCreateOrder),  // ditto
		),
		respondResponse(silentDecision),
	)
	al := proactiveLoop(t, prov)
	recs := registerRecording(al, tools.NameGetTicker, tools.NameGetOrderBook, tools.NameMessage, tools.NameCreateOrder)

	req := proactiveReq()
	req.Tools = []string{tools.NameGetTicker, tools.NameMessage, tools.NameCreateOrder, "no_such_tool"}
	mustRun(t, al, req)

	if got, want := sortedDefNames(prov.defs[0]), []string{tools.NameGetTicker, tools.NameRespondProactive}; !slices.Equal(got, want) {
		t.Errorf("defined tools = %v, want %v (listed AND permitted, plus respond_proactive)", got, want)
	}
	if n := len(recs[tools.NameGetTicker].calls()); n != 1 {
		t.Errorf("get_ticker ran %d times, want 1", n)
	}
	for _, name := range []string{tools.NameGetOrderBook, tools.NameMessage, tools.NameCreateOrder} {
		if n := len(recs[name].calls()); n != 0 {
			t.Errorf("%s ran %d times in a turn that was not offered it", name, n)
		}
	}
	// A tool outside the subset is not there at all; one the ceiling denies is
	// refused as before.
	if got := toolResultFor(t, prov, 1, "b"); !strings.Contains(got, "not found") {
		t.Errorf("get_orderbook (outside the subset) answered %q, want a not-found error", got)
	}
	for _, id := range []string{"c", "d"} {
		if got := toolResultFor(t, prov, 1, id); !strings.Contains(got, "not permitted in an autonomous (proactive) turn") {
			t.Errorf("call %s (denied by the ceiling) answered %q", id, got)
		}
	}
}

func TestProactiveTools_RespondProactiveIsAlwaysPresent(t *testing.T) {
	for name, list := range map[string][]string{
		"only a denied name": {tools.NameMessage},
		"only unknown names": {"no_such_tool"},
		"a read tool":        {tools.NameGetTicker},
	} {
		t.Run(name, func(t *testing.T) {
			prov := script(respondResponse(silentDecision))
			al := proactiveLoop(t, prov)
			registerRecording(al, tools.NameGetTicker, tools.NameMessage)
			req := proactiveReq()
			req.Tools = list
			res := mustRun(t, al, req)
			if !slices.Contains(defNames(prov.defs[0]), tools.NameRespondProactive) {
				t.Errorf("respond_proactive is not defined: %v", defNames(prov.defs[0]))
			}
			if len(res.Decisions) != 1 || res.Decisions[0].Kind != proactive.DecideSilent {
				t.Errorf("the turn could not answer through respond_proactive: %+v", res.Decisions)
			}
		})
	}
}

func TestProactiveTools_StarRestoresEverythingPermitted(t *testing.T) {
	prov := script(respondResponse(silentDecision))
	al := proactiveLoop(t, prov)
	registerRecording(al, tools.NameGetTicker, tools.NameGetOrderBook, tools.NameMessage, tools.NameCreateOrder)

	// Everything the ceiling lets this kind of turn call, from the shared registry.
	ctx := tools.WithProactiveTurn(context.Background(), proactive.TurnProactive)
	want := append(defNames(al.GetRegistry().GetDefaultAgent().Tools.ToProviderDefsFor(ctx)), tools.NameRespondProactive)
	sort.Strings(want)

	req := proactiveReq()
	req.Tools = []string{AllProactiveTools}
	mustRun(t, al, req)

	got := sortedDefNames(prov.defs[0])
	if !slices.Equal(got, want) {
		t.Errorf(`"*" offered %v, want everything permitted: %v`, got, want)
	}
	for _, must := range []string{tools.NameGetTicker, tools.NameGetOrderBook} {
		if !slices.Contains(got, must) {
			t.Errorf(`"*" did not offer %s`, must)
		}
	}
	for _, denied := range []string{tools.NameMessage, tools.NameCreateOrder} {
		if slices.Contains(got, denied) {
			t.Errorf(`"*" offered %s, which the ceiling denies`, denied)
		}
	}
}

// With no list the kind's default applies: a read tool outside it is neither
// defined nor callable, and a heartbeat-only tool is the heartbeat's alone.
func TestProactiveTools_DefaultPerKind(t *testing.T) {
	const outside = tools.NameGetOrderBook
	for _, tc := range []struct {
		kind      proactive.TurnKind
		wantIn    []string
		wantNotIn []string
	}{
		{proactive.TurnHeartbeatLegacy,
			[]string{tools.NameGetTicker, tools.NameWebFetch, tools.NameTakeSnapshot, tools.NameRespondProactive},
			[]string{outside}},
		{proactive.TurnProactive,
			[]string{tools.NameGetTicker, tools.NameRespondProactive},
			[]string{outside, tools.NameWebFetch, tools.NameTakeSnapshot, tools.NameWebSearch}},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			prov := script(
				callsResponse(call("o", outside)),
				respondResponse(silentDecision),
			)
			al := proactiveLoop(t, prov)
			recs := registerRecording(al, tools.NameGetTicker, outside, tools.NameWebFetch, tools.NameTakeSnapshot, tools.NameWebSearch)

			req := proactiveReq()
			req.Kind = tc.kind
			mustRun(t, al, req)

			defs := defNames(prov.defs[0])
			for _, n := range tc.wantIn {
				if !slices.Contains(defs, n) {
					t.Errorf("%s: default subset does not offer %s (offered %v)", tc.kind, n, defs)
				}
			}
			for _, n := range tc.wantNotIn {
				if slices.Contains(defs, n) {
					t.Errorf("%s: default subset offers %s (offered %v)", tc.kind, n, defs)
				}
			}
			if n := len(recs[outside].calls()); n != 0 {
				t.Errorf("%s ran %d times outside the default subset", outside, n)
			}
			if got := toolResultFor(t, prov, 1, "o"); !strings.Contains(got, "not found") {
				t.Errorf("%s outside the default subset answered %q", outside, got)
			}
		})
	}
}

// The subset lives on the turn's own clone: the agent's registry is the same
// before and after, and the user turns around it are offered exactly the same
// definitions.
func TestProactiveTools_SharedRegistryAndUserTurnsUntouched(t *testing.T) {
	prov := script(
		providers.LLMResponse{Content: "first user turn"},
		respondResponse(silentDecision),
		providers.LLMResponse{Content: "proactive done"},
		providers.LLMResponse{Content: "second user turn"},
	)
	al := proactiveLoop(t, prov)
	registerRecording(al, tools.NameGetTicker, tools.NameGetOrderBook)
	shared := al.GetRegistry().GetDefaultAgent().Tools
	namesBefore := shared.List()

	if _, err := al.ProcessDirect(context.Background(), "hello", "user-1"); err != nil {
		t.Fatal(err)
	}
	req := proactiveReq()
	req.Tools = []string{tools.NameGetTicker}
	mustRun(t, al, req)
	if _, err := al.ProcessDirect(context.Background(), "hello again", "user-2"); err != nil {
		t.Fatal(err)
	}

	if after := shared.List(); !slices.Equal(namesBefore, after) {
		t.Errorf("the shared registry changed: before %v, after %v", namesBefore, after)
	}
	if _, ok := shared.Get(tools.NameRespondProactive); ok {
		t.Error("respond_proactive leaked into the shared registry")
	}
	if _, ok := shared.Get(tools.NameGetOrderBook); !ok {
		t.Error("a tool outside the subset was removed from the shared registry")
	}

	prov.mu.Lock()
	defer prov.mu.Unlock()
	before, _ := json.Marshal(prov.defs[0])
	proactiveDefs := defNames(prov.defs[1])
	after, _ := json.Marshal(prov.defs[len(prov.defs)-1])
	if string(before) != string(after) {
		t.Errorf("a user turn's tool definitions changed across a proactive turn:\nbefore %s\nafter  %s", before, after)
	}
	want, _ := json.Marshal(shared.ToProviderDefs())
	if string(before) != string(want) {
		t.Error("a user turn is not offered exactly the registry's definitions")
	}
	if slices.Contains(defNames(prov.defs[0]), tools.NameRespondProactive) {
		t.Error("a user turn was offered respond_proactive")
	}
	if !slices.Equal(sortedStrings(proactiveDefs), []string{tools.NameGetTicker, tools.NameRespondProactive}) {
		t.Errorf("the proactive turn was offered %v", proactiveDefs)
	}
}

func sortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// The skills listing follows load_skill: offered with the tool, absent without.
func TestProactiveTools_SkillsIndexFollowsLoadSkill(t *testing.T) {
	skillsHeader := "# Skills\n\nCall `load_skill`"
	run := func(t *testing.T, list []string) string {
		t.Helper()
		prov := script(respondResponse(silentDecision))
		al, _ := defaultWorkspaceLoop(t, prov)
		req := proactiveReq()
		req.Tools = list
		mustRun(t, al, req)
		prov.mu.Lock()
		defer prov.mu.Unlock()
		return prov.msgs[0][0].Content
	}

	if sys := run(t, nil); !strings.Contains(sys, skillsHeader) {
		t.Error("the default subset has load_skill but the prompt lists no skills")
	}
	if sys := run(t, []string{AllProactiveTools}); !strings.Contains(sys, skillsHeader) {
		t.Error(`"*" has load_skill but the prompt lists no skills`)
	}
	if sys := run(t, []string{tools.NameGetTicker}); strings.Contains(sys, skillsHeader) || strings.Contains(sys, "# Skills") {
		t.Error("a subset without load_skill still lists skills the turn cannot open")
	}
}
