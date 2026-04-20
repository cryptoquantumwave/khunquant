package pairing

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/fileutil"
	"github.com/cryptoquantumwave/khunquant/pkg/identity"
)

const (
	// codeAlphabet excludes ambiguous characters: 0/O, 1/I
	codeAlphabet   = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	codeLength     = 8
	defaultTTL     = 2 * time.Hour
	maxPendingUser = 3
)

// Request is a pending pairing request from an unknown Telegram user.
type Request struct {
	Code        string `json:"code"`
	Platform    string `json:"platform"`
	PlatformID  string `json:"platform_id"`
	Username    string `json:"username,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
	CanonicalID string `json:"canonical_id"`
	ChatID      int64  `json:"chat_id"`
	CreatedAtMS int64  `json:"created_at_ms"`
	ExpiresAtMS int64  `json:"expires_at_ms"`
}

// IsExpired reports whether the request has passed its TTL.
func (r *Request) IsExpired() bool {
	return time.Now().UnixMilli() > r.ExpiresAtMS
}

type store struct {
	Requests []Request `json:"requests"`
}

// Store manages pairing requests on disk.
type Store struct {
	path string
	mu   sync.Mutex
}

// NewStore creates a Store backed by the given file path.
// The parent directory is created if it does not exist.
func NewStore(storePath string) *Store {
	return &Store{path: storePath}
}

// load reads requests from disk, returns empty store on missing file.
func (s *Store) load() (*store, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return &store{}, nil
		}
		return nil, err
	}
	var st store
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// save atomically writes the store to disk.
func (s *Store) save(st *store) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return fileutil.WriteFileAtomic(s.path, data, 0o600)
}

// ListPending returns all non-expired pending requests.
func (s *Store) ListPending() ([]Request, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	st, err := s.load()
	if err != nil {
		return nil, err
	}

	var active []Request
	var changed bool
	for _, r := range st.Requests {
		if !r.IsExpired() {
			active = append(active, r)
		} else {
			changed = true
		}
	}
	if changed {
		st.Requests = active
		_ = s.save(st)
	}
	return active, nil
}

// Upsert ensures a pending request exists for the given user.
// Returns the request and whether it was newly created.
// If the user already has maxPendingUser unexpired requests the oldest is replaced.
func (s *Store) Upsert(platform, platformID, username, displayName string, chatID int64) (*Request, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	st, err := s.load()
	if err != nil {
		return nil, false, err
	}

	canonicalID := identity.BuildCanonicalID(platform, platformID)
	now := time.Now()

	// Prune expired entries and find existing requests for this user.
	var surviving []Request
	var userRequests []*Request
	for i := range st.Requests {
		r := &st.Requests[i]
		if r.IsExpired() {
			continue
		}
		surviving = append(surviving, *r)
		if r.CanonicalID == canonicalID {
			rCopy := surviving[len(surviving)-1]
			userRequests = append(userRequests, &rCopy)
		}
	}
	st.Requests = surviving

	// Return existing request if one exists.
	if len(userRequests) > 0 {
		// Return the most recent one.
		latest := userRequests[len(userRequests)-1]
		return latest, false, nil
	}

	// If the user has too many pending, remove the oldest.
	if len(userRequests) >= maxPendingUser {
		// Remove oldest from surviving list.
		for i, r := range st.Requests {
			if r.CanonicalID == canonicalID {
				st.Requests = append(st.Requests[:i], st.Requests[i+1:]...)
				break
			}
		}
	}

	// Generate a new code.
	code, err := generateCode()
	if err != nil {
		return nil, false, fmt.Errorf("generate pairing code: %w", err)
	}

	req := Request{
		Code:        code,
		Platform:    platform,
		PlatformID:  platformID,
		Username:    username,
		DisplayName: displayName,
		CanonicalID: canonicalID,
		ChatID:      chatID,
		CreatedAtMS: now.UnixMilli(),
		ExpiresAtMS: now.Add(defaultTTL).UnixMilli(),
	}
	st.Requests = append(st.Requests, req)

	if err := s.save(st); err != nil {
		return nil, false, err
	}
	return &req, true, nil
}

// Approve removes the request with the given code and returns it.
// Returns an error if the code is not found or has expired.
func (s *Store) Approve(code string) (*Request, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	st, err := s.load()
	if err != nil {
		return nil, err
	}

	var found *Request
	var remaining []Request
	for _, r := range st.Requests {
		if r.Code == code {
			if r.IsExpired() {
				return nil, fmt.Errorf("pairing code %q has expired", code)
			}
			rCopy := r
			found = &rCopy
		} else {
			remaining = append(remaining, r)
		}
	}
	if found == nil {
		return nil, fmt.Errorf("pairing code %q not found", code)
	}

	st.Requests = remaining
	if err := s.save(st); err != nil {
		return nil, err
	}
	return found, nil
}

// Reject removes the request with the given code without approving it.
func (s *Store) Reject(code string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	st, err := s.load()
	if err != nil {
		return err
	}

	var found bool
	var remaining []Request
	for _, r := range st.Requests {
		if r.Code == code {
			found = true
		} else {
			remaining = append(remaining, r)
		}
	}
	if !found {
		return fmt.Errorf("pairing code %q not found", code)
	}

	st.Requests = remaining
	return s.save(st)
}

// generateCode returns a random 8-character code from codeAlphabet.
func generateCode() (string, error) {
	n := big.NewInt(int64(len(codeAlphabet)))
	result := make([]byte, codeLength)
	for i := range result {
		idx, err := rand.Int(rand.Reader, n)
		if err != nil {
			return "", err
		}
		result[i] = codeAlphabet[idx.Int64()]
	}
	return string(result), nil
}
