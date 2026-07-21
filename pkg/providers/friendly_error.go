package providers

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// reasonPriority ranks failover reasons so that the most user-actionable one
// wins when several candidates fail for different reasons. Higher is stronger.
var reasonPriority = map[FailoverReason]int{
	FailoverBilling:         6,
	FailoverAuth:            5,
	FailoverRateLimit:       4,
	FailoverOverloaded:      4,
	FailoverContextOverflow: 3,
	FailoverFormat:          2,
	FailoverTimeout:         1,
	FailoverNetwork:         1,
	FailoverUnknown:         0,
}

// retryAfterPattern extracts a retry/reset duration that providers embed in a
// rate-limit message body, covering both the antigravity form
// ("resets in 94h48m42s") and the Retry-After header form that providers append
// ("retry after 30s").
var retryAfterPattern = regexp.MustCompile(`(?i)(?:resets?\s+in|retry[- ]after:?)\s+([0-9hmsdw.]+)`)

// parseRetryAfter extracts a retry/reset wait from an error message, returning
// 0 when none is present. It accepts Go-style durations ("94h48m42s") and a
// bare number of seconds ("30").
func parseRetryAfter(msg string) time.Duration {
	m := retryAfterPattern.FindStringSubmatch(msg)
	if len(m) < 2 {
		return 0
	}
	token := strings.TrimRight(m[1], ".")
	if token == "" {
		return 0
	}
	if d, err := time.ParseDuration(token); err == nil {
		return d
	}
	if secs, err := strconv.Atoi(token); err == nil {
		return time.Duration(secs) * time.Second
	}
	return 0
}

// humanizeDuration renders a duration as a short, rounded, user-facing string.
func humanizeDuration(d time.Duration) string {
	switch {
	case d <= 0:
		return ""
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Round(time.Second).Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Round(time.Minute).Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Round(time.Hour).Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Round(24*time.Hour).Hours()/24))
	}
}

// EnsureClassified guarantees the returned error carries failover
// classification metadata. If err already is (or wraps) a *FailoverError or
// *FallbackExhaustedError, it is returned unchanged. Otherwise ClassifyError is
// applied so that single-candidate calls — which never pass through the
// fallback chain — still reach the user boundary with a typed reason.
func EnsureClassified(err error, provider, model string) error {
	if err == nil {
		return nil
	}
	var fe *FailoverError
	if errors.As(err, &fe) {
		return err
	}
	var fx *FallbackExhaustedError
	if errors.As(err, &fx) {
		return err
	}
	if classified := ClassifyError(err, provider, model); classified != nil {
		return classified
	}
	return err
}

// DominantReason returns the most user-actionable reason among the recorded
// attempts, along with the provider and underlying error that produced it.
// Attempt.Reason is only populated for a subset of cases, so each attempt's
// error is (re)classified to derive a reason when one is missing.
func (e *FallbackExhaustedError) DominantReason() (FailoverReason, string, error) {
	best := FailoverUnknown
	bestProvider := ""
	var bestErr error
	for _, a := range e.Attempts {
		if a.Skipped {
			continue
		}
		reason := a.Reason
		if reason == "" {
			if classified := ClassifyError(a.Error, a.Provider, a.Model); classified != nil {
				reason = classified.Reason
			} else {
				reason = FailoverUnknown
			}
		}
		if bestErr == nil || reasonPriority[reason] >= reasonPriority[best] {
			best = reason
			bestProvider = a.Provider
			bestErr = a.Error
		}
	}
	return best, bestProvider, bestErr
}

// FriendlyError renders a provider error into a short, user-facing message.
// It unwraps failover classification metadata (from either the single-candidate
// or fallback path) and maps the typed reason to a friendly template, falling
// back to the raw error text only when the reason is genuinely unknown.
func FriendlyError(err error) string {
	if err == nil {
		return ""
	}

	var fe *FailoverError
	if errors.As(err, &fe) {
		return friendlyForReason(fe.Reason, fe.Provider, fe.RetryAfter, fe.Wrapped)
	}

	var fx *FallbackExhaustedError
	if errors.As(err, &fx) {
		reason, provider, wrapped := fx.DominantReason()
		return friendlyForReason(reason, provider, retryAfterFromError(wrapped), wrapped)
	}

	// Not pre-classified: try a direct classification before giving up.
	if classified := ClassifyError(err, "", ""); classified != nil {
		return friendlyForReason(classified.Reason, "", classified.RetryAfter, err)
	}

	return fmt.Sprintf("⚠️ Something went wrong: %v", err)
}

// friendlyForReason maps a failover reason to a user-facing message. provider,
// retryAfter, and cause are best-effort context; any may be empty/zero/nil.
func friendlyForReason(reason FailoverReason, provider string, retryAfter time.Duration, cause error) string {
	on := ""
	if provider != "" {
		on = " on " + provider
	}
	resetHint := humanizeDuration(retryAfter)

	switch reason {
	case FailoverRateLimit, FailoverOverloaded:
		msg := fmt.Sprintf("⚠️ Rate limit reached%s. Please try again later.", on)
		if resetHint != "" {
			msg = fmt.Sprintf("⚠️ Rate limit reached%s. Try again in %s.", on, resetHint)
		}
		return msg
	case FailoverBilling:
		return fmt.Sprintf("💳 Billing issue%s — out of credits or plan limit reached. Please check your plan & billing.", on)
	case FailoverAuth:
		return fmt.Sprintf("🔑 Authentication failed%s. Please re-authenticate.", on)
	case FailoverContextOverflow:
		return "📏 This conversation is too long for the model. Start a new chat or shorten your message."
	case FailoverTimeout:
		return fmt.Sprintf("⏳ The request timed out%s. Please try again.", on)
	case FailoverNetwork:
		return fmt.Sprintf("🌐 Network error reaching the provider%s. Please try again.", on)
	case FailoverFormat:
		return "⚠️ The request was rejected as malformed. This is likely a bug — please report it."
	default:
		if cause != nil {
			return fmt.Sprintf("⚠️ Request failed%s: %v", on, cause)
		}
		return fmt.Sprintf("⚠️ Request failed%s.", on)
	}
}

// retryAfterFromError extracts an embedded retry/reset wait from an error
// message, returning 0 when none is present. Used for the fallback path where
// the dominant attempt's error is not itself a *FailoverError.
func retryAfterFromError(err error) time.Duration {
	if err == nil {
		return 0
	}
	return parseRetryAfter(strings.ToLower(err.Error()))
}
