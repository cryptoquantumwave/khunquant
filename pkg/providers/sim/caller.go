package sim

import (
	"context"

	"github.com/cryptoquantumwave/khunquant/pkg/providers/protocoltypes"
)

// The Chat options that tell a providers.Complete call from an agent turn. They are
// literals here because this package cannot import pkg/providers; pkg/providers and
// pkg/agent have tests that pin each against the code that sets it.
const (
	OptionCacheKey      = "prompt_cache_key" // agent turns send the agent id, the seahorse summarizer "seahorse"
	OptionNoNativeTools = "no_native_tools"  // providers.OptionNoNativeTools, set by CompleteLight (the news parser)
	CacheKeySeahorse    = "seahorse"
)

// classify says what kind of call this is. A providers.Complete call has no tools and
// is told apart by its options: the seahorse summarizer by its cache key, the news
// parser (CompleteLight) by no_native_tools, any other tool-less call without a cache
// key as a plain completion. Everything else is an agent turn, which the gateway's
// TurnInfo describes from the turn's context.
func (e *Engine) classify(ctx context.Context, tools []protocoltypes.ToolDefinition, options map[string]any) CallInfo {
	key, _ := options[OptionCacheKey].(string)
	noNative, _ := options[OptionNoNativeTools].(bool)

	var ci CallInfo
	switch {
	case key == CacheKeySeahorse:
		ci = CallInfo{Turn: turnComplete, Caller: callerSeahorse}
	case noNative:
		ci = CallInfo{Turn: turnComplete, Caller: callerNews}
	case len(tools) == 0 && key == "":
		ci = CallInfo{Turn: turnComplete, Caller: callerComplete}
	case e.o.TurnInfo != nil:
		ci = e.o.TurnInfo(ctx)
	}
	if ci.Turn == "" {
		ci.Turn = "unknown"
	}
	ci.Tools = make([]string, 0, len(tools))
	for _, t := range tools {
		ci.Tools = append(ci.Tools, t.Function.Name)
	}
	return ci
}
