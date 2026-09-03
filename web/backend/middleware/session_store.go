package middleware

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"sync"
	"time"
)

// DefaultSessionTTL bounds how long a dashboard session stays valid without
// being re-issued. It is deliberately short enough that an abandoned browser
// tab stops being a standing credential.
const DefaultSessionTTL = 12 * time.Hour

// SessionStore holds live dashboard sessions.
//
// Sessions live in memory only. They are therefore invalidated wholesale by a
// launcher restart, which is a deliberate trade rather than an oversight: the
// launcher is a single local process, persistence would mean shipping a store
// (web/backend/dashboardauth exists but contains only an unsupported-platform
// stub), and "restarting the launcher logs everyone out" is defensible
// behaviour for this kind of tool.
//
// What this buys, and what the previous design could not do, is per-session
// identity: each session has its own unguessable ID, so one can be revoked
// without disturbing the others.
type SessionStore struct {
	mu       sync.RWMutex
	ttl      time.Duration
	sessions map[string]time.Time // id → expiry
	now      func() time.Time     // overridable in tests
}

// NewSessionStore returns an empty store. A ttl of zero uses DefaultSessionTTL.
func NewSessionStore(ttl time.Duration) *SessionStore {
	if ttl <= 0 {
		ttl = DefaultSessionTTL
	}
	return &SessionStore{
		ttl:      ttl,
		sessions: make(map[string]time.Time),
		now:      time.Now,
	}
}

// Create mints a new session and returns its ID.
//
// The ID is 32 bytes of crypto/rand, independent of any configured secret. That
// independence is the point: the previous scheme set the cookie value to the
// launcher secret itself, so a single leaked cookie disclosed the secret and
// every session was byte-identical and therefore individually unrevocable.
func (s *SessionStore) Create() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	id := hex.EncodeToString(raw)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	s.sessions[id] = s.now().Add(s.ttl)
	return id, nil
}

// Valid reports whether id names a live session, and is constant-time in the
// comparison so a caller cannot probe for a valid prefix by timing.
func (s *SessionStore) Valid(id string) bool {
	if id == "" {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	now := s.now()
	for known, expiry := range s.sessions {
		if subtle.ConstantTimeCompare([]byte(known), []byte(id)) == 1 {
			return now.Before(expiry)
		}
	}
	return false
}

// Destroy invalidates one session. This is what logout does, and it is the
// capability the old shared-secret cookie could not express.
func (s *SessionStore) Destroy(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
}

// DestroyAll invalidates every session, for use when the password changes: a
// credential rotation that left existing sessions alive would not actually
// revoke access.
func (s *SessionStore) DestroyAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions = make(map[string]time.Time)
}

// Count reports live (unexpired) sessions. Used by tests.
func (s *SessionStore) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	return len(s.sessions)
}

func (s *SessionStore) pruneLocked() {
	now := s.now()
	for id, expiry := range s.sessions {
		if !now.Before(expiry) {
			delete(s.sessions, id)
		}
	}
}
