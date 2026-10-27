package gateway

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/egress"
)

// lockTestToken is the instance token of every locked test: 32+ characters, as
// lockOptionsFromEnv requires, and distinctive enough to grep for.
const lockTestToken = "kq-lock-test-token-5f3a9c71d2e84b60a1c7"

// repoSandboxFixtures finds workspace/sandbox by walking up to go.mod.
func repoSandboxFixtures(t *testing.T) string {
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

// copySandboxFixtures copies the repo's venue fixtures into dst and fails if none
// were copied, so a path bug names its real cause instead of failing later.
func copySandboxFixtures(t *testing.T, dst string) {
	t.Helper()
	src := repoSandboxFixtures(t)
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

// newInstanceHome makes a sandbox instance home: a temp dir with the marker and the
// venue fixtures under workspace/sandbox. It also points HOME at another temp dir so
// the "not the real ~/.khunquant" check never sees the developer's home.
func newInstanceHome(t *testing.T) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, instanceMarker), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	copySandboxFixtures(t, filepath.Join(home, "workspace", "sandbox"))
	return home
}

// setLockEnv sets the environment a locked gateway reads for the instance at home.
func setLockEnv(t *testing.T, home string) {
	t.Helper()
	t.Setenv("KHUNQUANT_HOME", home)
	t.Setenv("KHUNQUANT_CONFIG", filepath.Join(home, "config.json"))
	t.Setenv("KQ_SANDBOX_TOKEN", lockTestToken)
	t.Setenv("KQ_SANDBOX_INSTANCE", "test")
}

// lockProcessForTest locks this test process the way gatewayCmd does and undoes it
// (lock, global state and save guard) when the test ends.
func lockProcessForTest(t *testing.T, home string) sandbox.LockOptions {
	t.Helper()
	setLockEnv(t, home)
	sandbox.ResetLockForTest()
	egress.ResetForTest()
	t.Cleanup(egress.ResetForTest)
	t.Cleanup(func() { config.SetSaveGuard(nil) })
	t.Cleanup(sandbox.ResetLockForTest)

	lk, err := lockOptionsFromEnv()
	if err != nil {
		t.Fatalf("lockOptionsFromEnv: %v", err)
	}
	if err := armSandboxLock(lk); err != nil {
		t.Fatalf("armSandboxLock: %v", err)
	}
	return lk
}
