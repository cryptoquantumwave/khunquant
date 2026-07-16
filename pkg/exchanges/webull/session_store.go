package webull

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/credential"
	"github.com/cryptoquantumwave/khunquant/pkg/fileutil"
)

// Webull's login session requires periodic in-app 2FA approval but, once
// approved, the resulting token is valid for Webull's own ~15-day window.
// Caching that token only in process memory (as the original implementation
// did) meant the approval was lost on every process restart and was never
// shared between the khunquant gateway and the web launcher backend — two
// separate OS processes that each build their own Client. This file persists
// the approved session to disk so every process converges on the same
// session for its real lifetime, not just one process's uptime.
//
// Concurrency note: reads/writes are serialized within this process via
// sessionFileMu, but there is no cross-process file lock. Writes only happen
// on token state transitions (a handful of times per 15-day window), so a
// lost race between two processes just costs one extra login prompt — the
// same risk level config.json/.security.yml already accept without a lock.

// sessionEntry is the on-disk representation of one account's Webull login
// session. Token is encrypted at rest via the same enc:// mechanism that
// already protects api_key/secret in .security.yml (see config.SecureString)
// when a KHUNQUANT_KEY_PASSPHRASE/SSH key is configured; otherwise it is
// stored in plaintext, matching today's behavior for api_key/secret rather
// than introducing a new risk tier.
type sessionEntry struct {
	Token     config.SecureString `yaml:"token"`
	Status    string              `yaml:"status"`
	ExpiresAt int64               `yaml:"expires_at"` // unix ms, same unit Webull returns
	UpdatedAt int64               `yaml:"updated_at"` // unix ms
}

type sessionFile struct {
	Accounts map[string]sessionEntry `yaml:"accounts"`
}

var sessionFileMu sync.Mutex

// sessionFilePathFn resolves the on-disk session cache location. It's a
// package var (not a plain function) so tests can redirect it to a temp
// directory instead of touching the real $KHUNQUANT_HOME/.khunquant, the
// same override-a-package-var pattern client_test.go already uses for
// retryDelayFn.
var sessionFilePathFn = func() string {
	return filepath.Join(config.HomeDir(), ".webull-sessions.yml")
}

// readSessionFile loads the session file. A missing file is not an error —
// it just means no account has ever connected yet.
func readSessionFile() (*sessionFile, error) {
	data, err := os.ReadFile(sessionFilePathFn())
	if err != nil {
		if os.IsNotExist(err) {
			return &sessionFile{Accounts: map[string]sessionEntry{}}, nil
		}
		return nil, err
	}
	var sf sessionFile
	if err := yaml.Unmarshal(data, &sf); err != nil {
		return nil, err
	}
	if sf.Accounts == nil {
		sf.Accounts = map[string]sessionEntry{}
	}
	return &sf, nil
}

func writeSessionFile(sf *sessionFile) error {
	data, err := yaml.Marshal(sf)
	if err != nil {
		return err
	}
	return fileutil.WriteFileAtomic(sessionFilePathFn(), data, 0o600)
}

// loadSession returns the persisted session for account, if any. A missing
// file, a missing account entry, or a corrupt file are all treated as "no
// session" (ok=false) rather than a hard error — this is a best-effort
// cache, not a required source of truth; the normal login flow is always
// the fallback when no cached session is found.
func loadSession(account string) (token, status string, expiresAt time.Time, ok bool) {
	sessionFileMu.Lock()
	defer sessionFileMu.Unlock()

	sf, err := readSessionFile()
	if err != nil {
		return "", "", time.Time{}, false
	}
	entry, found := sf.Accounts[account]
	if !found || entry.Token.String() == "" {
		return "", "", time.Time{}, false
	}
	return entry.Token.String(), entry.Status, time.UnixMilli(entry.ExpiresAt), true
}

// saveSession persists the current session for account. Passing an empty
// token clears the entry (mirrors Client.invalidateToken).
//
// Two hard safety rules protect the shared file from a process that is
// missing the enc:// passphrase (e.g. a fresh install or a service started
// without ~/.khunquant/.passphrase):
//  1. If the existing file cannot be read/decrypted, saveSession fails
//     instead of rebuilding from an empty map — a rebuild would silently
//     destroy every OTHER account's stored session.
//  2. If the existing file contains enc:// entries but this process has no
//     passphrase, saveSession refuses to write — SecureString would
//     serialize the new token as plaintext, silently downgrading a file the
//     user had encrypted.
func saveSession(account, token, status string, expiresAt time.Time) error {
	sessionFileMu.Lock()
	defer sessionFileMu.Unlock()

	sf, err := readSessionFile()
	if err != nil {
		return fmt.Errorf("webull: session file %s exists but cannot be read (missing passphrase or corrupt file?) — refusing to overwrite it: %w", sessionFilePathFn(), err)
	}

	if credential.PassphraseProvider() == "" {
		if raw, readErr := os.ReadFile(sessionFilePathFn()); readErr == nil &&
			strings.Contains(string(raw), credential.EncScheme) {
			return fmt.Errorf("webull: session file %s holds encrypted entries but no passphrase is available — refusing to write a plaintext downgrade", sessionFilePathFn())
		}
	}

	if token == "" {
		delete(sf.Accounts, account)
	} else {
		sf.Accounts[account] = sessionEntry{
			Token:     *config.NewSecureString(token),
			Status:    status,
			ExpiresAt: expiresAt.UnixMilli(),
			UpdatedAt: time.Now().UnixMilli(),
		}
	}
	return writeSessionFile(sf)
}
