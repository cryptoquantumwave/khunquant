//go:build !windows

package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labsnap"
)

const simAt = "2026-10-20T09:59:00+07:00"

// fakeLabGroup answers the clock and snapshot groups for the fake gateway: the clock from
// <home>/lab/clock.json (a file the snapshot carries), and a take that writes a snapshot of
// the fake's own home the way the lab does (no log, pid file, marker or snapshots).
func fakeLabGroup(home, group string, body []byte, mode string) (any, error) {
	switch group {
	case "clock":
		now := simAt
		if b, err := os.ReadFile(filepath.Join(home, "lab", "clock.json")); err == nil {
			now = strings.TrimSpace(string(b))
		}
		return map[string]string{"now": now, "mode": mode0(mode)}, nil
	case "snapshot":
		var a struct{ ID, Label string }
		if err := json.Unmarshal(body, &a); err != nil {
			return nil, err
		}
		return nil, fakeTake(home, a.ID, a.Label)
	}
	return nil, fmt.Errorf("no group %q", group)
}

func mode0(m string) string {
	if m == "" {
		return "manual"
	}
	return m
}

func fakeTake(home, id, label string) error {
	dir := filepath.Join(home, "lab", "snapshots", id)
	sim, _ := time.Parse(time.RFC3339, simAt)
	m := labsnap.Manifest{ID: id, Label: label, Instance: os.Getenv("KQ_SANDBOX_INSTANCE"), Home: home, BinaryVersion: config.FormatVersion(),
		SimTime: sim, CreatedWall: time.Now(), LLM: os.Getenv("KQ_SANDBOX_LLM"), Seed: os.Getenv("KQ_SANDBOX_SEED"), Parts: []string{}}
	err := filepath.WalkDir(home, func(p string, d fs.DirEntry, err error) error {
		rel, _ := filepath.Rel(home, p)
		rel = filepath.ToSlash(rel)
		if err != nil || d.IsDir() || !d.Type().IsRegular() ||
			rel == "gateway.log" || rel == ".kq-sandbox" || rel == ".khunquant.pid" || strings.HasPrefix(rel, "lab/snapshots") {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		dst := filepath.Join(dir, labsnap.HomeDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(dst, b, 0o600); err != nil {
			return err
		}
		sum, n, _ := labsnap.SumFile(dst)
		m.Files = append(m.Files, labsnap.FileEntry{Path: rel, SHA256: sum, Bytes: n})
		return nil
	})
	if err != nil {
		return err
	}
	b, _ := json.Marshal(m)
	return os.WriteFile(filepath.Join(dir, labsnap.ManifestName), b, 0o600)
}

func snapSupervisor(t *testing.T, max int, extra ...string) (*Supervisor, string) {
	t.Helper()
	base := t.TempDir()
	cfg := fakeConfig(base, append([]string{"--snapshot-lab"}, extra...)...)
	cfg.MaxInstances = max
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s, base
}

func put(t *testing.T, home, rel, body string) {
	t.Helper()
	p := filepath.Join(home, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// setup is an instance a with state in it: a clock file, a boot count, and a text file
// that names its own home.
func setup(t *testing.T, s *Supervisor, name string, o UpOptions) Info {
	t.Helper()
	o.Name = name
	o.FixturesFrom = repoFixtures(t)
	info, err := s.Up(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	put(t, info.Home, "lab/clock.json", simAt)
	put(t, info.Home, "lab/boot", "3\n")
	put(t, info.Home, "workspace/notes.json", `{"home":"`+info.Home+`","file":"`+info.Home+`/workspace/x"}`)
	return info
}

func tree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := os.ReadFile(p)
			rel, _ := filepath.Rel(root, p)
			out[filepath.ToSlash(rel)] = string(b)
		}
		return nil
	})
	return out
}

// A snapshot lands in the store outside the home and survives down; a restore verifies it,
// brings the same sim time and files back, and never carries the token or the log.
func TestSupervisor_SnapshotRestoreRoundTrip(t *testing.T) {
	s, base := snapSupervisor(t, 1, "--echo-token")
	a := setup(t, s, "a", UpOptions{})
	si, err := s.Snapshot(context.Background(), "", "first")
	if err != nil {
		t.Fatal(err)
	}
	id := si.Manifest.ID
	if want := "a-20261020T0959-1"; id != want || si.Dir != filepath.Join(base, "kq-sbx-snapshots", id) || si.Manifest.Label != "first" {
		t.Fatalf("snapshot = %+v", si)
	}
	if _, err := os.Stat(filepath.Join(a.Home, "lab", "snapshots", id)); err == nil {
		t.Error("the snapshot is still in the instance home")
	}
	for p, body := range tree(t, si.Dir) { // the token is never in a snapshot (the log holds it, and is left out)
		if strings.Contains(body, a.RevealToken()) {
			t.Errorf("the token is in %s", p)
		}
	}
	if si2, err := s.Snapshot(context.Background(), "a", ""); err != nil || si2.Manifest.ID != "a-20261020T0959-2" {
		t.Fatalf("second snapshot = %+v, %v", si2.Manifest.ID, err)
	}
	if err := s.Down(context.Background(), "a", false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(si.Dir); err != nil {
		t.Fatalf("down removed the snapshot: %v", err)
	}
	if list, err := s.ListSnapshots(); err != nil || len(list) != 2 {
		t.Fatalf("list = %d, %v", len(list), err)
	}

	info, err := s.Restore(context.Background(), id, "")
	if err != nil || info.State != StateRunning || info.Name != "a" {
		t.Fatalf("restore = %+v, %v", info, err)
	}
	got := tree(t, info.Home)
	if got["lab/boot"] != "3\n" || got["lab/clock.json"] != simAt {
		t.Errorf("restored files: boot %q clock %q", got["lab/boot"], got["lab/clock.json"])
	}
	if strings.Contains(got["gateway.log"], a.RevealToken()) {
		t.Error("the old token is in the restored home's log")
	}
	if err := s.DeleteSnapshot(id); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSnapshot("../a"); err == nil {
		t.Error("a snapshot id with .. was accepted")
	}
}

// a's snapshot restored as b while a runs: paths in b's files are b's, none is a's, the
// port is b's own, b has its own marker, and a is untouched.
func TestRestore_IntoNewNameRewritesPaths(t *testing.T) {
	s, _ := snapSupervisor(t, 2)
	a := setup(t, s, "a", UpOptions{Accounts: []string{"binance"}, Seed: "77"})
	si, err := s.Snapshot(context.Background(), "a", "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Restore(context.Background(), si.Manifest.ID, "b")
	if err != nil {
		t.Fatal(err)
	}
	if b.Home == a.Home || b.State != StateRunning {
		t.Fatalf("b = %+v", b)
	}
	if a2, _ := s.Get("a"); a2.State != StateRunning {
		t.Errorf("a is %s after restoring b", a2.State)
	}
	files := tree(t, b.Home)
	for p, body := range files {
		if p != "gateway.log" && strings.Contains(body, a.Home) {
			t.Errorf("%s still names a's home", p)
		}
	}
	var notes struct{ Home, File string }
	_ = json.Unmarshal([]byte(files["workspace/notes.json"]), &notes)
	if notes.Home != b.Home || notes.File != b.Home+"/workspace/x" {
		t.Errorf("notes = %+v, want b's home %s", notes, b.Home)
	}
	var cfg struct {
		Gateway struct{ Port int }
		Agents  struct{ Defaults struct{ Workspace string } }
		Debug   struct{ Sandbox struct{ FixturesDir string `json:"fixtures_dir"` } }
	}
	if err := json.Unmarshal([]byte(files["config.json"]), &cfg); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(b.GatewayURL, fmt.Sprint(":", cfg.Gateway.Port)) || cfg.Gateway.Port == 0 {
		t.Errorf("port %d is not b's (%s)", cfg.Gateway.Port, b.GatewayURL)
	}
	if cfg.Agents.Defaults.Workspace != filepath.Join(b.Home, "workspace") || cfg.Debug.Sandbox.FixturesDir != filepath.Join(b.Home, "workspace", "sandbox") {
		t.Errorf("config paths: %+v", cfg)
	}
	if strings.TrimSpace(files[".kq-sandbox"]) != "b" || files[".sandbox-accounts"] != `["binance"]` {
		t.Errorf("marker %q accounts %q", files[".kq-sandbox"], files[".sandbox-accounts"])
	}
	if !strings.Contains(files["facts.txt"], "instance=b\nseed=77\n") { // the seed of a, not one derived from "b"
		t.Errorf("b's facts: %q, want a's seed 77", files["facts.txt"])
	}
}

// A snapshot that was changed, extended, or taken by another binary is refused before any
// home is made.
func TestSupervisor_RestoreRefusesTamperedOrForeign(t *testing.T) {
	s, base := snapSupervisor(t, 2)
	setup(t, s, "a", UpOptions{})
	si, err := s.Snapshot(context.Background(), "a", "")
	if err != nil {
		t.Fatal(err)
	}
	restore := func() error { _, err := s.Restore(context.Background(), si.Manifest.ID, "b"); return err }
	edit := func(f func(*labsnap.Manifest)) {
		m, _ := labsnap.Read(si.Dir)
		f(&m)
		b, _ := json.Marshal(m)
		_ = os.WriteFile(filepath.Join(si.Dir, labsnap.ManifestName), b, 0o600)
	}
	check := func(what string, want error) {
		t.Helper()
		err := restore()
		if err == nil || (want != nil && !errors.Is(err, want)) {
			t.Errorf("%s: err = %v, want %v", what, err, want)
		}
		if _, serr := os.Stat(filepath.Join(base, "kq-sbx-b")); serr == nil {
			t.Errorf("%s: a home was made for a refused restore", what)
		}
	}
	orig, _ := os.ReadFile(filepath.Join(si.Dir, labsnap.ManifestName))
	reset := func() { _ = os.WriteFile(filepath.Join(si.Dir, labsnap.ManifestName), orig, 0o600) }

	edit(func(m *labsnap.Manifest) { m.BinaryVersion = "other-build" })
	check("version", ErrVersionMismatch)
	reset()
	boot := filepath.Join(si.Dir, labsnap.HomeDir, "lab", "boot")
	_ = os.WriteFile(boot, []byte("99\n"), 0o600)
	check("changed file", labsnap.ErrMismatch)
	_ = os.WriteFile(boot, []byte("3\n"), 0o600)
	extra := filepath.Join(si.Dir, labsnap.HomeDir, "extra")
	_ = os.WriteFile(extra, []byte("x"), 0o600)
	check("unlisted file", labsnap.ErrMismatch)
	_ = os.Remove(extra)
	edit(func(m *labsnap.Manifest) { m.Files[0].Path = "../escape" })
	check("path outside the home", labsnap.ErrMismatch)
	reset()
	edit(func(m *labsnap.Manifest) { m.LLM = "live" })
	check("live manifest", ErrMoneyAtRisk)
	reset()
	edit(func(m *labsnap.Manifest) { m.Files = append(m.Files, labsnap.FileEntry{Path: "lab/budget.json"}) })
	check("budget file", nil)
	reset()
	if _, err := s.Restore(context.Background(), "../x", "b"); err == nil {
		t.Error("a snapshot id with .. was accepted")
	}
	if err := restore(); err != nil {
		t.Fatalf("the untouched snapshot no longer restores: %v", err)
	}
}

// A running clock, and a live or record instance, are refused.
func TestSupervisor_SnapshotRefusals(t *testing.T) {
	s, _ := snapSupervisor(t, 1, "--clock-mode=scaled")
	setup(t, s, "a", UpOptions{})
	if _, err := s.Snapshot(context.Background(), "a", ""); !errors.Is(err, ErrClockRunning) {
		t.Fatalf("scaled clock: err = %v, want ErrClockRunning", err)
	}
	if err := s.Down(context.Background(), "a", false); err != nil {
		t.Fatal(err)
	}

	s2, _ := snapSupervisor(t, 1)
	s2.cfg.Getenv = shell(liveShell)
	_, err := s2.Up(context.Background(), UpOptions{Name: "r", FixturesFrom: repoFixtures(t), LLM: "record", LLMModel: liveModel, LLMBudget: "2USD", LLMAPIBase: "http://127.0.0.1:9"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s2.Snapshot(context.Background(), "r", ""); !errors.Is(err, ErrMoneyAtRisk) {
		t.Fatalf("record instance: err = %v, want ErrMoneyAtRisk", err)
	}
}
