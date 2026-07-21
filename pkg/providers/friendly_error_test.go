package providers

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestFriendlyError_AntigravityRateLimit(t *testing.T) {
	// The real-world single-candidate case: raw provider error, unclassified,
	// carrying an embedded reset hint.
	raw := errors.New("antigravity rate limit exceeded: Individual quota reached. " +
		"Please upgrade your subscription to increase your limits. Resets in 94h48m42s. " +
		"(reset in 94h48m42.150784164s)")

	// Simulate the loop boundary: classify then wrap.
	classified := EnsureClassified(raw, "antigravity", "gemini-3.5-flash-low")
	wrapped := fmt.Errorf("LLM call failed after retries: %w", classified)

	got := FriendlyError(wrapped)
	if !strings.Contains(got, "Rate limit reached") {
		t.Fatalf("expected rate-limit message, got: %q", got)
	}
	if !strings.Contains(got, "antigravity") {
		t.Fatalf("expected provider name, got: %q", got)
	}
	// 94h48m42s is humanized (rounded to days) rather than dumped raw.
	if !strings.Contains(got, "Try again in 4d") {
		t.Fatalf("expected humanized reset hint, got: %q", got)
	}
	if strings.Contains(got, "94h48m42s") {
		t.Fatalf("raw duration should be humanized, got: %q", got)
	}
}

func TestFriendlyError_FallbackExhausted(t *testing.T) {
	err := &FallbackExhaustedError{Attempts: []FallbackAttempt{
		{Provider: "openai", Model: "gpt-x", Error: errors.New("connection reset by peer"), Duration: time.Second},
		{Provider: "anthropic", Model: "claude-x", Error: errors.New("HTTP 429 Too Many Requests"), Duration: time.Second},
	}}

	got := FriendlyError(err)
	// rate_limit (priority 4) must win over network (priority 1).
	if !strings.Contains(got, "Rate limit reached") {
		t.Fatalf("expected rate-limit to dominate, got: %q", got)
	}
	if !strings.Contains(got, "anthropic") {
		t.Fatalf("expected dominant provider anthropic, got: %q", got)
	}
}

func TestFriendlyError_Reasons(t *testing.T) {
	cases := []struct {
		reason FailoverReason
		want   string
	}{
		{FailoverBilling, "Billing issue"},
		{FailoverAuth, "Authentication failed"},
		{FailoverContextOverflow, "too long"},
		{FailoverTimeout, "timed out"},
		{FailoverNetwork, "Network error"},
	}
	for _, tc := range cases {
		fe := &FailoverError{Reason: tc.reason, Provider: "p", Wrapped: errors.New("x")}
		got := FriendlyError(fe)
		if !strings.Contains(got, tc.want) {
			t.Errorf("reason %s: want substring %q, got %q", tc.reason, tc.want, got)
		}
	}
}

func TestFriendlyError_UnknownFallsBackToRaw(t *testing.T) {
	err := errors.New("something totally unrecognized happened")
	got := FriendlyError(err)
	if !strings.Contains(got, "something totally unrecognized") {
		t.Fatalf("expected raw text passthrough, got: %q", got)
	}
}

func TestParseRetryAfter(t *testing.T) {
	cases := []struct {
		msg  string
		want time.Duration
	}{
		{"resets in 94h48m42s.", 94*time.Hour + 48*time.Minute + 42*time.Second},
		{"api request failed: status 429 (retry after 30s)", 30 * time.Second},
		{"retry-after: 120", 120 * time.Second},
		{"nothing here", 0},
	}
	for _, tc := range cases {
		if got := parseRetryAfter(tc.msg); got != tc.want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", tc.msg, got, tc.want)
		}
	}
}

func TestFriendlyError_PrefersTypedRetryAfter(t *testing.T) {
	fe := &FailoverError{
		Reason:     FailoverRateLimit,
		Provider:   "anthropic",
		RetryAfter: 90 * time.Second,
		Wrapped:    errors.New("HTTP 429"),
	}
	got := FriendlyError(fe)
	if !strings.Contains(got, "Try again in 2m") { // 90s rounds to 2m
		t.Fatalf("expected humanized retry-after, got: %q", got)
	}
}

func TestClassifyError_PopulatesRetryAfter(t *testing.T) {
	err := errors.New("API request failed: Status: 429 (retry after 45s)")
	fe := ClassifyError(err, "openai", "gpt-x")
	if fe == nil {
		t.Fatal("expected classification")
	}
	if fe.Reason != FailoverRateLimit {
		t.Fatalf("expected rate_limit, got %s", fe.Reason)
	}
	if fe.RetryAfter != 45*time.Second {
		t.Fatalf("expected RetryAfter=45s, got %v", fe.RetryAfter)
	}
}

func TestEnsureClassified_PreservesExisting(t *testing.T) {
	fe := &FailoverError{Reason: FailoverAuth, Provider: "p", Wrapped: errors.New("x")}
	if got := EnsureClassified(fe, "other", "m"); got != fe {
		t.Fatalf("expected existing FailoverError to be returned unchanged")
	}
	fx := &FallbackExhaustedError{}
	if got := EnsureClassified(fx, "other", "m"); got != error(fx) {
		t.Fatalf("expected existing FallbackExhaustedError to be returned unchanged")
	}
}
