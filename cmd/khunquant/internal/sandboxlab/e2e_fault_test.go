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

// TestE2E_FaultAddListClear drives the real binary over stdio: sandbox_fault adds a rule, the
// agent's own tool call (get_ticker, through the real ccxt client) meets OKX's rate-limit error,
// list counts the firing, the journal holds fault.fired, and clear ends it.
// (written_then_timeout through the futures tool needs a credentialed exchange account, which
// the binary's `sandbox mcp` instance has none of; the gateway test
// TestWrittenThenTimeout_ThroughTheFuturesTool covers it with a test credential.)
// KQ_SANDBOX_E2E_BIN names a prebuilt khunquant.
func TestE2E_FaultAddListClear(t *testing.T) {
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
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "e2e-fault", Version: "1"}, nil).Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connect to `sandbox mcp`: %v\n%s", err, stderr.String())
	}
	t.Cleanup(func() { _ = cs.Close() })
	call := func(name string, args any, into any) string {
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
		return sb.String()
	}
	type toolResult struct {
		IsError bool   `json:"is_error"`
		ForLLM  string `json:"for_llm"`
	}
	callTool := func(name string, args map[string]any) toolResult {
		t.Helper()
		var r toolResult
		call("sandbox_call_tool", map[string]any{"name": name, "args": args, "as": "cli"}, &r) // the local console
		return r
	}

	var up struct {
		State string `json:"state"`
	}
	call("sandbox_instance", map[string]any{"op": "up", "fixtures_from": repoFixtures(t),
		"clock_mode": "manual", "clock_start": "2026-03-10T09:00:00Z"}, &up)
	if up.State != "running" {
		t.Fatalf("up = %+v", up)
	}
	defer call("sandbox_instance", map[string]any{"op": "down"}, nil)

	call("sandbox_market", map[string]any{"op": "set", "venue": "okx", "symbol": "BTC/USDT", "price": 98000}, nil)
	ticker := map[string]any{"provider": "okx", "symbol": "BTC/USDT"}
	if r := callTool("get_ticker", ticker); r.IsError {
		t.Fatalf("get_ticker before any rule failed: %s", r.ForLLM)
	}

	var added struct {
		IDs []string `json:"ids"`
	}
	call("sandbox_fault", map[string]any{"op": "add",
		"match": map[string]any{"venue": "okx", "path_prefix": "/api/v5/market/ticker"}, "effect": map[string]any{"error": "rate_limit"}}, &added)
	if len(added.IDs) != 1 {
		t.Fatalf("add = %+v", added)
	}
	r := callTool("get_ticker", ticker)
	if !r.IsError || !strings.Contains(strings.ToLower(r.ForLLM), "ratelimit") && !strings.Contains(r.ForLLM, "50011") {
		t.Fatalf("get_ticker under the rule = %+v, want OKX's rate-limit error", r)
	}

	var list struct {
		Rules []struct {
			Rule  struct{ ID string }
			Fired int
		} `json:"rules"`
	}
	call("sandbox_fault", map[string]any{"op": "list"}, &list)
	if len(list.Rules) != 1 || list.Rules[0].Rule.ID != added.IDs[0] || list.Rules[0].Fired < 1 {
		t.Fatalf("list = %+v, want the rule fired", list)
	}
	var j struct {
		Entries []struct {
			Payload map[string]any `json:"payload"`
		} `json:"entries"`
	}
	call("sandbox_inspect", map[string]any{"op": "journal", "kinds": []string{"fault.fired"}}, &j)
	if len(j.Entries) < 1 || j.Entries[0].Payload["rule_id"] != added.IDs[0] || j.Entries[0].Payload["venue"] != "okx" {
		t.Fatalf("fault.fired entries = %+v", j.Entries)
	}
	var cleared struct {
		Cleared int `json:"cleared"`
	}
	call("sandbox_fault", map[string]any{"op": "clear"}, &cleared)
	if cleared.Cleared != 1 {
		t.Fatalf("clear = %+v", cleared)
	}
	call("sandbox_fault", map[string]any{"op": "list"}, &list)
	if len(list.Rules) != 0 {
		t.Fatalf("list after clear = %+v", list)
	}
	if r := callTool("get_ticker", ticker); r.IsError {
		t.Fatalf("get_ticker after clear failed: %s", r.ForLLM)
	}
}
