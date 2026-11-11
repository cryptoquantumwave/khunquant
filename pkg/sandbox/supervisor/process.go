// Package supervisor runs locked sandbox instances of the gateway: the prebuilt binary
// in its own HOME, process group, config and token. `khunquant sandbox up/down` and the
// sandbox MCP server both start and stop instances through it.
package supervisor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labclient"
)

const (
	// instanceMarker is the file lockOptionsFromEnv requires in KHUNQUANT_HOME.
	instanceMarker = ".kq-sandbox"
	homePrefix     = "kq-sbx-" // instance home: <base>/kq-sbx-<name>

	defaultReadyTimeout = 60 * time.Second
	logName             = "gateway.log"
)

// ErrUnixOnly is returned on platforms without process groups and POSIX signals.
var ErrUnixOnly = errors.New("khunquant sandbox up/down need a unix system (process groups and signals)")

// nameRE keeps an instance name a single plain path element.
var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}$`)

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

// markerContent is the marker file: the instance name, then the pid of the supervisor
// that owns the home, so another supervisor can tell a live owner from a dead one.
func markerContent(name string) []byte {
	return []byte(fmt.Sprintf("%s\nsupervisor=%d\n", name, os.Getpid()))
}

// markerSupervisor is the supervisor pid a marker names, or 0 for an older marker
// (the name only) or an unreadable one.
func markerSupervisor(home string) int {
	data, err := os.ReadFile(filepath.Join(home, instanceMarker))
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "supervisor="); ok {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				return n
			}
		}
	}
	return 0
}

// prepareHome fills a fresh instance home: the marker, the fixtures under
// workspace/sandbox, and the sandbox profile (config.json and .security.yml).
func prepareHome(home, name string, port int, fixtures []fixtureFile, accounts ...string) error {
	if err := os.WriteFile(filepath.Join(home, instanceMarker), markerContent(name), 0o600); err != nil {
		return fmt.Errorf("sandbox up: write marker: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(home, sandbox.TempDirName), 0o700); err != nil {
		return fmt.Errorf("sandbox up: temporary directory: %w", err)
	}
	for _, f := range fixtures {
		if err := copyFile(f.path, filepath.Join(home, "workspace", "sandbox", f.venue, "fixtures.json")); err != nil {
			return fmt.Errorf("sandbox up: copy %s fixtures: %w", f.venue, err)
		}
	}
	if _, err := sandbox.WriteProfile(sandbox.ProfileOptions{Home: home, Port: port}); err != nil {
		return fmt.Errorf("sandbox up: %w", err)
	}
	if err := writeAccounts(home, accounts); err != nil {
		return fmt.Errorf("sandbox up: record accounts: %w", err)
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
// HOME is the instance home and TMPDIR a directory inside it, so nothing the gateway runs
// sees the developer's.
func ChildEnv(home, name, token string) []string { return childEnv(home, name, token, nil) }

// childEnv is ChildEnv plus the AllowEnv-ed extras. The fixed entries come last: with a
// duplicate key the last one wins, and validEnv has refused the fixed keys anyway.
func childEnv(home, name, token string, extra map[string]string) []string {
	var env []string
	for _, k := range []string{"PATH", "LANG", "TZ"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(extra)) {
		env = append(env, k+"="+extra[k])
	}
	return append(env,
		"HOME="+home,
		// Never the developer's: the media store is under the temporary directory, and the
		// file tools may always read it (sandbox.TempDirName).
		"TMPDIR="+filepath.Join(home, sandbox.TempDirName),
		"KHUNQUANT_HOME="+home,
		"KHUNQUANT_CONFIG="+filepath.Join(home, "config.json"),
		"KQ_SANDBOX_LOCKED=1",
		"KQ_SANDBOX_TOKEN="+token,
		"KQ_SANDBOX_INSTANCE="+name,
	)
}

// proc is one running gateway process. exited closes when it has ended and code is
// then valid (128+signal for a killed process).
type proc struct {
	cmd    *exec.Cmd
	exited chan struct{}
	code   int
}

// spawn starts the gateway in its own process group with the environment from childEnv
// (the token goes there, never into argv) and its output appended to <home>/gateway.log.
func spawn(bin string, args, env []string, home string) (*proc, error) {
	logFile, err := os.OpenFile(filepath.Join(home, logName), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("sandbox up: open log: %w", err)
	}
	defer logFile.Close() // the child keeps its own copy

	cmd := exec.Command(bin, args...) //nolint:gosec // bin is this executable or a test double
	cmd.Dir, cmd.Env = home, env
	cmd.Stdout, cmd.Stderr = logFile, logFile
	setProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("sandbox up: start the gateway: %w", err)
	}
	p := &proc{cmd: cmd, exited: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		p.code = exitCode(cmd.ProcessState)
		close(p.exited)
	}()
	return p, nil
}

// errGatewayExited is what waitReady returns when the process ended first.
var errGatewayExited = errors.New("the gateway exited before it was ready")

// waitReady polls /api/sandbox/status with the instance token until the gateway reports
// itself ready (fully started: first generation of services published, signals handled;
// /health is up earlier). Only the instance itself knows the token, so an answer from
// anything else (a stray listener on the port) never counts; an instance that does not
// report an enabled, locked sandbox is never handed out, and one that does not yet say
// ready, or does not carry the field, is polled again.
func waitReady(ctx context.Context, c *labclient.Client, exited <-chan struct{}, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("interrupted while waiting for the gateway: %w", ctx.Err())
		case <-exited:
			return errGatewayExited
		case <-deadline.C:
			return fmt.Errorf("the gateway was not ready after %s", timeout)
		case <-tick.C:
		}
		pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		locked, ready, err := c.Status(pctx)
		cancel()
		if err != nil {
			continue // not listening yet, or not an instance that knows the token
		}
		if !locked {
			return ErrNotLocked
		}
		if ready {
			return nil
		}
	}
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
