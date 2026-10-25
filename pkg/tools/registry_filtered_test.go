package tools

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

func TestToolRegistry_CloneFiltered_KeepsOnlyWhatIsAskedFor(t *testing.T) {
	r := NewToolRegistry()
	for _, n := range []string{"read_file", "exec", "web_search"} {
		r.Register(newMockTool(n, "d"))
	}
	r.RegisterHidden(newMockTool("mcp_tool", "d"))

	clone := r.CloneFiltered(func(name string) bool { return name == "web_search" || name == "mcp_tool" })

	if got := clone.List(); !slices.Equal(got, []string{"mcp_tool", "web_search"}) {
		t.Errorf("clone has %v, want [mcp_tool web_search]", got)
	}
	// A tool left out is not callable by name.
	res := clone.Execute(context.Background(), "exec", map[string]any{})
	if res == nil || !res.IsError || !strings.Contains(res.ForLLM, "not found") {
		t.Errorf("executing a filtered-out tool: %+v", res)
	}
	// The receiver is untouched.
	if r.Count() != 4 {
		t.Errorf("the source registry has %d tools after filtering a clone, want 4", r.Count())
	}
	if _, ok := r.Get("exec"); !ok {
		t.Error("the source registry lost a tool")
	}
	// Registering on the clone does not reach the source.
	clone.Register(newMockTool("extra", "d"))
	if _, ok := r.Get("extra"); ok {
		t.Error("a tool registered on the clone reached the source")
	}
}

func TestToolRegistry_CloneFiltered_NilKeepsEverything(t *testing.T) {
	r := NewToolRegistry()
	r.Register(newMockTool("a", "d"))
	r.RegisterHidden(newMockTool("b", "d"))
	if got, want := r.CloneFiltered(nil).Count(), r.Clone().Count(); got != want || got != 2 {
		t.Errorf("CloneFiltered(nil) has %d tools, Clone has %d, want both 2", got, want)
	}
}

// TurnMayCall is turnMayCall, exported: a caller narrowing a registry must ask
// the same question the executor asks.
func TestTurnMayCall_MatchesTheProactiveCeiling(t *testing.T) {
	hb := WithProactiveTurn(context.Background(), proactive.TurnHeartbeatLegacy)
	cd := WithProactiveTurn(context.Background(), proactive.TurnProactive)

	for _, tc := range []struct {
		name   string
		ctx    context.Context
		want   bool
		reason string
	}{
		{NameGetTicker, hb, true, "read"},
		{NameRespondProactive, cd, true, "the contract tool"},
		{NameMessage, hb, false, "notify"},
		{NameCreateOrder, hb, false, "trade"},
		{NameExec, cd, false, "system"},
		{NameWebFetch, hb, true, "heartbeat kind only"},
		{NameWebFetch, cd, false, "heartbeat kind only"},
		{NameTakeSnapshot, hb, true, "heartbeat kind only"},
		{NameTakeSnapshot, cd, false, "heartbeat kind only"},
		{"mcp_unclassified_tool", hb, false, "unclassified"},
	} {
		if got := TurnMayCall(tc.ctx, tc.name); got != tc.want {
			t.Errorf("TurnMayCall(%s) = %v, want %v (%s)", tc.name, got, tc.want, tc.reason)
		}
		if TurnMayCall(tc.ctx, tc.name) != turnMayCall(tc.ctx, tc.name) {
			t.Errorf("TurnMayCall(%s) differs from turnMayCall", tc.name)
		}
	}
}

// Linking this package in teaches the config package which tool names exist, so
// that a typo in proactive.heartbeat.tools is warned about and ignored.
func TestKnownToolNames_RegisteredWithConfig(t *testing.T) {
	for _, n := range []string{NameGetTicker, NameWebSearch, NameRespondProactive, NameMessage} {
		if !IsKnownToolName(n) {
			t.Errorf("%s is not a known tool name", n)
		}
	}
	for _, n := range []string{"", "get_tikcer", "mcp_something"} {
		if IsKnownToolName(n) {
			t.Errorf("%q is a known tool name", n)
		}
	}

	c := config.DefaultConfig().Proactive
	c.Heartbeat.Tools = []string{NameGetTicker, "get_tikcer", "*", NameWebSearch}
	got := c.Effective().HeartbeatTools
	if want := []string{NameGetTicker, "*", NameWebSearch}; !slices.Equal(got, want) {
		t.Errorf("effective tools = %v, want %v (the config package should know tool names once tools is linked)", got, want)
	}
}
