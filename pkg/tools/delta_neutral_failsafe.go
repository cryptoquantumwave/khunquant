package tools

import (
	"context"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral"
)

// bookkeepingTimeout bounds post-fill persistence calls. They run on a context
// detached from the caller's cancellation so a cancelled request cannot turn a filled
// order into an orphan.
//
// (The failure paths of the delta-neutral legs themselves, a fill whose row cannot be
// saved and so on, live in pkg/deltaneutral's workflows: retried, traced on the
// execution and ended in recovery_required.)
const bookkeepingTimeout = 15 * time.Second

// saveExecutionLegFn is the single seam through which a workflow's leg rows are
// persisted (Deps.SaveLeg is pointed at it by the tests). Production code never
// replaces it; tests swap it to simulate a failing store.
var saveExecutionLegFn = func(ctx context.Context, s *deltaneutral.Store, leg *deltaneutral.ExecutionLeg) (int64, error) {
	return s.SaveExecutionLeg(ctx, leg)
}

// bookkeepingContext returns a context for post-fill persistence that survives
// cancellation of the request context.
func bookkeepingContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), bookkeepingTimeout)
}
