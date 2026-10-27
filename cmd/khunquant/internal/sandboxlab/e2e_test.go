package sandboxlab

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// repoRoot is the repository, found by walking up to go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test directory")
		}
		dir = parent
	}
}

// repoFixtures is the repository's workspace/sandbox, asserted non-empty.
func repoFixtures(t *testing.T) string {
	t.Helper()
	fx := filepath.Join(repoRoot(t), "workspace", "sandbox")
	entries, err := os.ReadDir(fx)
	if err != nil || len(entries) == 0 {
		t.Fatalf("the repository fixtures at %s are missing or empty: %v", fx, err)
	}
	return fx
}

// sandboxEnv gives a test a private HOME and TMPDIR, so the developer's real home is
// never read, and returns the base dir that instance homes are made under.
func sandboxEnv(t *testing.T) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	base := t.TempDir()
	t.Setenv("TMPDIR", base)
	t.Setenv("LANG", "C")
	t.Setenv("TZ", "UTC")
	return base
}

func getCode(t *testing.T, url, token string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// TestUpDown_LocalBinary drives the real binary: a locked gateway with the simchat
// channel starts, answers, and goes away. KQ_SANDBOX_E2E_BIN names a prebuilt
// khunquant; the test never compiles one.
func TestUpDown_LocalBinary(t *testing.T) {
	bin := os.Getenv("KQ_SANDBOX_E2E_BIN")
	if bin == "" {
		t.Skip("set KQ_SANDBOX_E2E_BIN to a prebuilt khunquant binary to run this test")
	}
	base := sandboxEnv(t) // private HOME and TMPDIR for the child commands
	run := func(args ...string) ([]byte, error) {
		cmd := exec.Command(bin, args...)
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "TMPDIR=" + base, "HOME=" + os.Getenv("HOME")}
		cmd.Dir = repoRoot(t) // ./workspace/sandbox resolves
		return cmd.CombinedOutput()
	}

	out, err := run("sandbox", "up", "--name", "e2e", "--json", "--fixtures-from", repoFixtures(t))
	if err != nil {
		t.Fatalf("sandbox up: %v\n%s", err, out)
	}
	var inst struct {
		Home       string `json:"home"`
		GatewayURL string `json:"gateway_url"`
		Token      string `json:"token"`
	}
	if err := json.Unmarshal(out, &inst); err != nil {
		t.Fatalf("up output is not the instance JSON: %v\n%s", err, out)
	}
	t.Cleanup(func() { _, _ = run("sandbox", "down", "e2e") })

	if c := getCode(t, inst.GatewayURL+"/health", ""); c != http.StatusOK {
		t.Fatalf("/health = %d", c)
	}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, inst.GatewayURL+"/api/sandbox/status", nil)
	req.Header.Set("Authorization", "Bearer "+inst.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var st struct {
		Locked bool     `json:"locked"`
		Venues []string `json:"venues"`
	}
	if err := json.Unmarshal(body, &st); err != nil || !st.Locked || len(st.Venues) < 1 {
		t.Fatalf("status = %s (%v), want locked with at least one venue", body, err)
	}

	if out, err := run("sandbox", "down", "e2e"); err != nil {
		t.Fatalf("sandbox down: %v\n%s", err, out)
	}
	if _, err := os.Stat(inst.Home); !os.IsNotExist(err) {
		t.Errorf("home survived down: %v", err)
	}
}
