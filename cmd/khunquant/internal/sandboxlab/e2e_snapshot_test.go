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

// TestE2E_BranchFromSnapshot drives the real binary over stdio: set up an instance (a price,
// a SimWeb rule, sim time 09:59), snapshot it, run on and spoil it, then restore the same
// snapshot over it and, beside it, as a second instance. Each branch starts at 09:59 with
// the same price and rule and at the journal position of the snapshot, and the branches do
// not share a clock. KQ_SANDBOX_E2E_BIN names a prebuilt khunquant.
func TestE2E_BranchFromSnapshot(t *testing.T) {
	bin := os.Getenv("KQ_SANDBOX_E2E_BIN")
	if bin == "" {
		t.Skip("set KQ_SANDBOX_E2E_BIN to a prebuilt khunquant binary to run this test")
	}
	base := sandboxEnv(t)
	cmd := exec.Command(bin, "sandbox", "mcp")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "TMPDIR=" + base, "HOME=" + os.Getenv("HOME"), "KQ_SANDBOX_MAX_INSTANCES=2"}
	cmd.Dir = repoRoot(t)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "e2e-branch", Version: "1"}, nil).Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connect to `sandbox mcp`: %v\n%s", err, stderr.String())
	}
	t.Cleanup(func() { _ = cs.Close() })
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
		if res.IsError {
			t.Fatalf("%s %v: %s\nstderr:\n%s", name, args, sb.String(), stderr.String())
		}
		return sb.String()
	}
	clock := func(instance string) string {
		var c struct{ Now string }
		if err := json.Unmarshal([]byte(call("sandbox_clock", map[string]any{"op": "get", "instance": instance})), &c); err != nil {
			t.Fatal(err)
		}
		return c.Now
	}

	call("sandbox_instance", map[string]any{"op": "up", "name": "a", "fixtures_from": repoFixtures(t), "clock_mode": "manual", "clock_start": "2026-10-19T07:59:00Z"})
	// An H8-sized history: 26 sim hours of clock steps in the journal before the snapshot.
	for done := false; !done; { // done:false means the wall budget ended first: call again
		var adv struct{ Done bool }
		_ = json.Unmarshal([]byte(call("sandbox_clock", map[string]any{"op": "advance", "instance": "a", "to": "2026-10-20T09:59:00Z"})), &adv)
		done = adv.Done
	}
	call("sandbox_market", map[string]any{"op": "set", "instance": "a", "venue": "binance", "symbol": "BTC/USDT", "price": 98000})
	call("sandbox_web", map[string]any{"op": "put", "instance": "a", "url_pattern": "snap.example/*", "status": 200, "body": "hello"})
	var snap struct {
		ID         string `json:"id"`
		SimTime    string `json:"sim_time"`
		JournalSeq int64  `json:"journal_seq"`
		Bytes      int64  `json:"bytes"`
	}
	start := time.Now()
	if err := json.Unmarshal([]byte(call("sandbox_instance", map[string]any{"op": "snapshot", "name": "a", "label": "setup"})), &snap); err != nil {
		t.Fatal(err)
	}
	took := time.Since(start)
	t.Logf("snapshot %s: %d bytes in %v", snap.ID, snap.Bytes, took)
	if took > 3*time.Second || snap.Bytes > 20<<20 {
		t.Errorf("snapshot of an H8-sized instance took %v and holds %d bytes, want under 3s and 20 MiB", took, snap.Bytes)
	}
	if snap.ID == "" || snap.JournalSeq == 0 || !strings.HasPrefix(snap.SimTime, "2026-10-20T09:59:00") {
		t.Fatalf("snapshot = %+v", snap)
	}

	// Branch 1: run on, change the price, then go back to the snapshot over the same name.
	call("sandbox_clock", map[string]any{"op": "advance", "instance": "a", "by": "2m"})
	call("sandbox_market", map[string]any{"op": "set", "instance": "a", "venue": "binance", "symbol": "BTC/USDT", "price": 50000})
	if got := clock("a"); !strings.HasPrefix(got, "2026-10-20T10:01:00") {
		t.Fatalf("a after the advance = %s", got)
	}
	call("sandbox_instance", map[string]any{"op": "restore", "snapshot": snap.ID, "name": "a"})
	// Branch 2: the same snapshot beside it.
	call("sandbox_instance", map[string]any{"op": "restore", "snapshot": snap.ID, "name": "b"})

	for _, in := range []string{"a", "b"} {
		if got := clock(in); !strings.HasPrefix(got, "2026-10-20T09:59:00") {
			t.Errorf("%s restored at %s, want 09:59", in, got)
		}
		if got := call("sandbox_market", map[string]any{"op": "get", "instance": in, "venue": "binance", "symbol": "BTC/USDT"}); !strings.Contains(got, "98000") {
			t.Errorf("%s price after the restore: %s", in, got)
		}
		if got := call("sandbox_web", map[string]any{"op": "list", "instance": in}); !strings.Contains(got, "snap.example") {
			t.Errorf("%s lost the SimWeb rule: %s", in, got)
		}
		var j struct{ Entries []struct{ Seq int64 } }
		if err := json.Unmarshal([]byte(call("sandbox_inspect", map[string]any{"op": "journal", "instance": in, "after_seq": snap.JournalSeq - 1, "limit": 1})), &j); err != nil || len(j.Entries) != 1 || j.Entries[0].Seq != snap.JournalSeq {
			t.Errorf("%s journal after seq %d: %+v (%v)", in, snap.JournalSeq-1, j, err)
		}
	}
	// The branches do not share a clock.
	call("sandbox_clock", map[string]any{"op": "advance", "instance": "a", "by": "2m"})
	if got := clock("b"); !strings.HasPrefix(got, "2026-10-20T09:59:00") {
		t.Errorf("b moved to %s when a advanced", got)
	}
	if got := call("sandbox_instance", map[string]any{"op": "snapshots"}); !strings.Contains(got, snap.ID) {
		t.Errorf("snapshots = %s", got)
	}
}
