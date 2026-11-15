//go:build !windows

package sandboxlab

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestE2E_UpWithOwnerAndAccountThenExplore drives the real binary over MCP the way an agent
// explores an instance by hand (RDY-2, RDY-3): one op=up with an owner and a placeholder
// exchange account and no clock_mode, then a read tool as the owner against the simulated
// venue, a clock advance, a snapshot and a down. The transcript is logged (go test -v).
// KQ_SANDBOX_E2E_BIN names a prebuilt khunquant.
func TestE2E_UpWithOwnerAndAccountThenExplore(t *testing.T) {
	bin := e2eBinary(t)
	base := sandboxEnv(t)
	cmd := exec.Command(bin, "sandbox", "mcp")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "TMPDIR=" + base, "HOME=" + os.Getenv("HOME")}
	cmd.Dir = repoRoot(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "e2e", Version: "1"}, nil).Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connect to `sandbox mcp`: %v", err)
	}
	defer func() { _ = cs.Close() }()

	call := func(name string, args map[string]any) string {
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
		a, _ := json.Marshal(args)
		t.Logf("> %s %s\n< %s", name, a, sb.String())
		if res.IsError {
			t.Fatalf("%s failed: %s", name, sb.String())
		}
		return sb.String()
	}

	call("sandbox_instance", map[string]any{
		"op": "up", "fixtures_from": repoFixtures(t), "accounts": []string{"binance"},
		"config_patch": map[string]any{"proactive": map[string]any{
			"enabled": true,
			"owner":   map[string]any{"channel": "simchat", "chat_id": "dm:owner", "sender_id": "simchat:owner"},
		}},
	})
	call("sandbox_account", map[string]any{"op": "seed", "venue": "binance", "balances": map[string]float64{"USDT": 1234}})
	call("sandbox_market", map[string]any{"op": "set", "venue": "binance", "symbol": "BTC/USDT", "price": 98000})

	if got := call("sandbox_call_tool", map[string]any{"as": "owner", "name": "get_assets_list", "args": map[string]any{"exchange": "binance", "wallet_type": "futures_usdt"}}); !strings.Contains(got, "1234") {
		t.Errorf("get_assets_list as the owner did not show the seeded balance: %s", got)
	}
	if got := call("sandbox_call_tool", map[string]any{"as": "owner", "name": "get_ticker", "args": map[string]any{"provider": "binance", "symbol": "BTC/USDT"}}); !strings.Contains(got, "98000") {
		t.Errorf("get_ticker as the owner did not show the simulated price: %s", got)
	}

	var before, after struct {
		Now  string `json:"now"`
		Mode string `json:"mode"`
	}
	if err := json.Unmarshal([]byte(call("sandbox_clock", map[string]any{"op": "get"})), &before); err != nil || before.Mode != "manual" {
		t.Fatalf("the default clock = %+v (%v), want manual", before, err)
	}
	call("sandbox_clock", map[string]any{"op": "advance", "by": "10m"})
	if err := json.Unmarshal([]byte(call("sandbox_clock", map[string]any{"op": "get"})), &after); err != nil || after.Now == before.Now {
		t.Errorf("the clock did not move: %+v -> %+v (%v)", before, after, err)
	}
	if got := call("sandbox_instance", map[string]any{"op": "snapshot", "label": "explored"}); !strings.Contains(got, `"id"`) {
		t.Errorf("snapshot = %s", got)
	}
	call("sandbox_instance", map[string]any{"op": "down"})
}
