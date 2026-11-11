package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/pid"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labsnap"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/simweb"
)

// An ordinary gateway has no snapshot route and no sandbox_snapshot tool.
func TestSnapshotGroup_NotRegisteredWhenUnlocked(t *testing.T) {
	f := newAssemblyFixture(t, enableAllRoutes)
	if code, body, err := postJSON(f.url("/api/sandbox/lab/snapshot"), testSandboxToken, `{"op":"take","id":"x"}`); err != nil || code != http.StatusNotFound {
		t.Fatalf("POST /api/sandbox/lab/snapshot = %d (%v) %s, want 404", code, err, body)
	}
	if mcpTools(t, f, testDevMCPToken)["sandbox_snapshot"] {
		t.Fatal("an unlocked gateway lists sandbox_snapshot")
	}
}

// copyTree copies the regular files under src to dst, as a restore does.
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dst, rel)), 0o700); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), b, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A locked, settled instance with a stopped clock snapshots into <home>/lab/snapshots: the
// manifest sums verify, the pid file, marker, log, crash files and older snapshots stay out,
// the token is nowhere in it, and a lab started on a copy of it resumes the same world: the
// same exchange state, the next order ID above the stored ones, and SimWeb's rules.
func TestSnapshotTake_LockedGateway(t *testing.T) {
	t.Setenv(lab.EnvClockStart, "2026-03-10T09:59:00Z")
	t.Setenv(lab.EnvClockMode, "manual")
	f := newLockedFixture(t)
	home := f.lab.Home
	if code, out := labPost(t, f, "market", `{"op":"set","venue":"binance","symbol":"BTC/USDT","price":98000}`); code != http.StatusOK {
		t.Fatalf("market set = %d %v", code, out)
	}
	if code, out := labPost(t, f, "web", `{"op":"put","url_pattern":"snap.example/*","status":200,"body":"hello"}`); code != http.StatusOK {
		t.Fatalf("web put = %d %v", code, out)
	}
	_, first := postOrder(t, f.lab.Server.BaseURL(), "binance", "fapi.binance.com", "/fapi/v1/order?symbol=BTCUSDT&side=BUY&type=market&quantity=0.001")
	firstID, _ := first["orderId"].(float64)
	if firstID == 0 {
		t.Fatalf("the first order answered %v", first)
	}
	// Files a snapshot must leave out.
	if _, err := pid.WritePidFile(home, "127.0.0.1", 1); err != nil {
		t.Fatal(err)
	}
	for rel, body := range map[string]string{".kq-sandbox": "a\n", "gateway.log": "log " + lockTestToken, "lab/crash.json": "{}", "lab/crash-1.json": "{}", "lab/restore/x": "x"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(home, rel)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, rel), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("/etc/hosts", filepath.Join(home, "workspace", "link")); err != nil {
		t.Fatal(err)
	}

	for _, bad := range []string{`{"op":"take","id":"../x"}`, `{"op":"take","id":"a/b"}`, `{"op":"take"}`} {
		if code, out := labPost(t, f, "snapshot", bad); code != http.StatusBadRequest {
			t.Errorf("%s = %d %v, want 400", bad, code, out)
		}
	}
	code, out := labPost(t, f, "snapshot", `{"op":"take","id":"t-1","label":"first"}`)
	if code != http.StatusOK || out["id"] != "t-1" {
		t.Fatalf("take = %d %v", code, out)
	}
	dir := filepath.Join(f.lab.SnapshotsDir(), "t-1")
	m, err := labsnap.Verify(dir, config.FormatVersion())
	if err != nil {
		t.Fatal(err)
	}
	if m.SimTime.Format("15:04") != "09:59" || m.JournalSeq == 0 || m.Home != home || len(m.Parts) < 3 {
		t.Errorf("manifest = %+v", m)
	}
	have := map[string]bool{}
	for _, e := range m.Files {
		have[e.Path] = true
	}
	for _, want := range []string{"workspace/memory/dca/dca.db", "lab/clock.json", "lab/web.json", "lab/journal.db", "lab/boot", "lab/world/exchange.json", "lab/world/market.json", "lab/world/faults.json"} {
		if !have[want] {
			t.Errorf("the snapshot lacks %s: %v", want, m.Files)
		}
	}
	for p := range have {
		if p == "gateway.log" || p == ".khunquant.pid" || p == ".kq-sandbox" || strings.HasPrefix(p, "lab/snapshots") || strings.HasPrefix(p, "lab/crash") || strings.HasPrefix(p, "lab/restore") || p == "workspace/link" || strings.HasSuffix(p, "-wal") || strings.HasSuffix(p, "-shm") {
			t.Errorf("the snapshot took %s", p)
		}
	}
	// A second take does not hold the first one.
	if code, _ := labPost(t, f, "snapshot", `{"op":"take","id":"t-2"}`); code != http.StatusOK {
		t.Fatal("second take failed")
	}
	m2, err := labsnap.Verify(filepath.Join(f.lab.SnapshotsDir(), "t-2"), config.FormatVersion())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range m2.Files {
		if strings.HasPrefix(e.Path, "lab/snapshots") {
			t.Errorf("t-2 holds %s", e.Path)
		}
	}
	// The token is nowhere in the snapshot.
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if b, _ := os.ReadFile(p); bytes.Contains(b, []byte(lockTestToken)) {
				t.Errorf("the instance token is in %s", p)
			}
		}
		return nil
	})
	if _, err := os.Stat(filepath.Join(f.workspace, "..", "lab", "snapshots")); err != nil {
		t.Errorf("the snapshot is not under <home>/lab: %v", err)
	}

	// A lab started on a copy of the snapshot continues the same world.
	home2 := t.TempDir()
	copyTree(t, filepath.Join(dir, labsnap.HomeDir), home2)
	want, err := os.ReadFile(filepath.Join(dir, labsnap.HomeDir, "lab", "world", "exchange.json"))
	if err != nil {
		t.Fatal(err)
	}
	l2, err := lab.New(context.Background(), lab.Options{
		Home: home2, FixturesDir: filepath.Join(home2, "workspace", "sandbox"), Token: lockTestToken + "-b", Getenv: os.Getenv,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l2.Close() })
	got, err := l2.Sim.MarshalState()
	if err != nil {
		t.Fatal(err)
	}
	var a, b any
	if json.Unmarshal(want, &a) != nil || json.Unmarshal(got, &b) != nil || !jsonEqual(a, b) {
		t.Errorf("the restored exchange differs from the snapshot's\n want %s\n  got %s", want, got)
	}
	_, next := postOrder(t, l2.Server.BaseURL(), "binance", "fapi.binance.com", "/fapi/v1/order?symbol=BTCUSDT&side=BUY&type=market&quantity=0.001")
	if id, _ := next["orderId"].(float64); id <= firstID {
		t.Errorf("the restored world minted order %v after %v", next["orderId"], firstID)
	}
	w, err := simweb.Open(simweb.Options{Path: filepath.Join(home2, "lab", "web.json")})
	if err != nil {
		t.Fatal(err)
	}
	if pages, _ := w.List(); len(pages) != 1 {
		t.Errorf("the restored SimWeb holds %d page rules, want 1", len(pages))
	}
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

// A running clock, and a live or record instance, are refused with the reason named.
func TestSnapshotTake_Refusals(t *testing.T) {
	f := newLockedFixture(t) // the default clock is realtime
	code, out := labPost(t, f, "snapshot", `{"op":"take","id":"r-1"}`)
	if code == http.StatusOK || !strings.Contains(toString(out), "manual mode") {
		t.Fatalf("a realtime clock: %d %v", code, out)
	}
	if _, err := os.Stat(filepath.Join(f.lab.SnapshotsDir(), "r-1")); err == nil {
		t.Error("a refused take left a directory")
	}
	for _, mode := range []string{"live", "record"} {
		t.Setenv(sandbox.EnvLLM, mode)
		if code, out := labPost(t, f, "snapshot", `{"op":"take","id":"r-2"}`); code == http.StatusOK || !strings.Contains(toString(out), "refused for a "+mode) {
			t.Errorf("%s instance: %d %v", mode, code, out)
		}
	}
}

func toString(v any) string { b, _ := json.Marshal(v); return string(b) }
