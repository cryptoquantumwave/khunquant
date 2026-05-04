package settrade

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/asn1"
	"encoding/base64"
	"math/big"
	"testing"
)

// generateTestKeyB64 creates a random P-256 private key and returns its
// raw scalar encoded as base64 standard encoding (matching loadPrivateKey input format).
func generateTestKeyB64(t *testing.T) (string, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	raw := key.D.Bytes()
	// Pad to 32 bytes.
	padded := make([]byte, 32)
	copy(padded[32-len(raw):], raw)
	b64 := base64.StdEncoding.EncodeToString(padded)
	return b64, key
}

func TestLoadPrivateKey_Valid(t *testing.T) {
	b64, _ := generateTestKeyB64(t)
	key, err := loadPrivateKey(b64)
	if err != nil {
		t.Fatalf("loadPrivateKey: %v", err)
	}
	if key == nil {
		t.Fatal("expected non-nil key")
	}
	if key.Curve != elliptic.P256() {
		t.Error("expected P-256 curve")
	}
}

func TestLoadPrivateKey_InvalidBase64(t *testing.T) {
	_, err := loadPrivateKey("!!!not_base64!!!")
	if err == nil {
		t.Error("expected error for invalid base64 input")
	}
}

func TestSign_ProducesValidSignature(t *testing.T) {
	b64, origKey := generateTestKeyB64(t)

	key, err := loadPrivateKey(b64)
	if err != nil {
		t.Fatalf("loadPrivateKey: %v", err)
	}

	sigHex, tsMs, err := sign(key, "test-api-key", "param1=value1", 0)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if sigHex == "" {
		t.Error("expected non-empty signature hex")
	}
	if tsMs <= 0 {
		t.Errorf("expected positive timestamp, got %d", tsMs)
	}

	// Reconstruct the payload that was signed and verify the ECDSA signature.
	import_fmt := func(apiKey, params string, ts int64) string {
		return apiKey + "." + params + "." + intToStr(ts)
	}
	payload := import_fmt("test-api-key", "param1=value1", tsMs)
	digest := sha256.Sum256([]byte(payload))

	// Decode hex → DER bytes → R, S.
	derBytes, err := hexDecode(sigHex)
	if err != nil {
		t.Fatalf("hex decode signature: %v", err)
	}
	var sig struct{ R, S *big.Int }
	if _, err := asn1.Unmarshal(derBytes, &sig); err != nil {
		t.Fatalf("ASN.1 unmarshal: %v", err)
	}

	// Verify against the original key's public key.
	if !ecdsa.Verify(&origKey.PublicKey, digest[:], sig.R, sig.S) {
		t.Error("signature verification failed")
	}
}

func TestSign_SignatureChangesWithDifferentInputs(t *testing.T) {
	b64, _ := generateTestKeyB64(t)
	key, _ := loadPrivateKey(b64)

	sig1, _, _ := sign(key, "api-key-1", "params", 0)
	sig2, _, _ := sign(key, "api-key-2", "params", 0)

	if sig1 == sig2 {
		t.Error("expected different signatures for different API keys (ECDSA is randomized)")
	}
}

// intToStr converts int64 to decimal string without importing fmt.
func intToStr(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	buf := make([]byte, 0, 20)
	for n > 0 {
		buf = append(buf, byte('0'+n%10))
		n /= 10
	}
	if neg {
		buf = append(buf, '-')
	}
	// Reverse.
	for i, j := 0, len(buf)-1; i < j; i, j = i+1, j-1 {
		buf[i], buf[j] = buf[j], buf[i]
	}
	return string(buf)
}

// hexDecode decodes a hex string without importing encoding/hex.
func hexDecode(s string) ([]byte, error) {
	if len(s)%2 != 0 {
		return nil, &hexErr{s}
	}
	b := make([]byte, len(s)/2)
	for i := range b {
		hi, ok1 := hexNibble(s[2*i])
		lo, ok2 := hexNibble(s[2*i+1])
		if !ok1 || !ok2 {
			return nil, &hexErr{s}
		}
		b[i] = hi<<4 | lo
	}
	return b, nil
}

type hexErr struct{ s string }

func (e *hexErr) Error() string { return "invalid hex: " + e.s }

func hexNibble(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}
