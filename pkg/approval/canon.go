package approval

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"unicode"
)

// codeAlphabet is [A-Z2-9] without the look-alikes 0, 1, I and O: 32 characters,
// so a random byte masked to 5 bits is uniform.
const codeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// CodeLen is the length of a proposal code.
const CodeLen = 4

// newCode returns a random code. Uniqueness among pending proposals is the
// database's job (a partial unique index); the caller retries on a conflict.
func newCode() string {
	var raw [CodeLen]byte
	mustRand(raw[:])
	out := make([]byte, CodeLen)
	for i, b := range raw {
		out[i] = codeAlphabet[int(b)&31]
	}
	return string(out)
}

// newNonce returns 8 random bytes as hex.
func newNonce() string {
	var raw [8]byte
	mustRand(raw[:])
	return hex.EncodeToString(raw[:])
}

func mustRand(b []byte) {
	if _, err := rand.Read(b); err != nil {
		// crypto/rand does not fail on supported platforms; a proposal whose code
		// and nonce cannot be random must not exist.
		panic("approval: no randomness: " + err.Error())
	}
}

// NormalizeCode upper-cases and trims a code as typed by the owner.
func NormalizeCode(code string) string { return strings.ToUpper(strings.TrimSpace(code)) }

// ValidCode reports whether s has the shape of a code.
func ValidCode(s string) bool {
	if len(s) != CodeLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		if strings.IndexByte(codeAlphabet, s[i]) < 0 {
			return false
		}
	}
	return true
}

// ButtonPayload is the payload of an Approve or Reject button: "<code>.<nonce>".
func ButtonPayload(code, nonce string) string { return code + "." + nonce }

// ParseButtonPayload splits a button payload.
func ParseButtonPayload(payload string) (code, nonce string, ok bool) {
	code, nonce, ok = strings.Cut(payload, ".")
	if !ok || !ValidCode(code) || nonce == "" {
		return "", "", false
	}
	return code, nonce, true
}

// nonceEqual compares nonces in constant time (both sides are padded to the same
// length so the comparison time does not depend on where they differ).
func nonceEqual(stored, given string) bool {
	return subtle.ConstantTimeCompare([]byte(stored), []byte(given)) == 1
}

// hashInput is the document that is hashed: the kind and the params, as canonical
// JSON. Struct field order is fixed, so the encoding is deterministic.
type hashInput struct {
	Kind   Kind            `json:"kind"`
	Params json.RawMessage `json:"params"`
}

// hashOf returns sha256 of the canonical JSON of {kind, params}.
func hashOf(kind Kind, params json.RawMessage) [32]byte {
	b, _ := json.Marshal(hashInput{Kind: kind, Params: params})
	return sha256.Sum256(b)
}

// canonicalOrder is the canonical JSON of p.
func canonicalOrder(p OrderParams) (json.RawMessage, error) {
	if err := validateOrderParams(p); err != nil {
		return nil, err
	}
	b, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("approval: encode params: %w", err)
	}
	return b, nil
}

// errHashMismatch: the stored row no longer matches its hash, or is not canonical.
var errHashMismatch = errors.New("approval: the stored proposal does not match its hash; refusing to act on it")

// decodeVerified decodes a stored proposal's params and proves they are exactly
// what was hashed: the hash of the stored bytes matches (constant time), the
// bytes decode with no unknown field, and encoding the decoded struct gives the
// stored bytes back (so nothing extra survives the decode), and the params are valid.
func decodeVerified(p Proposal) (OrderParams, error) {
	if p.Kind != KindOrder {
		return OrderParams{}, fmt.Errorf("approval: unsupported kind %q", p.Kind)
	}
	want := hashOf(p.Kind, p.Params)
	if subtle.ConstantTimeCompare(want[:], p.Hash[:]) != 1 {
		return OrderParams{}, errHashMismatch
	}
	dec := json.NewDecoder(bytes.NewReader(p.Params))
	dec.DisallowUnknownFields()
	var op OrderParams
	if err := dec.Decode(&op); err != nil {
		return OrderParams{}, fmt.Errorf("%w: %v", errHashMismatch, err)
	}
	if dec.More() {
		return OrderParams{}, errHashMismatch
	}
	again, err := json.Marshal(op)
	if err != nil || !bytes.Equal(again, p.Params) {
		return OrderParams{}, errHashMismatch
	}
	if err := validateOrderParams(op); err != nil {
		return OrderParams{}, fmt.Errorf("%w: %v", errHashMismatch, err)
	}
	return op, nil
}

// ErrSpotContractSymbol is the refusal for a spot order on a contract symbol
// (one containing ":"). The exchange library resolves such a symbol to the
// perpetual, which would bypass the leverage opt-in and value the order without
// the contract size, so the model is told how to recover.
const ErrSpotContractSymbol = "a contract symbol (it contains \":\", e.g. BTC/USDT:USDT) is not a spot symbol: use market=futures for contract symbols"

func finitePositive(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v > 0 }

// validateOrderParams enforces what every stored order must satisfy: finite,
// positive quantities and a USD value, known enums.
func validateOrderParams(p OrderParams) error {
	switch {
	case strings.TrimSpace(p.Provider) == "" || strings.TrimSpace(p.Symbol) == "":
		return errors.New("provider and symbol are required")
	case p.Market != MarketSpot && p.Market != MarketFutures:
		return errors.New("market must be spot or futures")
	case p.Side != "buy" && p.Side != "sell":
		return errors.New("side must be buy or sell")
	case p.Type != "market" && p.Type != "limit":
		return errors.New("type must be market or limit")
	case !finitePositive(p.Amount):
		return errors.New("amount must be a positive, finite number")
	case !finitePositive(p.RefPrice):
		return errors.New("reference price must be a positive, finite number")
	case !finitePositive(p.NotionalUSD):
		return errors.New("USD notional must be a positive, finite number")
	case !finitePositive(p.PriceCollarPct) || p.PriceCollarPct > 50:
		return errors.New("price collar must be a positive percentage of at most 50")
	case p.Type == "limit" && (p.Price == nil || !finitePositive(*p.Price)):
		return errors.New("a limit order needs a positive, finite price")
	case p.Type == "market" && p.Price != nil:
		return errors.New("a market order carries no price")
	case p.Market == MarketSpot && (p.ReduceOnly || p.PositionSide != "" || p.MarginMode != ""):
		return errors.New("futures-only fields set on a spot order")
	case p.Market == MarketFutures && !strings.Contains(p.Symbol, ":"):
		return errors.New("a futures symbol is a contract symbol such as BTC/USDT:USDT")
	case p.Market == MarketSpot && strings.Contains(p.Symbol, ":"):
		return errors.New(ErrSpotContractSymbol)
	}
	return nil
}

// CleanRationale checks the model's optional prose and returns it trimmed. It is
// one paragraph of at most MaxRationaleRunes characters; any control, line-break
// or invisible formatting character (bidi overrides included) refuses it rather
// than being stripped, so what the owner reads is exactly what the model wrote.
func CleanRationale(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	if n := len([]rune(s)); n > MaxRationaleRunes {
		return "", fmt.Errorf("rationale is %d characters; at most %d are allowed", n, MaxRationaleRunes)
	}
	for _, r := range s {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) {
			return "", errors.New("rationale must be one plain paragraph: control, line-break and invisible formatting characters are not allowed")
		}
	}
	return s, nil
}
