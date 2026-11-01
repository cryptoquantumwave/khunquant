package sandboxlab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/scenario"
)

var errBadSource = errors.New("give exactly one of file and inline")

func (m *mcpServer) registerScenarioTools() {
	m.srv.AddTool(&mcp.Tool{
		Name: "sandbox_scenario",
		Description: "Check scenario files (schema v1). ops: validate (file | inline YAML, instance?) returns the issues, each with a " +
			"JSON pointer; with an instance running, features it lacks are reported with the PR that adds them.",
		InputSchema: json.RawMessage(`{"type":"object","required":["op"],"additionalProperties":false,"properties":{
			"op":{"type":"string","enum":["validate"]},
			"file":{"type":"string","description":"path of a scenario file"},
			"inline":{"type":"string","description":"scenario YAML"},
			"instance":` + instanceProp + `}}`),
	}, m.scenarioHandler)
}

type scenarioArgs struct {
	Op       string `json:"op"`
	File     string `json:"file"`
	Inline   string `json:"inline"`
	Instance string `json:"instance"`
}

func (m *mcpServer) scenarioHandler(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a scenarioArgs
	dec := json.NewDecoder(bytes.NewReader(req.Params.Arguments))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		return errResult("sandbox_scenario: %v", err), nil
	}
	if a.Op != "validate" {
		return errResult("sandbox_scenario: op must be validate (got %q)", a.Op), nil
	}
	return m.validateOp(ctx, a)
}

// scenarioSource is the YAML of a call: a file or inline text, exactly one.
func scenarioSource(a scenarioArgs) (name string, data []byte, err error) {
	if (a.File == "") == (a.Inline == "") {
		return "", nil, errBadSource
	}
	if a.Inline != "" {
		return "inline", []byte(a.Inline), nil
	}
	data, err = os.ReadFile(a.File)
	return a.File, data, err
}

// capsOf is what the instance offers, or nil (static checks only) when there is none.
func (m *mcpServer) capsOf(ctx context.Context, instance string) *scenario.Caps {
	c, err := m.sup.Client(instance)
	if err != nil {
		return nil
	}
	info, err := c.Lab(ctx)
	if err != nil {
		return nil
	}
	caps := &scenario.Caps{Groups: map[string][]string{}, Kinds: info.Kinds}
	for _, g := range info.Groups {
		ops := []string{}
		for _, o := range g.Ops {
			ops = append(ops, o.Name)
		}
		caps.Groups[g.Name] = ops
	}
	return caps
}

func (m *mcpServer) validateOp(ctx context.Context, a scenarioArgs) (*mcp.CallToolResult, error) {
	name, data, err := scenarioSource(a)
	if err != nil {
		return errResult("sandbox_scenario validate: %v", err), nil
	}
	caps := m.capsOf(ctx, a.Instance)
	c, _ := checkScenario(name, data, caps)
	return jsonResult(map[string]any{
		"valid": c.ok(), "static_only": caps == nil, "scenario": c.Scenario, "issues": c.Issues, "error": c.Error,
	}), nil
}
