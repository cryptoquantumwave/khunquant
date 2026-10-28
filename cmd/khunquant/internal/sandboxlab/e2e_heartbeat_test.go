//go:build !windows

package sandboxlab

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestE2E_HeartbeatOnSimClock drives the real binary over stdio: an instance whose config
// enables a 30 minute legacy heartbeat runs it on sim time. Advancing the manual clock by
// 31 minutes makes the heartbeat turn happen (a scripted rule answers it), and the prompt
// the simulated model received shows the sim time, not the wall clock.
// KQ_SANDBOX_E2E_BIN names a prebuilt khunquant.
func TestE2E_HeartbeatOnSimClock(t *testing.T) {
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
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "e2e-heartbeat", Version: "1"}, nil).Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
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
		Home  string `json:"home"`
		State string `json:"state"`
	}
	call("sandbox_instance", map[string]any{"op": "up", "fixtures_from": repoFixtures(t),
		"clock_mode": "manual", "clock_start": "2026-03-10T09:00:00Z"}, &up)
	if up.State != "running" || up.Home == "" {
		t.Fatalf("up = %+v", up)
	}

	// The instance setup: enable a 30 minute heartbeat in the instance's config, give it a
	// task, and restart so the gateway reads both.
	cfgPath := filepath.Join(up.Home, "config.json")
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	cfg["heartbeat"] = map[string]any{"enabled": true, "interval": 30}
	patched, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(cfgPath, patched, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(up.Home, "workspace", "HEARTBEAT.md"), []byte("- check the market\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	call("sandbox_instance", map[string]any{"op": "restart", "mode": "graceful"}, nil)

	call("sandbox_llm", map[string]any{"op": "script_put", "rules": []map[string]any{{
		"id": "hb", "match": map[string]any{"turn": "heartbeat_legacy"}, "respond": map[string]any{"text": "HEARTBEAT_OK"},
	}}}, nil)

	time.Sleep(2 * time.Second) // wall time passes; the heartbeat waits on sim time
	var before struct {
		Entries []json.RawMessage `json:"entries"`
	}
	call("sandbox_inspect", map[string]any{"op": "journal", "kinds": []string{"llm.call"}, "limit": 100}, &before)
	if len(before.Entries) != 0 {
		t.Fatalf("%d model calls in wall time alone: %s", len(before.Entries), before.Entries)
	}

	var adv struct {
		Done bool `json:"done"`
	}
	call("sandbox_clock", map[string]any{"op": "advance", "by": "31m"}, &adv)
	if !adv.Done {
		t.Fatalf("advance = %+v", adv)
	}

	var after struct {
		Entries []struct {
			Subject string          `json:"subject"`
			Payload json.RawMessage `json:"payload"`
		} `json:"entries"`
	}
	call("sandbox_inspect", map[string]any{"op": "journal", "kinds": []string{"llm.call"}, "limit": 100}, &after)
	hb := 0
	for _, e := range after.Entries {
		if e.Subject == "heartbeat_legacy" {
			hb++
		}
	}
	t.Logf("llm.call entries after advance by=31m: %d total, %d heartbeat_legacy", len(after.Entries), hb)
	if hb < 1 {
		t.Fatalf("no heartbeat_legacy model call after advancing 31m: %+v", after.Entries)
	}

	// The prompt the model received shows sim time (2026-03-10, 09:xx), never the wall date.
	var calls json.RawMessage
	call("list_llm_calls", map[string]any{}, &calls)
	prompts := string(calls)
	var list struct {
		Entries []struct {
			Seq uint64 `json:"seq"`
		} `json:"entries"`
	}
	_ = json.Unmarshal(calls, &list)
	if len(list.Entries) == 0 {
		t.Fatalf("list_llm_calls returned no calls: %s", prompts)
	}
	var full strings.Builder
	for _, c := range list.Entries {
		var one json.RawMessage
		call("read_llm_call", map[string]any{"seq": c.Seq}, &one)
		full.Write(one)
	}
	if !strings.Contains(full.String(), "2026-03-10") {
		t.Fatalf("no recorded model prompt shows the sim date 2026-03-10:\n%.2000s", full.String())
	}
	call("sandbox_instance", map[string]any{"op": "down"}, nil)
}
