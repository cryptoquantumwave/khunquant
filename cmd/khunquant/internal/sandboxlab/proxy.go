package sandboxlab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labclient"
)

// groupSpec is a lab group the supervisor proxies before any instance exists: its ops
// (README 5.3) and the PR that adds it, for the "not in this binary" message.
type groupSpec struct {
	name, pr string
	ops      []string
}

var groupTable = []groupSpec{
	{"clock", "SBX-07", []string{"get", "advance", "set_mode"}},
	{"market", "SBX-03", []string{"get", "set", "path", "shock", "walk", "replay", "recording", "delist", "relist"}},
	{"account", "SBX-06", []string{"seed", "get", "orders"}},
	{"fault", "SBX-13", []string{"add", "list", "clear"}},
	{"chat", "SBX-02", []string{"send", "press", "react", "outbox", "set_state"}},
	{"llm", "SBX-04", []string{"mode", "script_put", "script_list", "cassette_load", "budget"}},
	{"web", "SBX-15", []string{"put", "list", "requests"}},
	{"call_tool", "SBX-06", nil},
	{"manage", "SBX-06", []string{"call"}},
	{"inspect", "SBX-05", []string{"journal", "decisions", "triggers", "world", "outbox", "workflows", "proposals", "policy", "settle", "wait_for", "idle", "export_replay"}},
	{"snapshot", "SBX-17", []string{"take"}},
	{"crash", "SBX-20", []string{"arm", "disarm", "list"}},
}

// devTools are the 15 dev-MCP tools of an instance, proxied under their own names.
var devTools = []struct{ name, desc string }{
	{"service_status", "Gateway status of the instance: uptime, generation, channels, agents."},
	{"list_tools", "Tools registered on the instance's agents."},
	{"list_llm_calls", "Recorded LLM calls of the instance."},
	{"read_llm_call", "One recorded LLM call in full."},
	{"list_sessions", "Agent sessions of the instance."},
	{"read_session_history", "The message history of one session."},
	{"search_sessions", "Search the sessions' messages."},
	{"read_logs", "The instance's recent log lines."},
	{"read_config", "The instance's config, secrets redacted."},
	{"sandbox_list_venues", "Venues and symbols the instance's exchange sandbox serves."},
	{"sandbox_read_fixtures", "Read a venue's fixtures."},
	{"sandbox_write_fixtures", "Replace a venue's fixtures."},
	{"sandbox_upsert_fixture", "Add or replace one fixture entry."},
	{"sandbox_reload_fixtures", "Reload the fixtures from disk."},
	{"sandbox_reset_simulator", "Reset the stateful simulator (orders, balances)."},
}

const instanceProp = `{"type":"string","description":"Instance name; optional while exactly one instance runs"}`

// staticGroupSchema is what a client sees before the first `up`.
func staticGroupSchema(g groupSpec) json.RawMessage {
	props := map[string]any{"instance": json.RawMessage(instanceProp)}
	schema := map[string]any{"type": "object", "properties": props, "additionalProperties": true}
	if len(g.ops) > 0 {
		props["op"] = map[string]any{"type": "string", "enum": g.ops}
		schema["required"] = []string{"op"}
	}
	b, _ := json.Marshal(schema)
	return b
}

// groupSchema is the schema of a group as the instance reports it: the union of its
// ops' argument properties, an "op" enum whose description lists the ops, and instance.
func groupSchema(g labclient.GroupInfo) json.RawMessage {
	props := map[string]json.RawMessage{"instance": json.RawMessage(instanceProp)}
	var ops, doc []string
	for _, o := range g.Ops {
		if o.Name != "" {
			ops = append(ops, o.Name)
			d := o.Name + ": " + o.Description
			if len(o.Required) > 0 {
				d += " (requires " + strings.Join(o.Required, ", ") + ")"
			}
			doc = append(doc, d)
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

// withInstance adds the instance argument to a dev-MCP tool's own input schema.
func withInstance(schema any) json.RawMessage {
	var m map[string]any
	if b, err := json.Marshal(schema); err != nil || json.Unmarshal(b, &m) != nil || m == nil {
		m = map[string]any{"type": "object"}
	}
	props, _ := m["properties"].(map[string]any)
	if props == nil {
		props = map[string]any{}
	}
	props["instance"] = json.RawMessage(instanceProp)
	m["properties"] = props
	b, _ := json.Marshal(m)
	return b
}

// splitInstance takes the supervisor-only "instance" argument out of a tool call and
// returns the rest, which is what the instance receives. Numbers keep their digits.
func splitInstance(raw json.RawMessage) (instance string, rest map[string]any, err error) {
	rest = map[string]any{}
	if len(bytes.TrimSpace(raw)) > 0 {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(&rest); err != nil {
			return "", nil, fmt.Errorf("arguments are not a JSON object: %w", err)
		}
	}
	if v, ok := rest["instance"]; ok {
		if instance, ok = v.(string); !ok {
			return "", nil, errors.New("instance must be a string")
		}
		delete(rest, "instance")
	}
	return instance, rest, nil
}

// groupHandler forwards a call of sandbox_<group> to the instance's lab route.
func (m *mcpServer) groupHandler(group string) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		instance, body, err := splitInstance(req.Params.Arguments)
		if err != nil {
			return errResult("%v", err), nil
		}
		c, err := m.sup.Client(instance)
		if err != nil {
			return errResult("%v", err), nil
		}
		raw, _ := json.Marshal(body)
		out, err := c.Call(ctx, group, raw)
		var le *labclient.Error
		switch {
		case errors.As(err, &le) && le.Code == lab.CodeUnknownGroup:
			return errResult("group %q is not in this binary (added by %s)", group, prOf(group)), nil
		case err != nil:
			return errResult("%v", err), nil
		}
		return textResult(string(out)), nil
	}
}

func prOf(group string) string {
	for _, g := range groupTable {
		if g.name == group {
			return g.pr
		}
	}
	return "a later SBX PR"
}

// devHandler forwards a call of a dev-MCP tool over an MCP session to the instance.
func (m *mcpServer) devHandler(tool string) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		instance, args, err := splitInstance(req.Params.Arguments)
		if err != nil {
			return errResult("%v", err), nil
		}
		c, err := m.sup.Client(instance)
		if err != nil {
			return errResult("%v", err), nil
		}
		res, err := c.CallDevTool(ctx, tool, args)
		if err != nil {
			return errResult("%v", err), nil
		}
		return res, nil
	}
}

// registerProxies adds the static tool of every group and dev-MCP tool.
func (m *mcpServer) registerProxies() {
	for _, g := range groupTable {
		desc := fmt.Sprintf("Lab group %q of a sandbox instance (added by %s). Start an instance with sandbox_instance first.", g.name, g.pr)
		m.srv.AddTool(&mcp.Tool{Name: "sandbox_" + g.name, Description: desc, InputSchema: staticGroupSchema(g)}, m.groupHandler(g.name))
	}
	for _, d := range devTools {
		m.srv.AddTool(&mcp.Tool{Name: d.name, Description: d.desc + " Proxied to the instance's dev-MCP server.",
			InputSchema: withInstance(nil)}, m.devHandler(d.name))
	}
}

// refine replaces the static schemas with the instance's own: its groups (a group the
// table does not know is added) and its dev-MCP tools. The SDK sends tools/list_changed
// for each. The supervisor and the instance are one binary, so the schemas cannot drift.
// A failure leaves the static schemas, which the instance validates against anyway.
func (m *mcpServer) refine(ctx context.Context, instance string) {
	c, err := m.sup.Client(instance)
	if err != nil {
		return
	}
	if groups, err := c.Groups(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "sandbox mcp: could not refine the group tools: %v\n", err)
	} else {
		for _, g := range groups {
			m.srv.AddTool(&mcp.Tool{Name: "sandbox_" + g.Name, Description: g.Description, InputSchema: groupSchema(g)}, m.groupHandler(g.Name))
		}
	}
	tools, err := c.DevTools(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sandbox mcp: could not refine the dev-MCP tools: %v\n", err)
		return
	}
	for _, t := range tools {
		for _, d := range devTools {
			if d.name == t.Name {
				m.srv.AddTool(&mcp.Tool{Name: t.Name, Description: t.Description, InputSchema: withInstance(t.InputSchema)}, m.devHandler(t.Name))
			}
		}
	}
}
