package gateway

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labjournal"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

// maxToolCallArgs caps the arguments a tool.call entry keeps.
const maxToolCallArgs = 4 << 10

// labToolTap journals every executed tool call as a "tool.call" entry. It only
// watches: BeforeTool always allows, and nothing in AfterTool, a panicking journal
// included, can reach the result the model sees. Calls refused before the hooks run
// (class ceiling, approval hold) never reach it; they show up as policy.decision.
type labToolTap struct {
	j interface{ Append(e labjournal.Entry) }
}

func (labToolTap) BeforeTool(context.Context, string, map[string]any) (bool, *tools.ToolResult) {
	return true, nil
}

func (t labToolTap) AfterTool(ctx context.Context, name string, args map[string]any, res *tools.ToolResult) {
	defer func() {
		if p := recover(); p != nil {
			logger.WarnCF("labjournal", "The tool-call tap failed", map[string]any{"tool": name, "panic": fmt.Sprint(p)})
		}
	}()
	p := map[string]any{
		"name": name, "origin": tools.ToolOrigin(ctx), "sender": tools.ToolSender(ctx),
		"turn_kind": string(tools.ProactiveTurnKind(ctx)), "is_error": res != nil && res.IsError,
	}
	switch b, err := json.Marshal(args); {
	case err != nil:
		p["args_error"] = err.Error()
	case len(b) > maxToolCallArgs:
		p["args"], p["args_truncated"] = string(b[:maxToolCallArgs]), true
	default:
		p["args"] = json.RawMessage(b)
	}
	payload, _ := json.Marshal(p)
	t.j.Append(labjournal.Entry{Kind: "tool.call", Source: "tools", Subject: name, Payload: payload})
}
