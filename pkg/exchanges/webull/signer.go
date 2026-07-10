package webull

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Signer handles Webull HMAC-SHA1 request signing.
// It is pure (no I/O) and testable via injectable timestamp and nonce functions.
type Signer struct {
	appKey    string
	appSecret string
	now       func() time.Time       // injectable time source
	nonceFn   func() (string, error) // injectable nonce generator
}

// NewSigner creates a signer with real time.Now and crypto random nonce.
func NewSigner(appKey, appSecret string) *Signer {
	return &Signer{
		appKey:    appKey,
		appSecret: appSecret,
		now:       time.Now,
		nonceFn:   randomNonce,
	}
}

// SignRequest builds the canonical string and HMAC-SHA1 signature for a Webull API request.
// path: the request path (e.g. "/openapi/assets/balance")
// method: HTTP method (e.g. "GET", "POST") — not used in signature but kept for future extensibility
// query: URL query parameters (can be nil or empty)
// body: request body bytes (can be nil or empty). If provided, the same bytes must be sent on the wire.
// host: the request host (e.g. "api.webull.com") — must match the actual request host for signature validity.
// Returns the signature headers to set on the request.
func (s *Signer) SignRequest(path, method, host string, query url.Values, body []byte) (SignHeaders, error) {
	timestamp := s.now().UTC().Format("2006-01-02T15:04:05Z")
	nonce, err := s.nonceFn()
	if err != nil {
		return SignHeaders{}, fmt.Errorf("webull: generate nonce: %w", err)
	}

	// Build str3 (path + params + optional body MD5)
	str3 := s.canonicalString(path, query, body, timestamp, nonce, host)

	// URL-encode the ENTIRE str3 (critical step — whole-string encoding)
	encoded := url.QueryEscape(str3)

	// Sign with HMAC-SHA1
	key := s.appSecret + "&"
	h := hmac.New(sha1.New, []byte(key))
	h.Write([]byte(encoded))
	signature := base64.StdEncoding.EncodeToString(h.Sum(nil))

	return SignHeaders{
		XAppKey:             s.appKey,
		XTimestamp:          timestamp,
		XSignature:          signature,
		XSignatureAlgorithm: "HMAC-SHA1",
		XSignatureVersion:   "1.0",
		XSignatureNonce:     nonce,
		XVersion:            "v2",
	}, nil
}

// canonicalString builds the signed canonical string per the verified Webull algorithm.
// Steps:
//  1. Merge query params + signing headers into an alphabetically sorted map.
//  2. Sort by key and build str1 = "k1=v1&k2=v2&..." using PLAIN values (NOT url-encoded).
//  3. If body exists: str2 = toUpper(hex(MD5(body)))
//  4. str3 = path + "&" + str1 (+ "&" + str2 if body present)
//  5. CRITICAL: URL-encode the ENTIRE str3 before HMAC signing.
func (s *Signer) canonicalString(path string, query url.Values, body []byte, timestamp, nonce, host string) string {
	// Collect all params
	params := make(map[string]string)

	// Add query parameters (ranging over nil map is a no-op)
	for k, vs := range query {
		if len(vs) > 0 {
			params[k] = vs[0]
		}
	}

	// Add signing headers
	params["x-app-key"] = s.appKey
	params["x-timestamp"] = timestamp
	params["x-signature-algorithm"] = "HMAC-SHA1"
	params["x-signature-version"] = "1.0"
	params["x-signature-nonce"] = nonce
	params["host"] = host

	// Sort by key and build str1 with PLAIN values (no per-component encoding)
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var sb strings.Builder
	for i, k := range keys {
		if i > 0 {
			sb.WriteString("&")
		}
		sb.WriteString(k)
		sb.WriteString("=")
		sb.WriteString(params[k])
	}
	str1 := sb.String()

	// Body MD5 hash (uppercase hex)
	var str2 string
	if len(body) > 0 {
		md5Hash := md5.Sum(body)
		str2 = strings.ToUpper(hex.EncodeToString(md5Hash[:]))
	}

	// Build str3 (canonical string before encoding)
	var str3 string
	if str2 != "" {
		str3 = path + "&" + str1 + "&" + str2
	} else {
		str3 = path + "&" + str1
	}

	// Return str3; will be URL-encoded in SignRequest before HMAC
	return str3
}

// SignHeaders holds the computed signature headers for a Webull request.
type SignHeaders struct {
	XAppKey             string
	XTimestamp          string
	XSignature          string
	XSignatureAlgorithm string
	XSignatureVersion   string
	XSignatureNonce     string
	XVersion            string
}

// randomNonce generates a 32-character hexadecimal nonce (16 random bytes → 32 hex chars).
// A crypto/rand failure is unrecoverable — returning an error is strictly better than
// emitting a predictable nonce, which would weaken replay protection.
func randomNonce() (string, error) {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("crypto/rand read: %w", err)
	}
	return hex.EncodeToString(nonce), nil
}
