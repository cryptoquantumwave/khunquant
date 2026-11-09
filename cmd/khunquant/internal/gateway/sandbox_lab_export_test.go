package gateway

import (
	"net/http"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

// A locked gateway offers export_replay and the recording op, and export_replay reaches
// the generation's trigger registry, attention configuration and profile: with a valid
// case it gets as far as the journal, which holds no recordable event yet.
func TestLockedGatewayExportReplayIsWired(t *testing.T) {
	f := newLockedFixture(t, ownerConfig, func(c *config.Config) {
		c.Proactive.Attention.Mode = config.ProactiveModeLive
		c.Proactive.Perception.Enabled = true
		c.Proactive.Triggers.Mode = config.ProactiveModeLive
	})
	const labels = `[{"match":"x","from":"2026-03-10T05:00:00Z","to":"2026-03-10T06:00:00Z"}]`
	code, out := labPost(t, f, "inspect", `{"op":"export_replay","case":"c1","labels":`+labels+`}`)
	msg, _ := out["error"].(string)
	if code != http.StatusBadRequest || !strings.Contains(msg, "no recordable event") {
		t.Errorf("export_replay = %d %v, want 400 from the journal read (the dependencies are wired)", code, out)
	}
	if code, out := labPost(t, f, "inspect", `{"op":"export_replay","case":"../x","labels":`+labels+`}`); code != http.StatusBadRequest {
		t.Errorf("a case with .. = %d %v, want 400", code, out)
	}
	if code, out := labPost(t, f, "market", `{"op":"recording","symbol":"BTC/USDT","path":"missing.jsonl"}`); code != http.StatusBadRequest {
		t.Errorf("recording of a missing file = %d %v, want 400 (the op exists and reads the home only)", code, out)
	}
}

// An ordinary gateway has no inspect or market group: no route, no MCP tool, so neither
// export_replay nor the recording op exists there.
func TestExportNotRegisteredWhenUnlocked(t *testing.T) {
	f := newAssemblyFixture(t, enableAllRoutes)
	for _, g := range []string{"inspect", "market"} {
		if code, body, err := postJSON(f.url("/api/sandbox/lab/"+g), testSandboxToken, `{"op":"export_replay","case":"c1"}`); err != nil || code != http.StatusNotFound {
			t.Errorf("POST /api/sandbox/lab/%s = %d (%v) %s, want 404", g, code, err, body)
		}
		if mcpTools(t, f, testDevMCPToken)["sandbox_"+g] {
			t.Errorf("an unlocked gateway lists sandbox_%s", g)
		}
	}
}
