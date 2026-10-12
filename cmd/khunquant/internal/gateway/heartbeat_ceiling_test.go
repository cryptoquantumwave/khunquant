package gateway

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/providers"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

// recordingProvider answers the first call with a respond_proactive call and
// records the tools it was offered.
type recordingProvider struct {
	mu    sync.Mutex
	calls int
	tools [][]string
}

func (p *recordingProvider) Chat(
	_ context.Context, _ []providers.Message, defs []providers.ToolDefinition, _ string, _ map[string]any,
) (*providers.LLMResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var names []string
	for _, d := range defs {
		names = append(names, d.Function.Name)
	}
	p.tools = append(p.tools, names)
	p.calls++
	if p.calls == 1 {
		return &providers.LLMResponse{ToolCalls: []providers.ToolCall{{
			ID: "r1", Type: "function", Name: tools.NameRespondProactive,
			Arguments: map[string]any{
				"decision": "notify", "severity": "warning", "dedup_key": "sol:drop",
				"text": "SOL is down\nwatch the position",
			},
		}}}, nil
	}
	return &providers.LLMResponse{Content: "done"}, nil
}

func (p *recordingProvider) GetDefaultModel() string { return "recording" }

// TestHeartbeatGoverned_NoMessageTool runs the governed handler on the gateway's
// real agent loop (with the gateway's own tools registered) and checks what the
// model was offered: it can report through respond_proactive and can read, but
// it cannot message, spawn, write files, run commands or touch the scheduler.
func TestHeartbeatGoverned_NoMessageTool(t *testing.T) {
	governedLive := compose(withOwner("1001"), withAttentionMode(config.ProactiveModeLive),
		withHeartbeat(config.HeartbeatModeGoverned))
	f := newAssemblyFixture(t, governedLive)

	rec := &recordingProvider{}
	prev := createProvider
	createProvider = func(*config.Config) (providers.LLMProvider, string, error) { return rec, "", nil }
	t.Cleanup(func() { createProvider = prev })
	if err := f.reload(governedLive); err != nil {
		t.Fatal(err)
	}

	// The tools exist on the loop, so their absence below is the ceiling's doing.
	registered := map[string]bool{}
	for _, n := range f.al.GetRegistry().GetDefaultAgent().Tools.List() {
		registered[n] = true
	}
	for _, n := range []string{tools.NameMessage, tools.NameSpawn, "write_file", "exec", "cron"} {
		if !registered[n] {
			t.Fatalf("precondition: %q is not registered on the loop, so the test proves nothing", n)
		}
	}

	sub := &fakeSubmitter{}
	res := governedHeartbeatHandler(f.al, sub, 30*time.Minute, proactive.ModelTierPrimary)(context.Background(), "heartbeat prompt", "", "")
	if res == nil || res.IsError {
		t.Fatalf("handler result = %+v", res)
	}

	rec.mu.Lock()
	if len(rec.tools) == 0 {
		rec.mu.Unlock()
		t.Fatal("the model was never called")
	}
	offered := map[string]bool{}
	for _, n := range rec.tools[0] {
		offered[n] = true
	}
	rec.mu.Unlock()

	for _, must := range []string{tools.NameRespondProactive, "take_snapshot"} {
		if !offered[must] {
			t.Errorf("the governed heartbeat turn is not offered %q", must)
		}
	}
	for _, banned := range []string{tools.NameMessage, tools.NameSpawn, "write_file", "edit_file",
		"append_file", "exec", "cron", "create_order"} {
		if offered[banned] {
			t.Errorf("the governed heartbeat turn is offered %q", banned)
		}
	}

	// And the decision came out of the real turn as the candidate.
	got := sub.submitted()
	if len(got) != 1 || got[0].Candidate.DedupKey != "heartbeat:sol:drop" ||
		got[0].Candidate.Severity != proactive.SeverityWarning || got[0].Candidate.Title != "SOL is down" ||
		got[0].Prose != "watch the position" {
		t.Fatalf("submitted %+v", got)
	}
}
