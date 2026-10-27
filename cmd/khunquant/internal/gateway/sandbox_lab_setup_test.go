package gateway

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

// labPost calls a lab group over its HTTP route and decodes the JSON answer.
func labPost(t *testing.T, f *lockedFixture, group, body string) (int, map[string]any) {
	t.Helper()
	code, out, err := postJSON(f.url("/api/sandbox/lab/"+group), lockTestToken, body)
	if err != nil {
		t.Fatalf("POST %s %s: %v", group, body, err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("POST %s %s = %d, body %q is not JSON: %v", group, body, code, out, err)
	}
	return code, m
}

// The account group is on a locked gateway as a route and an MCP tool, and its seed is
// the simulator's own state, which the exchange API then serves.
func TestLockedGatewayAccountGroupSeedsTheSimulator(t *testing.T) {
	f := newLockedFixture(t, func(c *config.Config) { c.Debug.DevMCP.Enabled = true })
	if !mcpTools(t, f.assemblyFixture, lockTestToken)["sandbox_account"] {
		t.Fatal("tools/list has no sandbox_account")
	}
	if code, out := labPost(t, f, "account", `{"op":"seed","venue":"binance","balances":{"USDT":10000}}`); code != http.StatusOK {
		t.Fatalf("seed = %d %v", code, out)
	}
	code, out := labPost(t, f, "account", `{"op":"get","venue":"binance"}`)
	usdt, _ := out["balances"].(map[string]any)["USDT"].(map[string]any)
	if code != http.StatusOK || usdt["free"] != 10000.0 {
		t.Fatalf("get = %d %v, want USDT free 10000", code, out)
	}
	// The exchange side reads the same state.
	status, body := sandboxCall(t, f, "GET", "binance/fapi.binance.com/fapi/v3/account")
	if status != http.StatusOK || !strings.Contains(body, "10000") {
		t.Fatalf("exchange account = %d %s, want it to show the seeded 10000", status, body)
	}
	if code, _ := labPost(t, f, "account", `{"op":"seed","venue":"bitkub","balances":{"USDT":1}}`); code != http.StatusServiceUnavailable {
		t.Errorf("seed on bitkub = %d, want 503 (unavailable)", code)
	}
}
