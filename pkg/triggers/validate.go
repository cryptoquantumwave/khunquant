package triggers

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// modeRank orders the modes: off < shadow < live. ok is false for any other text.
func modeRank(m string) (rank int, ok bool) {
	switch m {
	case ModeOff:
		return 0, true
	case ModeShadow:
		return 1, true
	case ModeLive:
		return 2, true
	}
	return 0, false
}

// globalRank reads the global mode. Anything but a known mode counts as off, so a
// config that cannot be read never turns evaluation on.
func globalRank(m string) int {
	r, _ := modeRank(strings.ToLower(strings.TrimSpace(m)))
	return r
}

// effectiveRank is min(global, spec). specMode must be a valid mode.
func effectiveRank(global int, specMode string) int {
	r, _ := modeRank(specMode)
	return min(global, r)
}

// validToken reports whether s is 1..max bytes of [A-Za-z0-9_.-], starting with a
// letter or digit: the shape of a spec ID and of a kind.
func validToken(s string, max int, allowUpper bool) bool {
	if s == "" || len(s) > max {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c >= 'A' && c <= 'Z':
			if !allowUpper {
				return false
			}
		case c == '_' || c == '-' || c == '.':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// plainText reports whether s has no control or unassigned characters.
func plainText(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) || !unicode.IsPrint(r) && r != ' ' {
			return false
		}
	}
	return true
}

// validateSpec checks a spec's shape: everything that does not need the factory.
// Params must already be normalised (empty becomes "{}").
func validateSpec(s Spec) error {
	switch {
	case !validToken(s.ID, maxIDLen, true):
		return fmt.Errorf("%w: id must be 1-%d characters of letters, digits, '_', '-' or '.'", ErrInvalidSpec, maxIDLen)
	case !validToken(s.Kind, maxKindLen, false):
		return fmt.Errorf("%w: kind must be 1-%d lower-case letters, digits, '_', '-' or '.'", ErrInvalidSpec, maxKindLen)
	}
	if _, ok := modeRank(s.Mode); !ok {
		return fmt.Errorf("%w: mode must be off, shadow or live", ErrInvalidSpec)
	}
	if strings.TrimSpace(s.Origin) == "" {
		return fmt.Errorf("%w: origin is required", ErrInvalidSpec)
	}
	if utf8.RuneCountInString(s.Origin) > MaxOriginRunes || !plainText(s.Origin) {
		return fmt.Errorf("%w: origin must be plain text of at most %d characters", ErrInvalidSpec, MaxOriginRunes)
	}
	if len(s.IntentID) > maxIntentIDLen || !plainText(s.IntentID) {
		return fmt.Errorf("%w: intent id must be plain text of at most %d bytes", ErrInvalidSpec, maxIntentIDLen)
	}
	if len(s.Params) > MaxParamsBytes {
		return fmt.Errorf("%w: params are %d bytes, the limit is %d", ErrInvalidSpec, len(s.Params), MaxParamsBytes)
	}
	if !json.Valid(s.Params) {
		return fmt.Errorf("%w: params are not valid JSON", ErrInvalidSpec)
	}
	return nil
}

// validateDemands checks what a trigger asked perception to fetch.
func validateDemands(ds []perception.Demand) error {
	if len(ds) > MaxDemandsPerTrigger {
		return fmt.Errorf("%d demands, the limit is %d", len(ds), MaxDemandsPerTrigger)
	}
	for _, d := range ds {
		if len(d.Provider) > maxSymbolLen || len(d.Account) > maxSymbolLen ||
			len(d.Symbol) > maxSymbolLen || len(d.Timeframe) > 16 {
			return fmt.Errorf("demand for %q has a field over its limit", d.Symbol)
		}
		if d.MinInterval < 0 || d.Lookback < 0 {
			return fmt.Errorf("demand for %q has a negative interval or lookback", d.Symbol)
		}
	}
	return nil
}

// checkCandidate refuses a malformed candidate. kind is the spec's kind: the
// dedup key must start with "<kind>:" and be of the form <kind>:<subject>:<variant>
// (the subject may itself hold ':', as in BTC/USDT:USDT, so the rule is that some
// ':' follows the prefix with text on both sides).
func checkCandidate(kind string, c proactive.Candidate) error {
	key := c.DedupKey
	switch {
	case key == "":
		return fmt.Errorf("empty dedup key")
	case len(key) > MaxDedupKeyBytes:
		return fmt.Errorf("dedup key is %d bytes, the limit is %d", len(key), MaxDedupKeyBytes)
	case !strings.HasPrefix(key, kind+":"):
		return fmt.Errorf("dedup key must start with %q", kind+":")
	case !plainText(key) || strings.ContainsAny(key, " \t"):
		return fmt.Errorf("dedup key has whitespace or control characters")
	}
	if rest := key[len(kind)+1:]; strings.HasPrefix(rest, ":") || strings.HasSuffix(rest, ":") ||
		!strings.Contains(rest, ":") {
		return fmt.Errorf("dedup key must have the form <kind>:<subject>:<variant>")
	}
	if c.Severity < proactive.SeverityInfo || c.Severity > proactive.SeverityCritical {
		return fmt.Errorf("severity %d out of range", int(c.Severity))
	}
	if c.Protective && c.Severity != proactive.SeverityCritical {
		return fmt.Errorf("a protective candidate must be critical")
	}
	if c.Compose != proactive.ComposeTemplate && c.Compose != proactive.ComposeLLM {
		return fmt.Errorf("compose mode %d out of range", int(c.Compose))
	}
	if math.IsNaN(c.Salience) || math.IsInf(c.Salience, 0) || c.Salience < 0 || c.Salience > 1 {
		return fmt.Errorf("salience must be a number from 0 to 1")
	}
	if c.DedupWindow < 0 {
		return fmt.Errorf("negative dedup window")
	}
	if strings.TrimSpace(c.Title) == "" || len(c.Title) > MaxTitleBytes || !utf8.ValidString(c.Title) {
		return fmt.Errorf("title must be 1-%d bytes of text", MaxTitleBytes)
	}
	if len(c.Subject) > MaxSubjectBytes || !utf8.ValidString(c.Subject) {
		return fmt.Errorf("subject is over %d bytes", MaxSubjectBytes)
	}
	if len(c.ForcedSkills) > MaxForcedSkills {
		return fmt.Errorf("%d forced skills, the limit is %d", len(c.ForcedSkills), MaxForcedSkills)
	}
	if len(c.Evidence) > 0 {
		// A value that does not encode (NaN, Inf, a channel) cannot be stored by
		// attention either, so the encoding is the finiteness check.
		b, err := json.Marshal(c.Evidence)
		if err != nil {
			return fmt.Errorf("evidence cannot be encoded: %w", err)
		}
		if len(b) > MaxEvidenceBytes {
			return fmt.Errorf("evidence is %d bytes, the limit is %d", len(b), MaxEvidenceBytes)
		}
	}
	return nil
}
