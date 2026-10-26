package gateway

import (
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/cmd/khunquant/internal/sandboxlab"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

// A locked gateway started exactly the way `khunquant sandbox up` starts it: the
// environment is sandboxlab.ChildEnv (HOME is the instance home, nothing else of the
// developer's shell), the config is the real sandbox profile, and the fixtures are
// the repository's. The simchat stub stands in for SBX-02's adapter, so the gateway
// starts fully. This is what shows that up's environment passes the lock checks.
func TestLockedGateway_StartsWithUpEnvAndProfile(t *testing.T) {
	home := newInstanceHome(t)
	port := freePort(t)
	if _, err := sandbox.WriteProfile(sandbox.ProfileOptions{Home: home, Port: port}); err != nil {
		t.Fatal(err)
	}

	// The test process has its own HOME (newInstanceHome set it); the child must not
	// see it, so ChildEnv is built after pointing TMPDIR at a scanned dir.
	childTmp := t.TempDir()
	t.Setenv("TMPDIR", childTmp)
	env := sandboxlab.ChildEnv(home, "upenv", lockTestToken)
	if !slices.Contains(env, "HOME="+home) {
		t.Fatalf("ChildEnv does not set HOME to the instance home: %v", env)
	}

	p := launchLockedProcess(t, home, port, env, []string{home, childTmp})
	p.waitReady()

	st := p.status()
	if !st.Enabled || !st.Locked || !st.Running || st.BaseURL == "" {
		t.Fatalf("status = %+v, want an enabled, locked, running sandbox", st)
	}
	// The gateway wrote its pid file where `down` looks for it.
	if _, err := os.Stat(filepath.Join(home, ".khunquant.pid")); err != nil {
		t.Errorf("no pid file in the instance home: %v", err)
	}
	if code, _, _ := p.get("/api/sandbox/status", "wrong-token"); code != http.StatusUnauthorized {
		t.Errorf("a wrong token got %d, want 401", code)
	}

	// Nothing in a locked run creates <home>/.khunquant: a restart of the same
	// instance must still pass the lock check.
	if _, err := os.Lstat(filepath.Join(home, ".khunquant")); !os.IsNotExist(err) {
		t.Errorf("the locked gateway created %s/.khunquant (err=%v)", home, err)
	}

	// Writing fixtures through the API (what sandbox_write_fixtures does) changes the
	// instance's copy and leaves the repository's fixtures byte-identical.
	repoFixtures := repoSandboxFixtures(t)
	before := fixtureSnapshot(t, repoFixtures)
	if len(before) == 0 {
		t.Fatalf("no fixtures found under %s", repoFixtures)
	}
	put := `[{"method":"GET","path_prefix":"/lab/ping","response":{"status":200,"body_text":"pong"}}]`
	req, err := http.NewRequest(http.MethodPut, fmt.Sprintf("http://127.0.0.1:%d/api/sandbox/fixtures?venue=okx", port), strings.NewReader(put))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+lockTestToken)
	resp, err := testHTTPClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT /api/sandbox/fixtures: %v (resp %v)\n%s", err, resp, p.out.String())
	}
	_ = resp.Body.Close()
	written, err := os.ReadFile(filepath.Join(home, "workspace", "sandbox", "okx", "fixtures.json"))
	if err != nil || string(written) != put {
		t.Errorf("the instance copy of the okx fixtures was not updated: %v", err)
	}
	if after := fixtureSnapshot(t, repoFixtures); !reflect.DeepEqual(before, after) {
		t.Error("the repository's workspace/sandbox changed")
	}

	// `down` stops it with SIGTERM.
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := p.wait(30 * time.Second); err != nil {
		t.Fatalf("SIGTERM did not stop the gateway cleanly: %v\n%s", err, p.out.String())
	}
}

// fixtureSnapshot maps every file under dir to its content, to show it unchanged.
func fixtureSnapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	snap := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, rerr := os.ReadFile(path)
		snap[path] = string(data)
		return rerr
	})
	if err != nil {
		t.Fatal(err)
	}
	return snap
}
