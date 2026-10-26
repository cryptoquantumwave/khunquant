package sandbox

import (
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// LockOptions configures the sandbox process lock.
type LockOptions struct {
	InstanceName string
	// Token is the instance token (env KQ_SANDBOX_TOKEN). It lives in memory only
	// and is never written to config.
	Token string
	// AllowHosts are egress exceptions (LLM provider hosts in live mode only).
	AllowHosts []string
}

// The lock is process state, decided before the config is read. Once set it cannot
// be cleared outside a test binary. lockMu is always taken before globalStateMu,
// never the other way round.
var (
	lockMu   sync.RWMutex
	lockOpts LockOptions
	locked   bool
)

// Lock turns the process into a sandbox: exchange traffic can only reach the
// sandbox server, and SetGlobalState can no longer switch the sandbox off. It arms
// the global state as (true, "") so traffic fails closed until a server starts.
// It may be called once per process and needs a token.
func Lock(opts LockOptions) error {
	if strings.TrimSpace(opts.Token) == "" {
		return fmt.Errorf("sandbox: lock needs an instance token")
	}
	lockMu.Lock()
	defer lockMu.Unlock()
	if locked {
		return fmt.Errorf("sandbox: process is already locked")
	}
	opts.AllowHosts = append([]string(nil), opts.AllowHosts...)
	lockOpts, locked = opts, true

	globalStateMu.Lock()
	globalEnabled, globalBaseURL = true, ""
	globalStateMu.Unlock()
	return nil
}

// Locked reports whether the process is locked.
func Locked() bool {
	lockMu.RLock()
	defer lockMu.RUnlock()
	return locked
}

// LockInfo returns a copy of the lock options and whether the process is locked.
func LockInfo() (LockOptions, bool) {
	lockMu.RLock()
	defer lockMu.RUnlock()
	o := lockOpts
	o.AllowHosts = append([]string(nil), o.AllowHosts...)
	return o, locked
}

// ChannelAllowed reports whether a chat channel may be built: always when unlocked,
// only the simulated "simchat" channel when locked.
func ChannelAllowed(name string) bool {
	if !Locked() {
		return true
	}
	return name == "simchat"
}

// CheckLockedModel says whether a model with this protocol may be built under the
// lock: only the simulated "sim" protocol. Every real provider is refused, however
// it is reached (an HTTP API, an agent CLI, a local daemon or OAuth), because the
// egress guard cannot see all of them. It returns nil when the process is not locked.
// This is the one place the rule lives; the model factory and the config overlay
// both ask it.
func CheckLockedModel(protocol string) error {
	lockMu.RLock()
	defer lockMu.RUnlock()
	if !locked || protocol == "sim" {
		return nil
	}
	return fmt.Errorf("sandbox: protocol %q is not allowed under the sandbox lock: only sim/* models are built", protocol)
}

// ResetLockForTest clears the lock and the global state. Test binaries only.
func ResetLockForTest() {
	if !testing.Testing() {
		panic("sandbox: ResetLockForTest is a test seam and may only run in a test binary")
	}
	lockMu.Lock()
	defer lockMu.Unlock()
	lockOpts, locked = LockOptions{}, false
	globalStateMu.Lock()
	globalEnabled, globalBaseURL = false, ""
	globalStateMu.Unlock()
}

// PathInside reports whether p is root itself or lies under it. Both sides are made
// absolute and have symlinks resolved as far as they exist (so /var vs /private/var
// and a symlink inside root that points out of it are both judged by where they
// really lead). An empty root or p is never inside.
func PathInside(root, p string) bool {
	if strings.TrimSpace(root) == "" || strings.TrimSpace(p) == "" {
		return false
	}
	r, p2 := resolveExisting(root), resolveExisting(p)
	rel, err := filepath.Rel(r, p2)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// resolveExisting makes p absolute and resolves symlinks in its longest existing
// ancestor, keeping the not-yet-existing tail as written.
func resolveExisting(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	cur, tail := abs, ""
	for {
		if res, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(res, tail)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs
		}
		tail = filepath.Join(filepath.Base(cur), tail)
		cur = parent
	}
}

// apiBaseHost returns the lower-cased host name of an API base URL ("" if none).
func apiBaseHost(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// hostIsLoopback accepts "localhost" and loopback IP literals (bare or bracketed).
func hostIsLoopback(h string) bool {
	h = strings.Trim(strings.ToLower(h), "[]")
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}
