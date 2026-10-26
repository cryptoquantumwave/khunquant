package sandboxlab

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

const (
	// instanceMarker is the file lockOptionsFromEnv requires in KHUNQUANT_HOME.
	instanceMarker = ".kq-sandbox"
	homePrefix     = "kq-sbx-" // instance home: <base>/kq-sbx-<name>

	defaultReadyTimeout = 60 * time.Second
)

// nameRE keeps an instance name a single plain path element.
var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}$`)

// UpOptions configures Up. Only Name and FixturesFrom come from the command line.
type UpOptions struct {
	Name         string // random when empty
	FixturesFrom string // dir holding <venue>/fixtures.json; ./workspace/sandbox when empty

	BaseDir      string        // parent of the instance home; os.TempDir() when empty
	Bin          string        // gateway binary; this executable when empty
	BinArgs      []string      // "gateway --sandbox-locked" when empty
	ReadyTimeout time.Duration // 60s when zero
}

// Instance is what `up` reports. Token is shown here and nowhere else.
type Instance struct {
	Name       string `json:"name"`
	Home       string `json:"home"`
	PID        int    `json:"pid"`
	GatewayURL string `json:"gateway_url"`
	Token      string `json:"token"`
	Log        string `json:"log"`
}

// Up makes an instance home, starts a locked gateway in it and returns once the
// gateway reports an enabled, locked sandbox. On any failure it stops the gateway and
// removes the home it made.
func Up(ctx context.Context, opts UpOptions) (*Instance, error) {
	name := opts.Name
	if name == "" {
		name = "sbx-" + randomHex(4)
	}
	home, err := instanceHome(opts.BaseDir, name)
	if err != nil {
		return nil, err
	}
	// What can fail before a directory exists comes first: a refused start leaves nothing.
	fixtures, err := listFixtures(opts.FixturesFrom)
	if err != nil {
		return nil, err
	}
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	token := randomHex(32)
	bin, args := opts.Bin, opts.BinArgs
	if bin == "" {
		if bin, err = os.Executable(); err != nil {
			return nil, fmt.Errorf("sandbox up: locate the khunquant binary: %w", err)
		}
	}
	if len(args) == 0 {
		args = []string{"gateway", "--sandbox-locked"}
	}

	if err := os.Mkdir(home, 0o700); err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("sandbox up: instance %q already exists at %s; run `khunquant sandbox down %s`", name, home, name)
		}
		return nil, fmt.Errorf("sandbox up: create %s: %w", home, err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(home)
		}
	}()
	if err := prepareHome(home, name, port, fixtures); err != nil {
		return nil, err
	}

	cmd, exited, logPath, err := spawn(home, name, token, bin, args)
	if err != nil {
		return nil, err
	}
	url := fmt.Sprintf("http://127.0.0.1:%d", port)
	timeout := opts.ReadyTimeout
	if timeout <= 0 {
		timeout = defaultReadyTimeout
	}
	if err := waitReady(ctx, url, token, exited, timeout); err != nil {
		killGroup(cmd.Process.Pid)
		_ = cmd.Process.Kill()
		<-exited
		return nil, fmt.Errorf("sandbox up: %w%s", err, logTail(logPath, token))
	}
	ok = true
	return &Instance{Name: name, Home: home, PID: cmd.Process.Pid, GatewayURL: url, Token: token, Log: logPath}, nil
}

// instanceHome is where the instance called name lives under base (os.TempDir() when
// empty). It validates the name and refuses a place that overlaps the real
// ~/.khunquant: the gateway cannot check that itself, because it runs with HOME set
// to the instance home.
func instanceHome(base, name string) (string, error) {
	if !nameRE.MatchString(name) {
		return "", fmt.Errorf("sandbox: invalid instance name %q (lower-case letters, digits and '-', at most 31 characters)", name)
	}
	if base == "" {
		base = os.TempDir()
	}
	base, err := filepath.Abs(base)
	if err != nil {
		return "", err
	}
	home := filepath.Join(base, homePrefix+name)
	if userHome, err := os.UserHomeDir(); err == nil {
		real := filepath.Join(userHome, ".khunquant")
		if sandbox.PathInside(real, home) || sandbox.PathInside(home, real) {
			return "", fmt.Errorf("sandbox: %s overlaps the real %s", home, real)
		}
	}
	return home, nil
}

// fixtureFile is one <venue>/fixtures.json to copy.
type fixtureFile struct{ venue, path string }

// listFixtures lists <dir>/<venue>/fixtures.json for each venue directory (dir
// defaults to ./workspace/sandbox). Links and dot directories are skipped. It errors
// when there is none, so `up` can refuse before it creates anything.
func listFixtures(dir string) ([]fixtureFile, error) {
	if dir == "" {
		dir = filepath.Join("workspace", "sandbox")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("sandbox up: fixtures dir: %w", err)
	}
	var files []fixtureFile
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		p := filepath.Join(dir, e.Name(), "fixtures.json")
		if info, err := os.Lstat(p); err == nil && info.Mode().IsRegular() {
			files = append(files, fixtureFile{venue: e.Name(), path: p})
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("sandbox up: no <venue>/fixtures.json under %s", dir)
	}
	return files, nil
}

// prepareHome fills a fresh instance home: the marker, the fixtures under
// workspace/sandbox, and the sandbox profile (config.json and .security.yml).
func prepareHome(home, name string, port int, fixtures []fixtureFile) error {
	if err := os.WriteFile(filepath.Join(home, instanceMarker), []byte(name+"\n"), 0o600); err != nil {
		return fmt.Errorf("sandbox up: write marker: %w", err)
	}
	for _, f := range fixtures {
		if err := copyFile(f.path, filepath.Join(home, "workspace", "sandbox", f.venue, "fixtures.json")); err != nil {
			return fmt.Errorf("sandbox up: copy %s fixtures: %w", f.venue, err)
		}
	}
	if _, err := sandbox.WriteProfile(sandbox.ProfileOptions{Home: home, Port: port}); err != nil {
		return fmt.Errorf("sandbox up: %w", err)
	}
	return nil
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o600)
}

// ChildEnv is the whole environment of the sandbox gateway, an allowlist: the config
// loader lets any KHUNQUANT_* variable of the developer's shell override the profile.
// HOME is the instance home, so nothing the gateway runs sees the developer's.
func ChildEnv(home, name, token string) []string {
	var env []string
	for _, k := range []string{"PATH", "TMPDIR", "LANG", "TZ"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return append(env,
		"HOME="+home,
		"KHUNQUANT_HOME="+home,
		"KHUNQUANT_CONFIG="+filepath.Join(home, "config.json"),
		"KQ_SANDBOX_LOCKED=1",
		"KQ_SANDBOX_TOKEN="+token,
		"KQ_SANDBOX_INSTANCE="+name,
	)
}

// spawn starts the gateway in its own process group with ChildEnv (the token goes
// there, never into argv) and its output in <home>/gateway.log; exited closes when it ends.
func spawn(home, name, token, bin string, args []string) (*exec.Cmd, <-chan struct{}, string, error) {
	logPath := filepath.Join(home, "gateway.log")
	logFile, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, nil, "", fmt.Errorf("sandbox up: open log: %w", err)
	}
	defer logFile.Close() // the child keeps its own copy

	cmd := exec.Command(bin, args...) //nolint:gosec // bin is this executable or a test double
	cmd.Dir, cmd.Env = home, ChildEnv(home, name, token)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	setProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		return nil, nil, "", fmt.Errorf("sandbox up: start the gateway: %w", err)
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	return cmd, exited, logPath, nil
}

// waitReady polls /api/sandbox/status with the instance token until the gateway
// answers. Only the instance itself knows the token, so an answer from anything else
// (a stray listener on the port) never counts; an instance that does not report an
// enabled, locked sandbox is never handed out.
func waitReady(ctx context.Context, url, token string, exited <-chan struct{}, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("interrupted while waiting for the gateway: %w", ctx.Err())
		case <-exited:
			return errors.New("the gateway exited before it was ready")
		case <-deadline.C:
			return fmt.Errorf("the gateway was not ready after %s", timeout)
		case <-tick.C:
		}
		code, body := get(ctx, url+"/api/sandbox/status", token)
		if code != http.StatusOK {
			continue
		}
		var st struct{ Enabled, Locked bool }
		if err := json.Unmarshal(body, &st); err != nil || !st.Locked || !st.Enabled {
			return errors.New("the gateway answered but does not report an enabled, locked sandbox")
		}
		return nil
	}
}

// get returns the status code (0 on a transport error) and body of a GET.
func get(ctx context.Context, url, bearer string) (int, []byte) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, nil
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
	if err != nil {
		return 0, nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	return resp.StatusCode, body
}

// logTail is the end of the gateway log for the error of a failed start, with the
// token masked in case anything echoed it.
func logTail(path, token string) string {
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return ""
	}
	if len(data) > 4096 {
		data = data[len(data)-4096:]
	}
	return "\n--- end of gateway.log ---\n" + strings.ReplaceAll(strings.TrimRight(string(data), "\n"), token, "***")
}

// freePort asks the kernel for an unused loopback port.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("sandbox up: find a free port: %w", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// randomHex returns 2*n random hex characters (crypto/rand.Read cannot fail).
func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
