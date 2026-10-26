// Package egress is the loopback-only network guard of a locked sandbox process.
// Once installed, clients on http.DefaultTransport or utils.CreateHTTPClient can only
// dial loopback and the policy's hosts; anything else fails with ErrBlocked before any
// DNS lookup. It does not cover clients with their own transport and dialer (ccxt's
// newDualStackTransport) or child processes; other means keep those off the network.
package egress

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// ErrBlocked is returned for a connection the policy does not allow.
var ErrBlocked = errors.New("egress: blocked by sandbox lock")

// Policy says what a locked process may reach besides loopback.
type Policy struct {
	AllowHosts []string // host names or IP literals, no port
}

var (
	mu        sync.RWMutex
	installed bool
	current   Policy
	orig      http.RoundTripper
)

// Install makes the policy process-wide by replacing http.DefaultTransport. It may
// be called once per process.
func Install(p Policy) error {
	mu.Lock()
	defer mu.Unlock()
	if installed {
		return errors.New("egress: guard is already installed")
	}
	orig = http.DefaultTransport
	http.DefaultTransport = Transport(p)
	current, installed = Policy{AllowHosts: append([]string(nil), p.AllowHosts...)}, true
	return nil
}

// Installed reports whether the guard is installed.
func Installed() bool {
	mu.RLock()
	defer mu.RUnlock()
	return installed
}

// Current returns the installed policy (zero when not installed).
func Current() Policy {
	mu.RLock()
	defer mu.RUnlock()
	return Policy{AllowHosts: append([]string(nil), current.AllowHosts...)}
}

// ResetForTest uninstalls the guard and restores http.DefaultTransport. Test
// binaries only.
func ResetForTest() {
	if !testing.Testing() {
		panic("egress: ResetForTest is a test seam and may only run in a test binary")
	}
	mu.Lock()
	defer mu.Unlock()
	if installed {
		http.DefaultTransport = orig
	}
	installed, current, orig = false, Policy{}, nil
}

// Transport returns a clone of the default transport with the guarded dialer and no
// proxy (a proxy would dial out on the client's behalf).
func Transport(p Policy) *http.Transport {
	t := &http.Transport{}
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		t = base.Clone()
	}
	t.Proxy, t.DialContext = nil, DialContext(p)
	t.Dial, t.DialTLS, t.DialTLSContext = nil, nil, nil //nolint:staticcheck // deprecated hooks must not bypass the guard
	return t
}

type dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// DialContext returns a dialer that connects only to loopback addresses and the
// policy's hosts. A blocked host is refused without a DNS lookup. "localhost" is
// resolved here and only its loopback addresses are used.
func DialContext(p Policy) func(ctx context.Context, network, addr string) (net.Conn, error) {
	var d net.Dialer
	return guard(p, d.DialContext)
}

// guard wraps dial with the policy; split out so a test can see what the policy lets
// through without touching the network.
func guard(p Policy, dial dialFunc) dialFunc {
	allow := map[string]bool{}
	for _, h := range p.AllowHosts {
		if h = normalizeHost(h); h != "" {
			allow[h] = true
		}
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		switch network {
		case "tcp", "tcp4", "tcp6":
		default:
			return nil, fmt.Errorf("%w: network %q", ErrBlocked, network)
		}
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("%w: bad address %q", ErrBlocked, addr)
		}
		host = normalizeHost(host)

		ip := net.ParseIP(host)
		if allow[host] || ip != nil && ip.IsLoopback() {
			return dial(ctx, network, net.JoinHostPort(host, port))
		}
		if host != "localhost" {
			return nil, fmt.Errorf("%w: %s", ErrBlocked, host)
		}
		addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		lastErr := fmt.Errorf("%w: %s resolves to no loopback address", ErrBlocked, host)
		for _, a := range addrs {
			if !a.IP.IsLoopback() {
				continue
			}
			conn, err := dial(ctx, network, net.JoinHostPort(a.IP.String(), port))
			if err == nil {
				return conn, nil
			}
			lastErr = err
		}
		return nil, lastErr
	}
}

// normalizeHost lower-cases a host and drops a trailing dot and IPv6 brackets.
func normalizeHost(h string) string {
	return strings.Trim(strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), "."), "[]")
}
