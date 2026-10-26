package lab

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

const testToken = "t-0123456789abcdef0123456789abcdef"

// repoFixtures finds workspace/sandbox by walking up to go.mod.
func repoFixtures(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return filepath.Join(dir, "workspace", "sandbox")
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test directory")
		}
		dir = parent
	}
}

// copyFixtures copies the repo's venue fixtures to dst and fails if none were copied.
func copyFixtures(t *testing.T, dst string) {
	t.Helper()
	src := repoFixtures(t)
	venues, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, v := range venues {
		if !v.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(src, v.Name(), "fixtures.json"))
		if err != nil {
			continue
		}
		if err := os.MkdirAll(filepath.Join(dst, v.Name()), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, v.Name(), "fixtures.json"), data, 0o644); err != nil {
			t.Fatal(err)
		}
		n++
	}
	if n == 0 {
		t.Fatalf("no fixtures copied from %s", src)
	}
}

func newTestLab(t *testing.T) *Lab {
	t.Helper()
	home := t.TempDir()
	fixtures := filepath.Join(home, "workspace", "sandbox")
	copyFixtures(t, fixtures)
	t.Cleanup(func() { sandbox.SetGlobalState(false, "") })

	l, err := New(context.Background(), Options{Home: home, FixturesDir: fixtures, Token: testToken})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

func TestNew_StartsSeededServer(t *testing.T) {
	l := newTestLab(t)

	if l.Token() != testToken {
		t.Fatal("token not kept")
	}
	if l.Server == nil || !l.Server.IsRunning() || l.Server.BaseURL() == "" {
		t.Fatal("sandbox server is not running")
	}
	if got := sandbox.GetInstance(); got != l.Server {
		t.Fatal("the lab server is not the global sandbox instance")
	}
	if en, base := sandbox.GlobalState(); !en || base != l.Server.BaseURL() {
		t.Fatalf("GlobalState = (%v,%q), want the lab server", en, base)
	}
	if len(l.Store.Venues()) == 0 {
		t.Fatal("no venues")
	}
	var seeded bool
	for _, v := range l.Store.Venues() {
		if b := l.State.GetState(v).Balances["USDT"]; b.Free == 100000 {
			seeded = true
		}
	}
	if !seeded {
		t.Fatal("simulator state was not seeded with the dev USDT balance")
	}
}

func TestNew_Errors(t *testing.T) {
	t.Cleanup(func() { sandbox.SetGlobalState(false, "") })
	home := t.TempDir()
	fixtures := filepath.Join(home, "fx")
	copyFixtures(t, fixtures)

	cases := []struct {
		name string
		opts Options
	}{
		{"no token", Options{Home: home, FixturesDir: fixtures}},
		{"fixtures outside home", Options{Home: home, FixturesDir: repoFixtures(t), Token: testToken}},
		{"fixtures dir missing (zero venues)", Options{Home: home, FixturesDir: filepath.Join(home, "nope"), Token: testToken}},
		{"fixtures dir empty (zero venues)", Options{Home: home, FixturesDir: t.TempDir(), Token: testToken}},
	}
	for _, tc := range cases {
		if tc.name == "fixtures dir empty (zero venues)" {
			tc.opts.Home = tc.opts.FixturesDir
		}
		if l, err := New(context.Background(), tc.opts); err == nil {
			_ = l.Close()
			t.Errorf("%s: New succeeded, want an error", tc.name)
		}
	}
}

func TestClose_IdempotentRunsTeardown(t *testing.T) {
	l := newTestLab(t)

	var order []int
	l.OnClose(func() error { order = append(order, 1); return nil })
	l.OnClose(func() error { order = append(order, 2); return nil })

	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if len(order) != 2 || order[0] != 2 || order[1] != 1 {
		t.Fatalf("teardown order = %v, want [2 1] exactly once", order)
	}
	if l.Server.IsRunning() {
		t.Fatal("server still running after Close")
	}
	if sandbox.GetInstance() == l.Server {
		t.Fatal("Close left the stopped server as the global instance")
	}
}
