package agent

import (
	"context"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/providers"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/sim"
)

// The simulated model (pkg/providers/sim) tells an agent turn from a providers.Complete
// call, and the news parser from the seahorse summarizer, by the options of the Chat
// call. These tests pin each signal at the code that sets it, so changing one fails
// here, by name, instead of silently misrouting a scripted answer. The seahorse signal
// is pinned in sim_signals_seahorse_test.go (that code has build tags).

// CompleteLight (the news parser's call) is tool-less, marks no_native_tools and sends
// no prompt_cache_key.
func TestCompleteLightMarksNoNativeTools(t *testing.T) {
	if sim.OptionNoNativeTools != providers.OptionNoNativeTools {
		t.Fatalf("sim.OptionNoNativeTools = %q, providers.OptionNoNativeTools = %q", sim.OptionNoNativeTools, providers.OptionNoNativeTools)
	}
	rec := &oneShotRecorder{reply: "x"}
	al, _ := defaultWorkspaceLoop(t, rec)
	if _, _, err := al.CompleteLight(context.Background(), "SYS", "USER", 100); err != nil {
		t.Fatal(err)
	}
	if rec.options[sim.OptionNoNativeTools] != true {
		t.Errorf("CompleteLight options = %v, want %s=true", rec.options, sim.OptionNoNativeTools)
	}
	if _, has := rec.options[sim.OptionCacheKey]; has || len(rec.tools) != 0 {
		t.Errorf("CompleteLight sent a cache key or tools: options %v, tools %v", rec.options, rec.tools)
	}
}

// An agent turn always sends prompt_cache_key (the agent id), which is what keeps it
// from being taken for a providers.Complete call.
func TestAgentTurnSetsPromptCacheKey(t *testing.T) {
	rec := &oneShotRecorder{reply: "hello"}
	al, _ := defaultWorkspaceLoop(t, rec)
	if _, err := al.ProcessDirect(context.Background(), "hi", "sess-sim-signal"); err != nil {
		t.Fatal(err)
	}
	if key, _ := rec.options[sim.OptionCacheKey].(string); key == "" || key == sim.CacheKeySeahorse {
		t.Errorf("an agent turn sent %s=%q, want the agent id (options %v)", sim.OptionCacheKey, key, rec.options)
	}
}
