package webull

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
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
	now       func() time.Time // injectable time source
	nonceFn   func() string    // injectable nonce generator
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
// path: the request path (e.g. "/v2/trading/accounts/12345/balances")
// method: HTTP method (e.g. "GET", "POST")
// query: URL query parameters (can be nil or empty)
// body: request body bytes (can be nil or empty). If provided, the same bytes must be sent on the wire.
// host: the request host (e.g. "api.webull.com") — must match the actual request host for signature validity.
// Returns the signature headers to set on the request.
func (s *Signer) SignRequest(path, method, host string, query url.Values, body []byte) SignHeaders {
	timestamp := s.now().UTC().Format("2006-01-02T15:04:05Z")
	nonce := s.nonceFn()

	// Build canonical string
	canonStr := s.canonicalString(path, query, body, timestamp, nonce, host)

	// Sign with HMAC-SHA1
	key := s.appSecret + "&"
	h := hmac.New(sha1.New, []byte(key))
	h.Write([]byte(canonStr))
	signature := base64.StdEncoding.EncodeToString(h.Sum(nil))

	return SignHeaders{
		XAppKey:             s.appKey,
		XTimestamp:          timestamp,
		XSignature:          signature,
		XSignatureAlgorithm: "HMAC-SHA1",
		XSignatureVersion:   "1.0",
		XSignatureNonce:     nonce,
		XVersion:            "v2",
	}
}

// canonicalString builds the signed canonical string.
// Steps:
//  1. Merge query params + signing headers into an alphabetically sorted string,
//     percent-encoding each key and value individually before joining:
//     name1=value1&name2=value2&...
//     This mirrors url.Values.Encode(), which is what client.go uses to build the
//     actual wire query string — encoding the whole joined string in one pass
//     (the previous approach) would also escape the literal "&"/"=" separators,
//     diverging from the wire representation for any value containing those
//     characters, "+", or a space.
//  2. If body exists: compute MD5 hex uppercase
//  3. Build: path + "&" + str1 + "&" + str2 (omit &str2 if no body)
//
// NOTE: unverified against the live Webull API — docs/webull-integration.md
// tracks confirming the exact expected canonicalization once real credentials
// are available.
func (s *Signer) canonicalString(path string, query url.Values, body []byte, timestamp, nonce, host string) string {
	// Collect all params
	params := make(map[string]string)

	// Add query parameters
	if query != nil {
		for k, vs := range query {
			if len(vs) > 0 {
				params[k] = vs[0]
			}
		}
	}

	// Add signing headers
	params["x-app-key"] = s.appKey
	params["x-timestamp"] = timestamp
	params["x-signature-algorithm"] = "HMAC-SHA1"
	params["x-signature-version"] = "1.0"
	params["x-signature-nonce"] = nonce
	params["host"] = host

	// Sort by key and build string
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
		sb.WriteString(url.QueryEscape(k))
		sb.WriteString("=")
		sb.WriteString(url.QueryEscape(params[k]))
	}
	str1 := sb.String()

	// Body MD5 hash (uppercase hex)
	var str2 string
	if len(body) > 0 {
		md5Hash := md5.Sum(body)
		str2 = strings.ToUpper(hex.EncodeToString(md5Hash[:]))
	}

	// Build canonical string
	if str2 != "" {
		return path + "&" + str1 + "&" + str2
	}
	return path + "&" + str1
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

// randomNonce generates a genuinely random 16-character alphanumeric nonce using crypto/rand.
// Each byte is independently random from the alphabet to ensure proper entropy.
func randomNonce() string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	nonce := make([]byte, 16)
	// Read 16 random bytes from crypto/rand.
	// Each byte will be uniformly distributed in [0, 255].
	if _, err := rand.Read(nonce); err != nil {
		// Fallback to time-based if crypto/rand fails (should never happen in practice).
		for i := range nonce {
			nonce[i] = alphabet[time.Now().UnixNano()%int64(len(alphabet))]
		}
		return string(nonce)
	}
	// Map each random byte into the alphabet.
	result := make([]byte, 16)
	for i, b := range nonce {
		result[i] = alphabet[b%byte(len(alphabet))]
	}
	return string(result)
}
