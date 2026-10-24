package config

import (
	"reflect"
	"strings"
	"testing"
)

// knownTools stands in for the registration the tools package makes when it is
// linked in; this package cannot import it.
func knownTools(t *testing.T, names ...string) {
	t.Helper()
	set := map[string]bool{}
	for _, n := range names {
		set[n] = true
	}
	SetToolNameChecker(func(name string) bool { return set[name] })
	t.Cleanup(func() { SetToolNameChecker(nil) })
}

func TestHeartbeatTools_DefaultIsEmpty(t *testing.T) {
	cfg := DefaultConfig()
	if got := cfg.Proactive.Heartbeat.Tools; len(got) != 0 {
		t.Errorf("default proactive.heartbeat.tools = %q, want empty (the turn's built-in set)", got)
	}
	if got := cfg.Proactive.Effective().HeartbeatTools; len(got) != 0 {
		t.Errorf("default effective tools = %q, want empty", got)
	}
}

func TestHeartbeatTools_ExplicitListIsCleaned(t *testing.T) {
	c := liveProactive()
	c.Heartbeat.Tools = []string{" get_ticker ", "", "web_search", "get_ticker", "  "}
	if got, want := c.Effective().HeartbeatTools, []string{"get_ticker", "web_search"}; !reflect.DeepEqual(got, want) {
		t.Errorf("effective tools = %q, want %q (trimmed, de-duplicated, first order kept)", got, want)
	}
	// The raw field is left as the file had it.
	if got := c.Heartbeat.Tools; len(got) != 5 {
		t.Errorf("Effective changed the raw list: %q", got)
	}
}

func TestHeartbeatTools_StarMeansAllPermitted(t *testing.T) {
	knownTools(t, "get_ticker")
	c := liveProactive()
	c.Heartbeat.Tools = []string{"*"}
	if got, want := c.Effective().HeartbeatTools, []string{"*"}; !reflect.DeepEqual(got, want) {
		t.Errorf(`effective tools = %q, want %q: "*" is not a tool name and is never judged unknown`, got, want)
	}
	if _, unknown := cleanToolNames([]string{"*"}); len(unknown) != 0 {
		t.Errorf(`"*" reported unknown: %q`, unknown)
	}
}

// Tools are independent of the owner and the downgrades, like the model tier:
// the list only matters to a governed heartbeat, and Effective passes it on.
func TestHeartbeatTools_IndependentOfDowngrades(t *testing.T) {
	c := defaultProactiveConfig() // proactive disabled, no owner
	c.Heartbeat.Tools = []string{"get_ticker"}
	if got := c.Effective().HeartbeatTools; !reflect.DeepEqual(got, []string{"get_ticker"}) {
		t.Errorf("effective tools = %q, want the configured list even when proactive is off", got)
	}
}

func TestHeartbeatTools_UnknownNameWarnsAndIsIgnored(t *testing.T) {
	knownTools(t, "get_ticker", "web_search")
	errs, warns := captureProactiveLogs(t)

	c := liveProactive()
	c.Heartbeat.Tools = []string{"get_ticker", "get_tikcer", "web_search", "nope", "nope"}

	if got, want := c.Effective().HeartbeatTools, []string{"get_ticker", "web_search"}; !reflect.DeepEqual(got, want) {
		t.Errorf("effective tools = %q, want %q (unknown names ignored)", got, want)
	}

	reportProactive(c)
	reportProactive(c)
	if len(*errs) != 0 {
		t.Errorf("an unknown tool name is an ERROR: %q; it must be a WARN", *errs)
	}
	if len(*warns) != 1 {
		t.Fatalf("warns = %q, want exactly one (logged once)", *warns)
	}
	w := (*warns)[0]
	for _, want := range []string{"proactive.heartbeat.tools", "get_tikcer", "nope"} {
		if !strings.Contains(w, want) {
			t.Errorf("warning %q does not name %q", w, want)
		}
	}
	if strings.Contains(w, "get_ticker,") || strings.Contains(w, "web_search") {
		t.Errorf("warning %q names a known tool", w)
	}

	// The section stays valid: an unknown tool must not disable delivery.
	if err := ValidateProactive(c); err != nil {
		t.Errorf("an unknown tool name made the section invalid: %v", err)
	}
	if !c.Effective().CanDeliver() {
		t.Error("an unknown tool name turned delivery off")
	}
}

// A list that is nothing but unknown names is a typo, not a request for a turn
// with no tools: it falls back to the built-in set.
func TestHeartbeatTools_OnlyUnknownNamesFallBackToDefault(t *testing.T) {
	knownTools(t, "get_ticker")
	c := liveProactive()
	c.Heartbeat.Tools = []string{"nope", "nada"}
	if got := c.Effective().HeartbeatTools; len(got) != 0 {
		t.Errorf("effective tools = %q, want empty (the default set)", got)
	}
}

func TestHeartbeatTools_NoCheckerJudgesNothingUnknown(t *testing.T) {
	_, warns := captureProactiveLogs(t)
	c := liveProactive()
	c.Heartbeat.Tools = []string{"whatever"}
	reportProactive(c)
	if len(*warns) != 0 {
		t.Errorf("warned without a way to know tool names: %q", *warns)
	}
	if got := c.Effective().HeartbeatTools; !reflect.DeepEqual(got, []string{"whatever"}) {
		t.Errorf("effective tools = %q", got)
	}
}

func TestHeartbeatTools_LoadSaveReloadAndEnv(t *testing.T) {
	knownTools(t, "get_ticker", "web_search", "load_skill")
	_, warns := captureProactiveLogs(t)

	pathA := writeRawConfig(t, `{"proactive": {"heartbeat": {"tools": ["get_ticker", "bogus"]}}}`)
	a, err := LoadConfig(pathA)
	if err != nil {
		t.Fatalf("an unknown tool name failed the load: %v", err)
	}
	if got := a.Proactive.Heartbeat.Tools; !reflect.DeepEqual(got, []string{"get_ticker", "bogus"}) {
		t.Fatalf("loaded tools = %q", got)
	}
	if got := a.Proactive.Effective().HeartbeatTools; !reflect.DeepEqual(got, []string{"get_ticker"}) {
		t.Errorf("effective tools = %q, want [get_ticker]", got)
	}
	if len(*warns) != 1 || !strings.Contains((*warns)[0], "bogus") {
		t.Errorf("load warns = %q, want one warning naming bogus", *warns)
	}

	// A reload reads the file again and applies the changed list.
	pathB := writeRawConfig(t, `{"proactive": {"heartbeat": {"tools": ["*"]}}}`)
	b, err := LoadConfig(pathB)
	if err != nil {
		t.Fatal(err)
	}
	if got := b.Proactive.Effective().HeartbeatTools; !reflect.DeepEqual(got, []string{"*"}) {
		t.Errorf("after reload effective tools = %q, want [*]", got)
	}
	pathC := writeRawConfig(t, `{"proactive": {}}`)
	c, err := LoadConfig(pathC)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Proactive.Effective().HeartbeatTools; len(got) != 0 {
		t.Errorf("after a reload that drops the list, effective tools = %q, want the default", got)
	}

	// Saved and loaded again, the list survives.
	out := t.TempDir() + "/config.json"
	if err := SaveConfig(out, a); err != nil {
		t.Fatal(err)
	}
	again, err := LoadConfig(out)
	if err != nil {
		t.Fatal(err)
	}
	if got := again.Proactive.Heartbeat.Tools; !reflect.DeepEqual(got, []string{"get_ticker", "bogus"}) {
		t.Errorf("tools after save and load = %q", got)
	}

	// From the environment it is comma-separated.
	t.Setenv("KHUNQUANT_PROACTIVE_HEARTBEAT_TOOLS", "web_search, load_skill")
	e, err := LoadConfig(pathC)
	if err != nil {
		t.Fatal(err)
	}
	if got := e.Proactive.Effective().HeartbeatTools; !reflect.DeepEqual(got, []string{"web_search", "load_skill"}) {
		t.Errorf("tools from the environment = %q", got)
	}
}
