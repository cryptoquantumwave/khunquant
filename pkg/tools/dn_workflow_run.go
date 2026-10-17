package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// runDNWorkflow starts the workflow of an unwind or a resize, waits up to dnOpenWait
// and decodes the output of its finalize step into out (a *UnwindResult or
// *ResizeResult). It returns a result to hand back at once when there is nothing to
// decode: no engine (nothing was started, nothing placed), a start that failed, a
// workflow that outlasted the wait (Deps.Tell tells the owner how it ended), or one
// that ended without a result.
func runDNWorkflow(ctx context.Context, kind string, input any, what, verb string, out any) *ToolResult {
	w := currentDNWorkflows()
	if w == nil {
		hint := ""
		if kind == deltaneutral.KindUnwind {
			hint = " If you must get out now, close the legs by hand with futures_close_position and create_order."
		}
		return ErrorResult(fmt.Sprintf("the workflow engine is not running, so a two-leg %s cannot be made durable. "+
			"Refusing to %s: a crash between the legs could not be resumed. Nothing was placed.%s", verb, verb, hint))
	}
	id, err := w.Start(ctx, kind, input)
	if err != nil {
		return ErrorResult(fmt.Sprintf("could not start the %s workflow: %v. Nothing was placed.", verb, err))
	}
	wctx, cancel := context.WithTimeout(ctx, dnOpenWait)
	defer cancel()
	wf, err := w.Wait(wctx, id)
	if err != nil {
		return UserResult(fmt.Sprintf("%s is in progress (workflow %s); you will get a message when it finishes.", what, id))
	}
	steps, err := w.Steps(ctx, id)
	var probe struct {
		Result string `json:"result"`
	}
	for _, s := range steps {
		if s.Name == "finalize" && len(s.Output) > 0 {
			_ = json.Unmarshal(s.Output, &probe)
			_ = json.Unmarshal(s.Output, out)
		}
	}
	if err != nil || probe.Result == "" {
		return ErrorResult(fmt.Sprintf(
			"The %s workflow %s ended %s without a result%s. Check the plan's executions and the exchange positions before doing anything else; "+
				"the plan may be recovery_required.", verb, id, wf.Status, mapJoin(": ", wf.Error)))
	}
	return nil
}

// refusedResult is a workflow that refused before it placed anything.
func refusedResult(msg string) *ToolResult {
	out := ErrorResult(msg)
	if strings.Contains(msg, broker.ErrRateLimited.Error()) {
		out = out.WithError(broker.ErrRateLimited)
	}
	return out
}
