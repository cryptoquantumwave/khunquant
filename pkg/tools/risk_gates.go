package tools

import (
	"fmt"

	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// allowDNLegs takes the rate-limit tokens for BOTH legs of a delta-neutral tool up front.
//
// It must be called after the dry-run / confirmation return (a preview places no order,
// so it must not spend the budget) and before the first order of the first leg. Taking
// the second token between the legs would let a missing token strand a half-open
// position; taking both here means a refusal happens while nothing has been placed.
// A same-exchange plan draws both tokens from one bucket.
//
// Returns nil when both tokens were granted, otherwise the refusal to return.
func allowDNLegs(futuresProvider, spotProvider string) *ToolResult {
	if !broker.DefaultLimiter.Allow(futuresProvider) {
		return ErrorResult(fmt.Sprintf("rate limit exceeded for futures provider %q", futuresProvider)).
			WithError(broker.ErrRateLimited)
	}
	if !broker.DefaultLimiter.Allow(spotProvider) {
		return ErrorResult(fmt.Sprintf("rate limit exceeded for spot provider %q", spotProvider)).
			WithError(broker.ErrRateLimited)
	}
	return nil
}
