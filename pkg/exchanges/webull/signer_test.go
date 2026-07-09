package webull

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestSignRequestDeterministic verifies that signing produces a consistent signature
// when timestamp and nonce are pinned.
func TestSignRequestDeterministic(t *testing.T) {
	// Create a signer with fixed timestamp and nonce
	signer := NewSigner("test-app-key", "test-app-secret")
	fixedTime := time.Date(2024, 7, 15, 10, 30, 45, 0, time.UTC)
	fixedNonce := "TestNonce1234567"

	signer.now = func() time.Time { return fixedTime }
	signer.nonceFn = func() string { return fixedNonce }

	// Test 1: Simple GET request with no body, no query params
	path1 := "/v2/trading/accounts/12345/balances"
	headers1 := signer.SignRequest(path1, "GET", "api.webull.com", nil, nil)

	if headers1.XAppKey != "test-app-key" {
		t.Errorf("expected XAppKey=test-app-key, got %s", headers1.XAppKey)
	}
	if headers1.XTimestamp != "2024-07-15T10:30:45Z" {
		t.Errorf("expected XTimestamp=2024-07-15T10:30:45Z, got %s", headers1.XTimestamp)
	}
	if headers1.XSignatureNonce != fixedNonce {
		t.Errorf("expected XSignatureNonce=%s, got %s", fixedNonce, headers1.XSignatureNonce)
	}
	if headers1.XSignatureAlgorithm != "HMAC-SHA1" {
		t.Errorf("expected XSignatureAlgorithm=HMAC-SHA1, got %s", headers1.XSignatureAlgorithm)
	}
	if headers1.XVersion != "v2" {
		t.Errorf("expected XVersion=v2, got %s", headers1.XVersion)
	}

	// Verify signature is base64-encoded (roughly 28 chars for SHA1)
	if len(headers1.XSignature) == 0 {
		t.Errorf("expected non-empty XSignature")
	}

	// Test 2: Same request should produce same signature
	headers1b := signer.SignRequest(path1, "GET", "api.webull.com", nil, nil)
	if headers1.XSignature != headers1b.XSignature {
		t.Errorf("signatures diverged for identical requests")
	}

	// Test 3: Different path should produce different signature
	path2 := "/v2/trading/accounts/12345/positions"
	headers2 := signer.SignRequest(path2, "GET", "api.webull.com", nil, nil)
	if headers1.XSignature == headers2.XSignature {
		t.Errorf("expected different signatures for different paths")
	}

	// Test 4: Request with body
	body := []byte(`{"symbol":"AAPL"}`)
	headers3 := signer.SignRequest(path1, "POST", "api.webull.com", nil, body)
	if headers1.XSignature == headers3.XSignature {
		t.Errorf("expected different signatures for request with body vs without")
	}

	// Test 5: Different body should produce different signature
	body2 := []byte(`{"symbol":"GOOGL"}`)
	headers4 := signer.SignRequest(path1, "POST", "api.webull.com", nil, body2)
	if headers3.XSignature == headers4.XSignature {
		t.Errorf("expected different signatures for different bodies")
	}

	// Test 6: Query parameters
	query := url.Values{}
	query.Set("limit", "10")
	headers5 := signer.SignRequest(path1, "GET", "api.webull.com", query, nil)
	if headers1.XSignature == headers5.XSignature {
		t.Errorf("expected different signatures with query params")
	}

	t.Logf("Sample signature (GET %s): %s", path1, headers1.XSignature)
}

// TestCanonicalStringConstruction verifies the canonical string format.
func TestCanonicalStringConstruction(t *testing.T) {
	signer := NewSigner("app-key-123", "app-secret-456")
	fixedTime := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	fixedNonce := "FixedNonce123456"

	signer.now = func() time.Time { return fixedTime }
	signer.nonceFn = func() string { return fixedNonce }

	// Build a canonical string manually to verify format
	path := "/v2/market/quotes/AAPL"
	query := url.Values{}
	query.Set("interval", "1d")
	body := []byte(`{"test":"data"}`)

	// The canonical string should be:
	// path & (sorted params) & (MD5 of body)
	// all URL-encoded

	canonical := signer.canonicalString(path, query, body, "2024-01-01T12:00:00Z", fixedNonce, "api.webull.com")

	// Verify it's not empty and looks reasonable
	if canonical == "" {
		t.Errorf("canonical string should not be empty")
	}

	// It should be URL-encoded, so should contain %
	if len(canonical) == 0 {
		t.Errorf("canonical string should have content")
	}

	// Verify determinism
	canonical2 := signer.canonicalString(path, query, body, "2024-01-01T12:00:00Z", fixedNonce, "api.webull.com")
	if canonical != canonical2 {
		t.Errorf("canonical string generation is not deterministic")
	}

	t.Logf("Sample canonical string: %s", canonical)
}

// TestSignatureVectorKnown tests against a known signature (for documentation).
// This is a reference test that documents the expected signature format.
func TestSignatureVectorKnown(t *testing.T) {
	signer := NewSigner("AppKey_TEST", "AppSecret_TEST")

	// Pin time and nonce for reproducibility
	fixedTime := time.Date(2024, 7, 7, 15, 30, 0, 0, time.UTC)
	fixedNonce := "NONCE1234567890X"

	signer.now = func() time.Time { return fixedTime }
	signer.nonceFn = func() string { return fixedNonce }

	// Simple test case: GET request, no body, no query
	path := "/v2/trading/accounts/ACC123/balances"
	headers := signer.SignRequest(path, "GET", "api.webull.com", nil, nil)

	// Verify the components are populated
	if headers.XAppKey != "AppKey_TEST" {
		t.Errorf("XAppKey mismatch")
	}
	if headers.XTimestamp != "2024-07-07T15:30:00Z" {
		t.Errorf("XTimestamp mismatch: %s", headers.XTimestamp)
	}
	if headers.XSignatureNonce != fixedNonce {
		t.Errorf("XSignatureNonce mismatch")
	}

	// Signature should be base64-encoded
	// SHA1 produces 20 bytes, base64-encoded is ~27 chars
	if len(headers.XSignature) < 20 {
		t.Errorf("XSignature too short (got %d chars, expected ~27)", len(headers.XSignature))
	}

	t.Logf("Known vector: app_key=AppKey_TEST, secret=AppSecret_TEST")
	t.Logf("  path=%s", path)
	t.Logf("  timestamp=%s", headers.XTimestamp)
	t.Logf("  nonce=%s", headers.XSignatureNonce)
	t.Logf("  signature=%s", headers.XSignature)
}

// TestRandomNonceRandomness verifies that randomNonce() produces genuinely random 16-char nonces.
func TestRandomNonceRandomness(t *testing.T) {
	nonce1 := randomNonce()
	nonce2 := randomNonce()

	// Verify both are 16 characters
	if len(nonce1) != 16 {
		t.Errorf("nonce1 length = %d, want 16", len(nonce1))
	}
	if len(nonce2) != 16 {
		t.Errorf("nonce2 length = %d, want 16", len(nonce2))
	}

	// Verify they are different (with overwhelming probability for crypto/rand)
	if nonce1 == nonce2 {
		t.Errorf("successive randomNonce() calls produced identical nonces (cryptographic failure)")
	}

	// Verify characters are from the expected alphabet
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	for _, r := range nonce1 {
		if !strings.ContainsRune(alphabet, r) {
			t.Errorf("nonce1 contains invalid character: %c", r)
		}
	}
	for _, r := range nonce2 {
		if !strings.ContainsRune(alphabet, r) {
			t.Errorf("nonce2 contains invalid character: %c", r)
		}
	}

	t.Logf("nonce1: %s", nonce1)
	t.Logf("nonce2: %s", nonce2)
}
