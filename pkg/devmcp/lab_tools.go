package devmcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab"
)

// registerLabTools registers one tool sandbox_<group> per group of the lab registry,
// the in-instance twin of POST /api/sandbox/lab/<group>. It registers nothing unless
// the process is locked and d carries the lab.
func registerLabTools(s *mcp.Server, d Deps) {
	if d.Lab == nil || !sandbox.Locked() {
		return
	}
	for _, g := range d.Lab.Groups() {
		s.AddTool(&mcp.Tool{Name: "sandbox_" + g.Name, Description: g.Description, InputSchema: labSchema(g)},
			labHandler(d.Lab, g.Name))
	}
}

// labSchema is the union of the ops' argument properties plus an "op" enum whose
// description lists the ops.
func labSchema(g lab.Group) json.RawMessage {
	props := map[string]json.RawMessage{}
	var ops, doc []string
	for _, o := range g.Ops {
		if o.Name != "" {
			ops = append(ops, o.Name)
			doc = append(doc, o.Name+": "+o.Description)
		}
		var p map[string]json.RawMessage
		_ = json.Unmarshal(o.Args, &p)
		for k, v := range p {
			if _, dup := props[k]; !dup {
				props[k] = v
			}
		}
	}
	schema := map[string]any{"type": "object", "properties": props}
	if len(ops) > 0 {
		props["op"], _ = json.Marshal(map[string]any{"type": "string", "enum": ops, "description": strings.Join(doc, "; ")})
		schema["required"] = []string{"op"}
	}
	b, _ := json.Marshal(schema)
	return b
}

func labHandler(l *lab.Lab, group string) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.Params.Arguments
		out, err := l.Call(ctx, group, lab.OpOf(args), args)
		if err != nil {
			out = map[string]string{"error": err.Error()} // "<code>: <message>"
		}
		b, merr := json.Marshal(out)
		if merr != nil {
			b, err = []byte(fmt.Sprintf(`{"error":%q}`, merr.Error())), merr
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}, IsError: err != nil}, nil
	}
}
