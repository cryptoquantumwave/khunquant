package sim

import (
	"context"

	"github.com/cryptoquantumwave/khunquant/pkg/providers/protocoltypes"
)

// classify says what kind of call this is.
func (e *Engine) classify(ctx context.Context, tools []protocoltypes.ToolDefinition, _ map[string]any) CallInfo {
	var ci CallInfo
	if e.o.TurnInfo != nil {
		ci = e.o.TurnInfo(ctx)
	}
	if ci.Turn == "" {
		ci.Turn = "unknown"
	}
	ci.Tools = ci.Tools[:0:0]
	for _, t := range tools {
		ci.Tools = append(ci.Tools, t.Function.Name)
	}
	return ci
}
