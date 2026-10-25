package agent

import (
	"context"
	"sync"

	"github.com/cryptoquantumwave/khunquant/pkg/providers"
)

// usageAgg totals the provider usage of one turn: its own LLM calls and those
// of the sub-turns that run inside it. It rides on the context, so a sub-turn
// started with the turn's context adds to the same total without a parameter.
type usageAgg struct {
	mu     sync.Mutex
	usage  providers.UsageInfo
	closed bool
}

type usageAggKey struct{}

func withUsageAgg(ctx context.Context, a *usageAgg) context.Context {
	return context.WithValue(ctx, usageAggKey{}, a)
}

// carryUsageAgg makes dst, a context that does not derive from src, add its LLM
// usage to the turn src belongs to.
func carryUsageAgg(dst, src context.Context) context.Context {
	if a, _ := src.Value(usageAggKey{}).(*usageAgg); a != nil {
		return withUsageAgg(dst, a)
	}
	return dst
}

// addTurnUsage adds u to the aggregate carried by ctx, if any. A nil u, which
// providers return when they report no usage, adds nothing.
func addTurnUsage(ctx context.Context, u *providers.UsageInfo) {
	a, _ := ctx.Value(usageAggKey{}).(*usageAgg)
	a.add(u)
}

func (a *usageAgg) add(u *providers.UsageInfo) {
	if a == nil || u == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return // a sub-turn that outlived its parent's report
	}
	a.usage.PromptTokens += u.PromptTokens
	a.usage.CompletionTokens += u.CompletionTokens
	a.usage.TotalTokens += u.TotalTokens
}

// close stops the aggregate accepting usage and returns its total.
func (a *usageAgg) close() providers.UsageInfo {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.closed = true
	return a.usage
}

// reportUsage hands the sink, once, the usage of the turn runAgentLoop is
// about to finish, and passes it up to an enclosing aggregate (a sub-turn that
// was given a sink of its own). It returns ctx carrying a fresh aggregate, and
// the function to defer. With a nil sink it returns ctx and a no-op, which is
// today's behaviour exactly.
func reportUsage(ctx context.Context, sink func(providers.UsageInfo)) (context.Context, func()) {
	if sink == nil {
		return ctx, func() {}
	}
	parent, _ := ctx.Value(usageAggKey{}).(*usageAgg)
	a := &usageAgg{}
	return withUsageAgg(ctx, a), func() {
		total := a.close()
		parent.add(&total)
		sink(total)
	}
}
