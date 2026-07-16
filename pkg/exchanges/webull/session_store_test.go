package webull

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/credential"
)

// withTestSessionFile redirects the session cache to a fresh temp file for
// the duration of t, so tests never touch the developer's real
// $KHUNQUANT_HOME/.webull-sessions.yml.
func withTestSessionFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "webull-sessions.yml")
	old := sessionFilePathFn
	sessionFilePathFn = func() string { return path }
	t.Cleanup(func() { sessionFilePathFn = old })
	return path
}

// withTestSSHKey generates a throwaway SSH key and points
// KHUNQUANT_SSH_KEY_PATH at it, so enc:// encryption/decryption is
// self-contained and works in CI (which has no ~/.ssh/khunquant_ed25519.key).
// Mirrors the setup in pkg/credential/credential_test.go.
func withTestSSHKey(t *testing.T) {
	t.Helper()
	keyPath := filepath.Join(t.TempDir(), "test_key.pem")
	if err := credential.GenerateSSHKey(keyPath); err != nil {
		t.Fatalf("GenerateSSHKey: %v", err)
	}
	t.Setenv(credential.SSHKeyPathEnvVar, keyPath)
}

func TestSessionStore_SaveLoadRoundTrip(t *testing.T) {
	withTestSessionFile(t)

	expiresAt := time.UnixMilli(1234567890123)
	if err := saveSession("main", "tok-abc123", TokenStatusNormal, expiresAt); err != nil {
		t.Fatalf("saveSession failed: %v", err)
	}

	token, status, gotExpiresAt, ok := loadSession("main")
	if !ok {
		t.Fatal("expected loadSession to find the saved entry")
	}
	if token != "tok-abc123" {
		t.Errorf("token = %q, want %q", token, "tok-abc123")
	}
	if status != TokenStatusNormal {
		t.Errorf("status = %q, want %q", status, TokenStatusNormal)
	}
	if !gotExpiresAt.Equal(expiresAt) {
		t.Errorf("expiresAt = %v, want %v", gotExpiresAt, expiresAt)
	}
}

func TestSessionStore_EncryptedRoundTrip(t *testing.T) {
	path := withTestSessionFile(t)
	withTestSSHKey(t)

	old := credential.PassphraseProvider
	credential.PassphraseProvider = func() string { return "test-passphrase-1234" }
	t.Cleanup(func() { credential.PassphraseProvider = old })

	expiresAt := time.UnixMilli(9999999999999)
	if err := saveSession("main", "secret-token-value", TokenStatusNormal, expiresAt); err != nil {
		t.Fatalf("saveSession failed: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read session file: %v", err)
	}
	if got := string(raw); !strings.Contains(got, "enc://") {
		t.Errorf("expected the on-disk token to be enc:// encrypted, got: %s", got)
	}
	if strings.Contains(string(raw), "secret-token-value") {
		t.Fatal("plaintext token leaked into the on-disk session file")
	}

	token, _, _, ok := loadSession("main")
	if !ok {
		t.Fatal("expected loadSession to find the saved entry")
	}
	if token != "secret-token-value" {
		t.Errorf("decrypted token = %q, want %q", token, "secret-token-value")
	}
}

func TestSessionStore_MissingFileIsNotOk(t *testing.T) {
	withTestSessionFile(t)

	if _, _, _, ok := loadSession("main"); ok {
		t.Fatal("expected loadSession to report no session for a missing file")
	}
}

func TestSessionStore_MissingAccountIsNotOk(t *testing.T) {
	withTestSessionFile(t)

	if err := saveSession("other-account", "tok", TokenStatusNormal, time.Now()); err != nil {
		t.Fatalf("saveSession failed: %v", err)
	}
	if _, _, _, ok := loadSession("main"); ok {
		t.Fatal("expected loadSession to report no session for an unrelated account")
	}
}

func TestSessionStore_CorruptFileIsNotOk(t *testing.T) {
	path := withTestSessionFile(t)

	if err := os.WriteFile(path, []byte("not: [valid yaml"), 0o600); err != nil {
		t.Fatalf("failed to write corrupt session file: %v", err)
	}

	if _, _, _, ok := loadSession("main"); ok {
		t.Fatal("expected loadSession to treat a corrupt file as no session")
	}
}

// TestSessionStore_SaveRefusesToClobberUnreadableFile covers the
// passphrase-less-process safety rule: when the existing file cannot be
// read (encrypted entries + no passphrase, or corrupt YAML), saveSession
// must fail instead of rebuilding from an empty map, which would destroy
// every other account's stored session.
func TestSessionStore_SaveRefusesToClobberUnreadableFile(t *testing.T) {
	path := withTestSessionFile(t)
	withTestSSHKey(t)

	// Write an encrypted entry with a passphrase installed...
	old := credential.PassphraseProvider
	credential.PassphraseProvider = func() string { return "test-passphrase-1234" }
	if err := saveSession("other-account", "precious-token", TokenStatusNormal, time.Now().Add(time.Hour)); err != nil {
		credential.PassphraseProvider = old
		t.Fatalf("saveSession failed: %v", err)
	}

	// ...then simulate a process with NO passphrase trying to save.
	credential.PassphraseProvider = func() string { return "" }
	t.Cleanup(func() { credential.PassphraseProvider = old })

	err := saveSession("main", "new-token", TokenStatusPending, time.Now().Add(time.Hour))
	if err == nil {
		t.Fatal("expected saveSession to refuse writing when the existing file is unreadable without a passphrase")
	}

	// The original encrypted entry must be untouched on disk.
	raw, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("failed to read session file: %v", readErr)
	}
	if !strings.Contains(string(raw), "other-account") {
		t.Fatal("existing account entry was clobbered from the session file")
	}
}

// TestSessionStore_SaveRefusesPlaintextDowngradeOfCorruptFile: a corrupt
// (non-YAML) existing file must also block writes rather than be replaced.
func TestSessionStore_SaveRefusesToClobberCorruptFile(t *testing.T) {
	path := withTestSessionFile(t)

	if err := os.WriteFile(path, []byte("not: [valid yaml"), 0o600); err != nil {
		t.Fatalf("failed to write corrupt session file: %v", err)
	}

	if err := saveSession("main", "tok", TokenStatusNormal, time.Now()); err == nil {
		t.Fatal("expected saveSession to refuse writing over an unreadable existing file")
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read session file: %v", err)
	}
	if string(raw) != "not: [valid yaml" {
		t.Fatal("corrupt file was overwritten instead of preserved for inspection")
	}
}

func TestSessionStore_EmptyTokenClearsEntry(t *testing.T) {
	withTestSessionFile(t)

	if err := saveSession("main", "tok-abc123", TokenStatusNormal, time.Now()); err != nil {
		t.Fatalf("saveSession failed: %v", err)
	}
	if _, _, _, ok := loadSession("main"); !ok {
		t.Fatal("expected the entry to exist before clearing")
	}

	if err := saveSession("main", "", "", time.Time{}); err != nil {
		t.Fatalf("saveSession (clear) failed: %v", err)
	}
	if _, _, _, ok := loadSession("main"); ok {
		t.Fatal("expected loadSession to report no session after clearing with an empty token")
	}
}
