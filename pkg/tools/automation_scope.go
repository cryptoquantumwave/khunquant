package tools

import (
	"context"
	"fmt"
	"strings"
)

var ctxKeyAutomationScope = &toolCtxKey{"automation_scope"}

// AutomationScope says which scheduled job an autonomous turn is running for.
//
// It is what lets one tool, execute_dca_order, run in a scheduled turn without
// opening the rest of the trade class: the exception holds only inside a job
// whose name starts with "dca:", and only for the plan that job was created for.
type AutomationScope struct {
	// JobName is the scheduled job's name, e.g. "dca:7:weekly btc".
	JobName string
	// DCAPlanID is the plan a "dca:" job belongs to, parsed by the gateway's DCA
	// handler. Zero for every other job.
	DCAPlanID int64
}

// WithAutomationScope tags the turn with the job it runs for. Set-once, like
// WithToolOrigin: the DCA handler tags first with the plan ID, and the generic
// tag ExecuteJob applies afterwards must not replace it - and a sub-turn must
// not be able to widen or relabel its parent's scope.
func WithAutomationScope(ctx context.Context, scope AutomationScope) context.Context {
	if scope.JobName == "" {
		return ctx
	}
	if _, ok := AutomationScopeFrom(ctx); ok {
		return ctx
	}
	return context.WithValue(ctx, ctxKeyAutomationScope, scope)
}

// AutomationScopeFrom returns the turn's job scope, if it has one.
func AutomationScopeFrom(ctx context.Context) (AutomationScope, bool) {
	s, ok := ctx.Value(ctxKeyAutomationScope).(AutomationScope)
	return s, ok
}

// isDCAJobScope reports whether the turn runs for a "dca:" job with a known plan.
func isDCAJobScope(ctx context.Context) bool {
	s, ok := AutomationScopeFrom(ctx)
	return ok && strings.HasPrefix(s.JobName, "dca:") && s.DCAPlanID != 0
}

// dcaScopeRefusal is execute_dca_order's own check, behind the policy gate. In an
// autonomous turn the call must name the plan the job belongs to; any other
// plan_id is refused, so a scheduled DCA job cannot be steered at a different
// plan. A turn a person started is not affected.
func dcaScopeRefusal(ctx context.Context, planID int64) *ToolResult {
	if !IsAutonomousOrigin(ToolOrigin(ctx)) {
		return nil
	}
	if s, ok := AutomationScopeFrom(ctx); ok && isDCAJobScope(ctx) && s.DCAPlanID == planID {
		return nil
	}
	return policyRefusal(NameExecuteDCAOrder, fmt.Sprintf(
		"%s may only execute the plan its own scheduled DCA job belongs to; plan %d is not that plan.",
		NameExecuteDCAOrder, planID))
}
