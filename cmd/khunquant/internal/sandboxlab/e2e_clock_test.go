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

// TestE2E_ClockAdvanceAndRestart drives the real binary over stdio: an instance started
// with a manual clock advances two sim hours in a few wall seconds, journals the steps,
// and a graceful restart resumes at the same sim time (clock.json).
// KQ_SANDBOX_E2E_BIN names a prebuilt khunquant.
func TestE2E_ClockAdvanceAndRestart(t *testing.T) {
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
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "e2e-clock", Version: "1"}, nil).Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
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
		PID   int    `json:"pid"`
	}
	call("sandbox_instance", map[string]any{"op": "up", "fixtures_from": repoFixtures(t),
		"clock_mode": "manual", "clock_start": "2026-03-10T09:00:00Z"}, &up)
	if up.State != "running" {
		t.Fatalf("up = %+v", up)
	}

	type clockState struct {
		Now  string `json:"now"`
		Mode string `json:"mode"`
	}
	var got clockState
	call("sandbox_clock", map[string]any{"op": "get"}, &got)
	if got.Mode != "manual" || got.Now != "2026-03-10T09:00:00Z" {
		t.Fatalf("clock get = %+v, want manual at the start given to `up`", got)
	}

	start := time.Now()
	var adv struct {
		Done    bool   `json:"done"`
		Reached string `json:"reached"`
		Steps   int    `json:"steps"`
	}
	call("sandbox_clock", map[string]any{"op": "advance", "by": "2h"}, &adv)
	wall := time.Since(start)
	if !adv.Done || adv.Reached != "2026-03-10T11:00:00Z" || adv.Steps < 1 {
		t.Fatalf("advance = %+v", adv)
	}
	if wall > 10*time.Second {
		t.Fatalf("advance by=2h took %v of wall time, want under 10 s", wall)
	}
	t.Logf("advance by=2h: %+v in %v wall", adv, wall)

	var j struct {
		Entries []struct {
			Kind string `json:"kind"`
		} `json:"entries"`
	}
	call("sandbox_inspect", map[string]any{"op": "journal", "kinds": []string{"clock.step"}, "limit": 1000}, &j)
	if len(j.Entries) == 0 || j.Entries[0].Kind != "clock.step" {
		t.Fatalf("no clock.step entries in the journal: %+v", j)
	}

	var before, after clockState
	call("sandbox_clock", map[string]any{"op": "get"}, &before)
	var restarted struct {
		PID int `json:"pid"`
	}
	call("sandbox_instance", map[string]any{"op": "restart", "mode": "graceful"}, &restarted)
	if restarted.PID == up.PID {
		t.Fatalf("restart kept pid %d", up.PID)
	}
	call("sandbox_clock", map[string]any{"op": "get"}, &after)
	if after.Now != before.Now || after.Mode != "manual" {
		t.Fatalf("clock after a graceful restart = %+v, want %+v (sim time kept)", after, before)
	}
	call("sandbox_instance", map[string]any{"op": "down"}, nil)
}
