//go:build !windows

package sandboxlab

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestE2E_SettleOnIdleInstance drives the real binary over stdio: `inspect idle` lists
// every component the plan names, `inspect settle` on a quiet instance answers
// settled:true, and two sim hours at the 1 m resolution (about 120 settled steps) take
// under 5 s of wall time, each step journaled as a `settle` entry.
// KQ_SANDBOX_E2E_BIN names a prebuilt khunquant.
func TestE2E_SettleOnIdleInstance(t *testing.T) {
	bin := os.Getenv("KQ_SANDBOX_E2E_BIN")
	if bin == "" {
		t.Skip("set KQ_SANDBOX_E2E_BIN to a prebuilt khunquant binary to run this test")
	}
	base := sandboxEnv(t)
	cmd := exec.Command(bin, "sandbox", "mcp")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "TMPDIR=" + base, "HOME=" + os.Getenv("HOME")}
	cmd.Dir = repoRoot(t)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "e2e-settle", Version: "1"}, nil).Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connect to `sandbox mcp`: %v\n%s", err, stderr.String())
	}
	t.Cleanup(func() { _ = cs.Close() })
	call := func(name string, args any, into any) {
		t.Helper()
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var sb strings.Builder
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				sb.WriteString(tc.Text)
			}
		}
		if res.IsError {
			t.Fatalf("%s %v: %s\nstderr:\n%s", name, args, sb.String(), stderr.String())
		}
		if into != nil {
			if err := json.Unmarshal([]byte(sb.String()), into); err != nil {
				t.Fatalf("%s: decode %q: %v", name, sb.String(), err)
			}
		}
	}

	var up struct {
		State string `json:"state"`
	}
	call("sandbox_instance", map[string]any{"op": "up", "fixtures_from": repoFixtures(t),
		"clock_mode": "manual", "clock_start": "2026-03-10T09:00:00Z"}, &up)
	if up.State != "running" {
		t.Fatalf("up = %+v", up)
	}

	var idle struct {
		Quiescers []struct {
			Name string `json:"name"`
			Idle bool   `json:"idle"`
		} `json:"quiescers"`
	}
	call("sandbox_inspect", map[string]any{"op": "idle"}, &idle)
	have := map[string]bool{}
	for _, q := range idle.Quiescers {
		have[q.Name] = q.Idle
	}
	for _, name := range []string{"engine", "perception", "outbox", "bus", "channels", "agent", "cognition", "cron", "notify",
		"orderfeed", "worldmodel", "journal", "approval", "mandate_sync", "feedback", "policy", "replay", "reflection",
		"hindsight", "intents", "simchat", "exchange_sim", "gateway.reload", "labjournal"} {
		if _, ok := have[name]; !ok {
			t.Errorf("inspect idle has no %q (have %v)", name, have)
		}
	}
	t.Logf("quiescers: %v", have)

	var st struct {
		Settled bool     `json:"settled"`
		WallMS  int64    `json:"wall_ms"`
		Busy    []string `json:"busy"`
	}
	call("sandbox_inspect", map[string]any{"op": "settle"}, &st)
	if !st.Settled || len(st.Busy) != 0 {
		t.Fatalf("settle on an idle instance = %+v", st)
	}

	start := time.Now()
	var adv struct {
		Done  bool `json:"done"`
		Steps int  `json:"steps"`
	}
	call("sandbox_clock", map[string]any{"op": "advance", "by": "2h"}, &adv)
	wall := time.Since(start)
	if !adv.Done || adv.Steps < 100 {
		t.Fatalf("advance = %+v", adv)
	}
	t.Logf("advance by=2h at resolution 1m: %d steps in %v wall (settled each step)", adv.Steps, wall)
	if wall > 5*time.Second {
		t.Fatalf("advance by=2h took %v of wall time, want under 5 s", wall)
	}

	var j struct {
		Entries []struct {
			Kind    string          `json:"kind"`
			Payload json.RawMessage `json:"payload"`
		} `json:"entries"`
	}
	call("sandbox_inspect", map[string]any{"op": "journal", "kinds": []string{"settle"}, "limit": 1000}, &j)
	if len(j.Entries) < adv.Steps {
		t.Fatalf("%d settle entries for %d steps", len(j.Entries), adv.Steps)
	}
	if !strings.Contains(string(j.Entries[len(j.Entries)-1].Payload), `"ok":true`) {
		t.Fatalf("last settle entry: %s", j.Entries[len(j.Entries)-1].Payload)
	}
	call("sandbox_instance", map[string]any{"op": "down"}, nil)
}
