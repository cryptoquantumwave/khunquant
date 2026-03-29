package settrade

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/asn1"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math/big"
	"time"
)

// loadPrivateKey decodes a base64-encoded raw 32-byte ECDSA P-256 private key scalar
// (big-endian). This matches the format expected by the Settrade SDK v2 (app_secret).
func loadPrivateKey(b64 string) (*ecdsa.PrivateKey, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		raw, err = base64.URLEncoding.DecodeString(b64)
		if err != nil {
			return nil, fmt.Errorf("settrade: decode app_secret base64: %w", err)
		}
	}
	curve := elliptic.P256()
	d := new(big.Int).SetBytes(raw)
	priv := &ecdsa.PrivateKey{
		D:         d,
		PublicKey: ecdsa.PublicKey{Curve: curve},
	}
	priv.PublicKey.X, priv.PublicKey.Y = curve.ScalarBaseMult(raw)
	return priv, nil
}

// ecdsaDERSig holds the ASN.1 sequence for an ECDSA signature.
type ecdsaDERSig struct {
	R, S *big.Int
}

// sign builds the Settrade login signature.
//
// Payload: apiKey + "." + params + "." + timestamp_ms_string
// Hash: SHA-256 of the UTF-8 payload.
// Signature: ECDSA P-256 → ASN.1 DER → hex string.
//
// Returns the hex-encoded DER signature and the Unix millisecond timestamp used.
func sign(key *ecdsa.PrivateKey, apiKey, params string) (sigHex string, tsMs int64, err error) {
	tsMs = time.Now().UnixMilli()
	payload := fmt.Sprintf("%s.%s.%d", apiKey, params, tsMs)
	digest := sha256.Sum256([]byte(payload))

	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		return "", 0, fmt.Errorf("settrade: ECDSA sign: %w", err)
	}

	derBytes, err := asn1.Marshal(ecdsaDERSig{R: r, S: s})
	if err != nil {
		return "", 0, fmt.Errorf("settrade: DER marshal signature: %w", err)
	}
	return hex.EncodeToString(derBytes), tsMs, nil
}
