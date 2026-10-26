package sandbox

import (
	"fmt"
	"net"
	"net/url"
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

// refusedProtocols spawn an agent, call a local daemon or use OAuth, none of which
// the egress guard can see.
var refusedProtocols = map[string]bool{
	"claude-cli": true, "claudecli": true, "codex-cli": true, "codexcli": true,
	"github-copilot": true, "copilot": true, "antigravity": true,
	"gemini-code-assist": true, "google-gemini": true,
}

// CheckLockedModel says whether a model with this protocol and API base may be
// built under the lock. It returns nil when the process is not locked.
func CheckLockedModel(protocol, apiBase string) error {
	lockMu.RLock()
	defer lockMu.RUnlock()
	if !locked {
		return nil
	}
	p := strings.ToLower(strings.TrimSpace(protocol))
	if refusedProtocols[p] {
		return fmt.Errorf("sandbox: protocol %q is not allowed under the sandbox lock", protocol)
	}
	if p == "sim" {
		return nil
	}
	host := apiBaseHost(apiBase)
	if host == "" {
		return fmt.Errorf("sandbox: protocol %q needs a loopback api_base under the sandbox lock", protocol)
	}
	if hostIsLoopback(host) {
		return nil
	}
	for _, h := range lockOpts.AllowHosts {
		if strings.EqualFold(strings.TrimSpace(h), host) {
			return nil
		}
	}
	return fmt.Errorf("sandbox: api_base host %q is neither loopback nor allowed under the sandbox lock", host)
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
