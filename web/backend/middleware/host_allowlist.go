package middleware

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
)

// maxLoggedRejectedHosts bounds the memory a probe can make HostAllowlist use
// by sending a stream of distinct Host values.
const maxLoggedRejectedHosts = 256

// maxShownHostLen caps how much of an attacker-supplied Host is echoed back or
// logged.
const maxShownHostLen = 253

// hostSet is the set of Host names the launcher answers to, beyond the
// built-in loopback names and IP literals.
type hostSet struct {
	extra map[string]struct{}
}

// NormalizeHost reduces a Host header (or an allowed_hosts entry) to the form
// the allowlist compares: no port, lower case, no trailing dot, IPv6 brackets
// removed. It returns "" for an empty host.
func NormalizeHost(raw string) string {
	h := strings.TrimSpace(raw)
	if h == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	} else if strings.HasPrefix(h, "[") && strings.HasSuffix(h, "]") {
		h = h[1 : len(h)-1]
	}
	h = strings.TrimRight(h, ".")
	return strings.ToLower(h)
}

func newHostSet(extra []string) *hostSet {
	s := &hostSet{extra: make(map[string]struct{}, len(extra))}
	for _, e := range extra {
		if h := NormalizeHost(e); h != "" {
			s.extra[h] = struct{}{}
		}
	}
	return s
}

// allows reports whether the Host header value names this launcher.
//
// A DNS-rebinding page is served from an attacker-controlled *name* that the
// attacker later points at 127.0.0.1, so the browser sends that name as Host
// and as Origin, and the two agree. Names are therefore refused unless they are
// provably local (localhost, *.localhost) or the operator listed them. An IP
// literal is always allowed: the page's origin is then that IP itself, so there
// is no name for an attacker to rebind.
func (s *hostSet) allows(hostHeader string) bool {
	h := NormalizeHost(hostHeader)
	if h == "" {
		return false
	}
	if h == "localhost" || (strings.HasSuffix(h, ".localhost") && len(h) > len(".localhost")) {
		return true
	}
	if net.ParseIP(h) != nil {
		return true
	}
	_, ok := s.extra[h]
	return ok
}

// activeHosts backs isTrustedLoopbackCaller, which has no handle on the
// middleware's configuration. It is set once at startup (SetAllowedHosts);
// until then only the built-in names are allowed.
var activeHosts atomic.Pointer[hostSet]

// SetAllowedHosts records the operator's allowed_hosts for the defence-in-depth
// check in SessionAuth. Pass the same list given to HostAllowlist.
func SetAllowedHosts(extra []string) { activeHosts.Store(newHostSet(extra)) }

// hostAllowed is the predicate isTrustedLoopbackCaller applies; HostAllowlist
// uses the same hostSet.allows rule.
func hostAllowed(hostHeader string) bool {
	s := activeHosts.Load()
	if s == nil {
		s = newHostSet(nil)
	}
	return s.allows(hostHeader)
}

// HostAllowlist answers 421 Misdirected Request to any request whose Host is
// not localhost, *.localhost, an IP literal, or listed in extraHosts. It closes
// DNS rebinding: without it a rebound page's Host and Origin agree, and the
// loopback auto-session in SessionAuth would hand it a session.
//
// Each rejected host is logged once (bounded), so a probe cannot flood the log.
func HostAllowlist(extraHosts []string, next http.Handler) http.Handler {
	set := newHostSet(extraHosts)
	var mu sync.Mutex
	logged := make(map[string]struct{})

	warnOnce := func(host string) {
		mu.Lock()
		defer mu.Unlock()
		if _, ok := logged[host]; ok {
			return
		}
		switch {
		case len(logged) < maxLoggedRejectedHosts:
			logged[host] = struct{}{}
			log.Printf("WARN: refused request for Host %s (421); to serve this name add it to "+
				"allowed_hosts in launcher-config.json", host)
		case len(logged) == maxLoggedRejectedHosts:
			logged["\x00overflow"] = struct{}{}
			log.Printf("WARN: refused requests for many distinct Host names; not logging further ones")
		}
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if set.allows(r.Host) {
			next.ServeHTTP(w, r)
			return
		}
		host := r.Host
		if len(host) > maxShownHostLen {
			host = host[:maxShownHostLen]
		}
		shown := fmt.Sprintf("%q", host)
		warnOnce(shown)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusMisdirectedRequest)
		_, _ = fmt.Fprintf(w, "421 Misdirected Request: the Host %s is not allowed for this launcher.\n"+
			"If you reach it through a domain name, add that name to \"allowed_hosts\" in "+
			"launcher-config.json and restart.\n", shown)
	})
}
