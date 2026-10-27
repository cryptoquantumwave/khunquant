package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

// repoFixtures is the repository's workspace/sandbox, found by walking up to go.mod.
func repoFixtures(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test directory")
		}
		dir = parent
	}
	fx := filepath.Join(dir, "workspace", "sandbox")
	files, err := listFixtures(fx)
	if err != nil || len(files) == 0 {
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
	// Variables the instance must never inherit.
	t.Setenv("KHUNQUANT_HOME", filepath.Join(t.TempDir(), "decoy"))
	t.Setenv("KHUNQUANT_GATEWAY_PORT", "1")
	t.Setenv("KQ_SANDBOX_ENABLED", "false")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:3128")
	t.Setenv("OPENAI_API_KEY", "sk-must-not-leak")
	return base
}

func fakeUp(t *testing.T, base string, extra ...string) (*Instance, error) {
	t.Helper()
	return Up(context.Background(), UpOptions{
		Name:         "t" + strconv.Itoa(int(time.Now().UnixNano()%100000)),
		FixturesFrom: repoFixtures(t),
		BaseDir:      base,
		Bin:          os.Args[0],
		BinArgs:      fakeArgs(extra...),
		ReadyTimeout: 30 * time.Second,
	})
}

func getCode(t *testing.T, url, token string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
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

func TestUpDown_FakeGateway(t *testing.T) {
	base := sandboxEnv(t)
	inst, err := fakeUp(t, base)
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	t.Cleanup(func() { _ = Down(DownOptions{Name: inst.Name, BaseDir: base, TermTimeout: time.Second}) })

	if inst.Home != filepath.Join(base, "kq-sbx-"+inst.Name) || inst.PID <= 0 || !processAlive(inst.PID) {
		t.Fatalf("instance = %+v", inst)
	}
	if len(inst.Token) < 32 {
		t.Fatalf("token %q is shorter than the 32 characters the lock needs", inst.Token)
	}
	if inst.Log != filepath.Join(inst.Home, "gateway.log") || !strings.HasPrefix(inst.GatewayURL, "http://127.0.0.1:") {
		t.Fatalf("instance = %+v", inst)
	}

	// The gateway the instance points at is up, locked, and keyed by the token.
	if c := getCode(t, inst.GatewayURL+"/health", ""); c != http.StatusOK {
		t.Errorf("/health = %d", c)
	}
	if c := getCode(t, inst.GatewayURL+"/api/sandbox/status", inst.Token); c != http.StatusOK {
		t.Errorf("/api/sandbox/status with the token = %d", c)
	}
	if c := getCode(t, inst.GatewayURL+"/api/sandbox/status", "wrong"); c != http.StatusUnauthorized {
		t.Errorf("/api/sandbox/status with a wrong token = %d", c)
	}

	// The token is nowhere on disk: not in config, not in the pid file, not in the log.
	var files int
	err = filepath.WalkDir(inst.Home, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		files++
		data, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		if bytes.Contains(data, []byte(inst.Token)) {
			t.Errorf("the instance token is on disk in %s", p)
		}
		return nil
	})
	if err != nil || files < 4 {
		t.Fatalf("scan of %s: files=%d err=%v", inst.Home, files, err)
	}
	if _, err := os.Stat(filepath.Join(inst.Home, ".khunquant.pid")); err != nil {
		t.Errorf("the gateway's pid file is missing: %v", err)
	}

	// The token travels in the environment, never on the command line.
	cmdline, err := processCommand(inst.PID)
	if err != nil {
		t.Fatalf("ps: %v", err)
	}
	if strings.Contains(cmdline, inst.Token) {
		t.Errorf("the token is on the gateway's command line: %s", cmdline)
	}

	// The gateway got exactly the allowlisted environment.
	envKeys, err := os.ReadFile(filepath.Join(inst.Home, "env-keys.txt"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"HOME", "KHUNQUANT_CONFIG", "KHUNQUANT_HOME", "KQ_SANDBOX_INSTANCE", "KQ_SANDBOX_LOCKED",
		"KQ_SANDBOX_TOKEN", "LANG", "PATH", "TMPDIR", "TZ"}
	sort.Strings(want)
	if got := strings.Split(string(envKeys), "\n"); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("gateway environment keys = %v\nwant %v", got, want)
	}

	if err := Down(DownOptions{Name: inst.Name, BaseDir: base, TermTimeout: 10 * time.Second}); err != nil {
		t.Fatalf("Down: %v", err)
	}
	if _, err := os.Stat(inst.Home); !os.IsNotExist(err) {
		t.Errorf("the instance home survived Down: %v", err)
	}
	if processAlive(inst.PID) {
		t.Error("the gateway is still running after Down")
	}
}

func TestChildEnv_Allowlist(t *testing.T) {
	sandboxEnv(t)
	home := filepath.Join(t.TempDir(), "kq-sbx-x")
	got := map[string]string{}
	for _, kv := range ChildEnv(home, "x", "tok") {
		k, v, _ := strings.Cut(kv, "=")
		got[k] = v
	}
	want := map[string]string{
		"HOME": home, "KHUNQUANT_HOME": home, "KHUNQUANT_CONFIG": filepath.Join(home, "config.json"),
		"KQ_SANDBOX_LOCKED": "1", "KQ_SANDBOX_TOKEN": "tok", "KQ_SANDBOX_INSTANCE": "x",
		"PATH": os.Getenv("PATH"), "TMPDIR": os.Getenv("TMPDIR"), "LANG": "C", "TZ": "UTC",
	}
	if len(got) != len(want) {
		t.Errorf("env = %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("env[%s] = %q, want %q", k, got[k], v)
		}
	}
}

func TestPrepareHome_Layout(t *testing.T) {
	sandboxEnv(t)
	src := repoFixtures(t)
	fixtures, err := listFixtures(src)
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(t.TempDir(), "kq-sbx-layout")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := prepareHome(home, "layout", 18795, fixtures); err != nil {
		t.Fatal(err)
	}

	if info, err := os.Stat(filepath.Join(home, instanceMarker)); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("marker: %v", err)
	}
	for _, f := range fixtures {
		want, _ := os.ReadFile(f.path)
		got, err := os.ReadFile(filepath.Join(home, "workspace", "sandbox", f.venue, "fixtures.json"))
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("fixtures for %s were not copied faithfully: %v", f.venue, err)
		}
	}
	// The port is read from the file itself: sandboxEnv sets a decoy
	// KHUNQUANT_GATEWAY_PORT, which LoadConfig would apply over it (the reason the
	// instance gets an allowlisted environment).
	raw, err := os.ReadFile(filepath.Join(home, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var onDisk struct {
		Gateway struct {
			Port int `json:"port"`
		} `json:"gateway"`
	}
	if err := json.Unmarshal(raw, &onDisk); err != nil || onDisk.Gateway.Port != 18795 {
		t.Errorf("gateway.port on disk = %d (%v), want 18795", onDisk.Gateway.Port, err)
	}
	cfg, err := config.LoadConfig(filepath.Join(home, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Agents.Defaults.Workspace != filepath.Join(home, "workspace") ||
		cfg.Debug.Sandbox.FixturesDir != filepath.Join(home, "workspace", "sandbox") {
		t.Errorf("profile on disk: workspace=%q fixtures=%q",
			cfg.Agents.Defaults.Workspace, cfg.Debug.Sandbox.FixturesDir)
	}
	// Everything written is inside the home; the config's secrets file is there too.
	if _, err := os.Stat(filepath.Join(home, ".security.yml")); err != nil {
		t.Errorf(".security.yml is not in the instance home: %v", err)
	}
}

// An empty fixtures source is refused before anything is created or started.
func TestFixtureCopy_FailsWhenEmpty(t *testing.T) {
	base := sandboxEnv(t)
	empty := t.TempDir()
	if err := os.MkdirAll(filepath.Join(empty, "binance"), 0o755); err != nil { // a venue dir with no fixtures.json
		t.Fatal(err)
	}
	_, err := Up(context.Background(), UpOptions{
		Name: "empty", FixturesFrom: empty, BaseDir: base,
		Bin: "/nonexistent/khunquant", // never reached
	})
	if err == nil || !strings.Contains(err.Error(), "fixtures.json") {
		t.Fatalf("Up with empty fixtures: err = %v, want a fixtures error", err)
	}
	if left, _ := os.ReadDir(base); len(left) != 0 {
		t.Errorf("a refused start left %d entries behind in the base dir", len(left))
	}
	if _, err := Up(context.Background(), UpOptions{Name: "missing", FixturesFrom: filepath.Join(empty, "nope"), BaseDir: base}); err == nil {
		t.Error("a missing fixtures dir must fail too")
	}
}

func TestListFixtures_IgnoresLinksAndDotDirs(t *testing.T) {
	dir := t.TempDir()
	mk := func(rel, content string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("okx/fixtures.json", "{}")
	mk(".hidden/fixtures.json", "{}")
	mk("stray.txt", "x")
	if err := os.Symlink(filepath.Join(dir, "okx"), filepath.Join(dir, "linked")); err != nil {
		t.Skip("no symlinks here:", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "bitkub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "okx", "fixtures.json"), filepath.Join(dir, "bitkub", "fixtures.json")); err != nil {
		t.Fatal(err)
	}
	files, err := listFixtures(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].venue != "okx" {
		t.Errorf("fixtures = %+v, want only okx", files)
	}
}

func TestListFixtures_DefaultFromWorkingDirectory(t *testing.T) {
	sandboxEnv(t)
	src := repoFixtures(t)
	t.Chdir(filepath.Dir(filepath.Dir(src))) // the repository root
	files, err := listFixtures("")
	if err != nil || len(files) == 0 {
		t.Fatalf("default fixtures from the repo root: %v (%d files)", err, len(files))
	}
	// Outside any repository (and with the test binary in the build cache) there is
	// nothing to find, and the search fails instead of guessing.
	t.Chdir(t.TempDir())
	if _, err := listFixtures(""); err == nil {
		t.Error("listFixtures found fixtures from an empty temp dir")
	}
}

func TestValidateName(t *testing.T) {
	base := t.TempDir()
	for _, ok := range []string{"x", "h8", "my-lab-1", "sbx-0a1b2c3d"} {
		if _, err := instanceHome(base, ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "../x", "a/b", "A", "-x", "x y", ".hidden", strings.Repeat("a", 32), "a.b"} {
		if _, err := instanceHome(base, bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestInstanceHome_RefusesRealHome(t *testing.T) {
	sandboxEnv(t)
	userHome, _ := os.UserHomeDir()
	for _, base := range []string{filepath.Join(userHome, ".khunquant"), filepath.Join(userHome, ".khunquant", "deep")} {
		if _, err := instanceHome(base, "x"); err == nil {
			t.Errorf("a home under %s was accepted", base)
		}
	}
	if _, err := instanceHome(t.TempDir(), "x"); err != nil {
		t.Errorf("a home under a temp dir was refused: %v", err)
	}
}

func TestUp_RefusesExistingInstance(t *testing.T) {
	base := sandboxEnv(t)
	if err := os.Mkdir(filepath.Join(base, "kq-sbx-dup"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := Up(context.Background(), UpOptions{Name: "dup", FixturesFrom: repoFixtures(t), BaseDir: base, Bin: os.Args[0], BinArgs: fakeArgs()})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("err = %v, want already exists", err)
	}
	if _, serr := os.Stat(filepath.Join(base, "kq-sbx-dup")); serr != nil {
		t.Error("Up removed a directory it did not create")
	}
}

// A start that fails leaves nothing behind: no home, no process.
func TestUp_FailureCleansUp(t *testing.T) {
	cases := []struct {
		name  string
		extra []string
		want  string
	}{
		{"gateway exits early", []string{"--exit-early"}, "boom"},
		{"gateway is not locked", []string{"--not-locked"}, "locked"},
		{"gateway never answers", []string{"--hang"}, "not ready"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := sandboxEnv(t)
			pidOut := filepath.Join(t.TempDir(), "pid")
			_, err := Up(context.Background(), UpOptions{
				Name: "fail", FixturesFrom: repoFixtures(t), BaseDir: base,
				Bin: os.Args[0], BinArgs: fakeArgs(append(tc.extra, "--pidout="+pidOut)...),
				ReadyTimeout: 1500 * time.Millisecond,
			})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
			if left, _ := os.ReadDir(base); len(left) != 0 {
				t.Errorf("a failed start left %d entries behind", len(left))
			}
			if raw, rerr := os.ReadFile(pidOut); rerr == nil {
				p, _ := strconv.Atoi(string(raw))
				deadline := time.Now().Add(5 * time.Second)
				for processAlive(p) && time.Now().Before(deadline) {
					time.Sleep(50 * time.Millisecond)
				}
				if processAlive(p) {
					t.Errorf("the gateway (pid %d) was left running after a failed start", p)
				}
			}
		})
	}
}

func TestUp_ContextCancelStopsAndCleans(t *testing.T) {
	base := sandboxEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	_, err := Up(ctx, UpOptions{
		Name: "cancel", FixturesFrom: repoFixtures(t), BaseDir: base,
		Bin: os.Args[0], BinArgs: fakeArgs("--hang"), ReadyTimeout: time.Minute,
	})
	if err == nil || !strings.Contains(err.Error(), "interrupted") {
		t.Fatalf("err = %v, want interrupted", err)
	}
	if left, _ := os.ReadDir(base); len(left) != 0 {
		t.Errorf("an interrupted start left %d entries behind", len(left))
	}
}
