package webull

import (
	"encoding/hex"
	"net/url"
	"testing"
	"time"
)

// TestSignerKnownAnswerTest_CaseA verifies the signer against a pinned known answer.
// Case A: path="/openapi/assets/balance", query {account_id=ACC123}, no body
// Expected signature = "WCW9WOmMywXskxQETpxb2HxiaX8="
// With: appKey="testappkey000000000000000000abcd", appSecret="testappsecret00000000000000000000"
//
//	ts="2026-01-02T03:04:05Z", nonce="0123456789abcdef0123456789abcdef"
func TestSignerKnownAnswerTest_CaseA(t *testing.T) {
	signer := NewSigner("testappkey000000000000000000abcd", "testappsecret00000000000000000000")

	// Pin time and nonce
	fixedTime := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	fixedNonce := "0123456789abcdef0123456789abcdef"

	signer.now = func() time.Time { return fixedTime }
	signer.nonceFn = func() (string, error) { return fixedNonce, nil }

	// Case A: GET /openapi/assets/balance with query param account_id=ACC123
	path := "/openapi/assets/balance"
	query := url.Values{}
	query.Set("account_id", "ACC123")

	headers, err := signer.SignRequest(path, "GET", "api.webull.com", query, nil)
	if err != nil {
		t.Fatalf("SignRequest failed: %v", err)
	}

	expectedSig := "WCW9WOmMywXskxQETpxb2HxiaX8="
	if headers.XSignature != expectedSig {
		t.Errorf("Case A signature mismatch:\n  expected: %s\n  got:      %s", expectedSig, headers.XSignature)
	}

	// Verify other headers are set correctly
	if headers.XAppKey != "testappkey000000000000000000abcd" {
		t.Errorf("XAppKey mismatch: expected testappkey000000000000000000abcd, got %s", headers.XAppKey)
	}
	if headers.XTimestamp != "2026-01-02T03:04:05Z" {
		t.Errorf("XTimestamp mismatch: expected 2026-01-02T03:04:05Z, got %s", headers.XTimestamp)
	}
	if headers.XSignatureNonce != fixedNonce {
		t.Errorf("XSignatureNonce mismatch: expected %s, got %s", fixedNonce, headers.XSignatureNonce)
	}
}

// TestSignerKnownAnswerTest_CaseB verifies the signer against a pinned known answer with body.
// Case B: path="/openapi/trade/order/place", no query, body={"account_id":"ACC123","new_orders":[{"symbol":"AAPL"}]}
// Body MD5 upper = "234BF3AD800833DF4B37304BFA897448"
// Expected signature = "c0wjmfwNUyQmI1dZ3JBfSgsZ9ss="
// With same credentials and timestamp/nonce as Case A
func TestSignerKnownAnswerTest_CaseB(t *testing.T) {
	signer := NewSigner("testappkey000000000000000000abcd", "testappsecret00000000000000000000")

	// Pin time and nonce (same as Case A)
	fixedTime := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	fixedNonce := "0123456789abcdef0123456789abcdef"

	signer.now = func() time.Time { return fixedTime }
	signer.nonceFn = func() (string, error) { return fixedNonce, nil }

	// Case B: POST /openapi/trade/order/place with body
	path := "/openapi/trade/order/place"
	body := []byte(`{"account_id":"ACC123","new_orders":[{"symbol":"AAPL"}]}`)

	headers, err := signer.SignRequest(path, "POST", "api.webull.com", nil, body)
	if err != nil {
		t.Fatalf("SignRequest failed: %v", err)
	}

	expectedSig := "c0wjmfwNUyQmI1dZ3JBfSgsZ9ss="
	if headers.XSignature != expectedSig {
		t.Errorf("Case B signature mismatch:\n  expected: %s\n  got:      %s", expectedSig, headers.XSignature)
	}

	// Verify the body MD5 is computed correctly
	md5Hash := signer.canonicalString(path, nil, body, "2026-01-02T03:04:05Z", fixedNonce, "api.webull.com")
	// The canonical string should contain the MD5 hash
	if !stringContains(md5Hash, "234BF3AD800833DF4B37304BFA897448") {
		t.Errorf("Body MD5 not found in canonical string: %s", md5Hash)
	}
}

// TestNonceIsHex32 verifies that randomNonce generates 32 hex characters
func TestNonceIsHex32(t *testing.T) {
	nonce, err := randomNonce()
	if err != nil {
		t.Fatalf("randomNonce failed: %v", err)
	}

	// Should be 32 characters (16 bytes → 32 hex chars)
	if len(nonce) != 32 {
		t.Errorf("nonce length = %d, want 32", len(nonce))
	}

	// Should be valid hex
	if _, err := hex.DecodeString(nonce); err != nil {
		t.Errorf("nonce is not valid hex: %v", err)
	}

	// Verify it's different on successive calls (with overwhelming probability)
	nonce2, _ := randomNonce()
	if nonce == nonce2 {
		t.Errorf("successive randomNonce() calls produced identical nonces (should be crypto random)")
	}

	t.Logf("Sample nonce: %s", nonce)
}

// TestSignatureConsistency verifies that the signature is deterministic given fixed inputs
func TestSignatureConsistency(t *testing.T) {
	signer := NewSigner("key1", "secret1")

	fixedTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	fixedNonce := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa1"

	signer.now = func() time.Time { return fixedTime }
	signer.nonceFn = func() (string, error) { return fixedNonce, nil }

	path := "/openapi/assets/balance"
	query := url.Values{}
	query.Set("account_id", "ACC123")

	sig1, err := signer.SignRequest(path, "GET", "api.webull.com", query, nil)
	if err != nil {
		t.Fatalf("SignRequest failed: %v", err)
	}
	sig2, err := signer.SignRequest(path, "GET", "api.webull.com", query, nil)
	if err != nil {
		t.Fatalf("SignRequest failed: %v", err)
	}

	if sig1.XSignature != sig2.XSignature {
		t.Errorf("signatures diverged for identical requests")
	}
}

// stringContains is a simple substring search helper for test validation
func stringContains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
