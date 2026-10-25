package tools

import (
	"context"
	"fmt"

	"github.com/cryptoquantumwave/khunquant/pkg/logger"
)

// Hook sees every tool call a registry runs, MCP tools and tools discovered at
// runtime included, because it runs inside ExecuteWithContext and not in any tool.
//
// BeforeTool runs after today's checks (the argument schema, the class ceiling, the
// per-tool rows and the approval hold) have allowed the call. Returning allow=false
// refuses it: override, when not nil, is the result the model sees, else a generic
// refusal is. AfterTool runs after the tool, with the result it returned.
//
// Hooks run in registration order before the tool, and the first refusal wins (the
// later hooks and the tool do not run). AfterTool runs in reverse order, and only for
// a call that ran. A hook that panics is recovered; in BeforeTool that refuses the call
// (fail closed) and logs one ERROR.
type Hook interface {
	BeforeTool(ctx context.Context, name string, args map[string]any) (allow bool, override *ToolResult)
	AfterTool(ctx context.Context, name string, args map[string]any, res *ToolResult)
}

// AddHook appends h to the registry's hooks. A clone (Clone, CloneFiltered) carries
// the hooks the registry has at that moment, so a subagent's calls are hooked too.
func (r *ToolRegistry) AddHook(h Hook) {
	if h == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hooks = append(r.hooks, h)
}

// hookChain is the hooks that apply to a call now: the armed policy hook, which is
// package state so that a clone, a registry built on reload and a registry nobody
// wired cannot miss it, then the registry's own, in registration order.
func (r *ToolRegistry) hookChain() []Hook {
	r.mu.RLock()
	own := r.hooks
	r.mu.RUnlock()
	if pe := livePolicy.Load(); pe != nil {
		chain := make([]Hook, 0, len(own)+1)
		return append(append(chain, pe), own...)
	}
	return own
}

// runBeforeHooks returns nil to let the call proceed, else the refusal to return.
func runBeforeHooks(ctx context.Context, hooks []Hook, name string, args map[string]any) *ToolResult {
	for _, h := range hooks {
		if refusal := beforeOne(ctx, h, name, args); refusal != nil {
			return refusal
		}
	}
	return nil
}

func beforeOne(ctx context.Context, h Hook, name string, args map[string]any) (refusal *ToolResult) {
	defer func() {
		if p := recover(); p != nil {
			logger.ErrorCF("tool", "A tool hook panicked; the call is refused",
				map[string]any{"tool": name, "hook": fmt.Sprintf("%T", h), "panic": fmt.Sprint(p)})
			refusal = policyRefusal(name, "A safety hook failed while checking this call, so it was not run. Nothing was done.")
		}
	}()
	allow, override := h.BeforeTool(ctx, name, args)
	if allow {
		return nil
	}
	if override == nil {
		return policyRefusal(name, "A safety hook refused this call. Nothing was done.")
	}
	return override
}

func runAfterHooks(ctx context.Context, hooks []Hook, name string, args map[string]any, res *ToolResult) {
	for i := len(hooks) - 1; i >= 0; i-- {
		afterOne(ctx, hooks[i], name, args, res)
	}
}

func afterOne(ctx context.Context, h Hook, name string, args map[string]any, res *ToolResult) {
	defer func() {
		if p := recover(); p != nil {
			logger.ErrorCF("tool", "A tool hook panicked after the call; the result is unchanged",
				map[string]any{"tool": name, "hook": fmt.Sprintf("%T", h), "panic": fmt.Sprint(p)})
		}
	}()
	h.AfterTool(ctx, name, args, res)
}
