package middleware

import (
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// M-9: the dashboard password is chosen by a person, and nothing limited how
// fast it could be guessed - through the login form or through Basic auth on
// any request. bcrypt slows each guess, but concurrent requests parallelise.
//
// AuthLimiter locks an address out after repeated failures. It keys on the TCP
// peer (RemoteAddr), never on X-Forwarded-For: a header the client writes would
// let every guess arrive from a "new" address. Behind a reverse proxy every
// client shares the proxy's address, so a lockout there applies to all of
// them - the conservative failure for a password check.
type AuthLimiter struct {
	mu          sync.Mutex
	maxFailures int
	window      time.Duration
	now         func() time.Time
	entries     map[string]*authFailures
}

type authFailures struct {
	count int
	first time.Time
}

const (
	defaultAuthMaxFailures = 5
	defaultAuthWindow      = 15 * time.Minute
	// maxTrackedAuthAddrs bounds memory under a spray from many addresses.
	maxTrackedAuthAddrs = 10000
)

// NewAuthLimiter allows maxFailures failed attempts per address per window.
func NewAuthLimiter(maxFailures int, window time.Duration) *AuthLimiter {
	return &AuthLimiter{
		maxFailures: maxFailures,
		window:      window,
		now:         time.Now,
		entries:     make(map[string]*authFailures),
	}
}

// DefaultAuthLimiter is shared by the login handler and PasswordAuth, so a
// guess through either counts against the same budget.
var DefaultAuthLimiter = NewAuthLimiter(defaultAuthMaxFailures, defaultAuthWindow)

func peerAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// Allow reports whether the caller may attempt authentication now, and if not,
// how long until it may.
func (l *AuthLimiter) Allow(r *http.Request) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[peerAddr(r)]
	if !ok {
		return true, 0
	}
	elapsed := l.now().Sub(e.first)
	if elapsed >= l.window {
		delete(l.entries, peerAddr(r))
		return true, 0
	}
	if e.count >= l.maxFailures {
		return false, l.window - elapsed
	}
	return true, 0
}

// Fail records a failed attempt.
func (l *AuthLimiter) Fail(r *http.Request) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	addr := peerAddr(r)
	e, ok := l.entries[addr]
	if !ok || now.Sub(e.first) >= l.window {
		if !ok && len(l.entries) >= maxTrackedAuthAddrs {
			l.pruneLocked(now)
		}
		l.entries[addr] = &authFailures{count: 1, first: now}
		return
	}
	e.count++
}

// Succeed clears the caller's failures.
func (l *AuthLimiter) Succeed(r *http.Request) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, peerAddr(r))
}

func (l *AuthLimiter) pruneLocked(now time.Time) {
	for addr, e := range l.entries {
		if now.Sub(e.first) >= l.window {
			delete(l.entries, addr)
		}
	}
	// Still full of live entries: start over rather than grow without bound.
	// Losing counts is the lesser failure next to unbounded memory.
	if len(l.entries) >= maxTrackedAuthAddrs {
		l.entries = make(map[string]*authFailures)
	}
}

// RejectTooManyAttempts writes the lockout response.
func RejectTooManyAttempts(w http.ResponseWriter, retryAfter time.Duration) {
	secs := int(retryAfter.Seconds()) + 1
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = w.Write([]byte(`{"error":"too many failed attempts; try again later"}`))
}
