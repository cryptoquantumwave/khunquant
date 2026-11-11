//go:build !windows

package sandboxlab

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestPrintConfig_PointsAtRunningBinary(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	cmd := newMCPCommand()
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--print-config"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var got struct {
		MCPServers map[string]struct {
			Command string
			Args    []string
			Env     map[string]string
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("--print-config is not JSON: %v\n%s", err, out.String())
	}
	s, ok := got.MCPServers[mcpServerName]
	if !ok || len(got.MCPServers) != 1 {
		t.Fatalf("servers = %v, want only %s", got.MCPServers, mcpServerName)
	}
	if s.Command != exe || !slices.Equal(s.Args, []string{"sandbox", "mcp"}) || len(s.Env) != 0 {
		t.Errorf("entry = %+v, want command %s, args [sandbox mcp], no env", s, exe)
	}
}

// .mcp.json starts only the supervisor (which locks every instance itself:
// supervisor.TestSupervisor_AlwaysLocks), never a gateway, carries no credential, and is
// what --print-config prints for ./build/khunquant.
func TestMCPJSON_IsValid(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), ".mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	want, err := mcpConfig("./build/khunquant")
	if err != nil {
		t.Fatal(err)
	}
	var a, b any
	if err := json.Unmarshal(data, &a); err != nil {
		t.Fatalf(".mcp.json: %v", err)
	}
	_ = json.Unmarshal(want, &b)
	if !jsonEqual(a, b) {
		t.Errorf(".mcp.json differs from --print-config for ./build/khunquant:\n%s\nwant:\n%s", data, want)
	}
	servers, _ := a.(map[string]any)["mcpServers"].(map[string]any)
	entry, _ := servers[mcpServerName].(map[string]any)
	if len(servers) != 1 || entry == nil {
		t.Fatalf("servers = %v, want only %s", servers, mcpServerName)
	}
	args, _ := json.Marshal(entry["args"])
	if string(args) != `["sandbox","mcp"]` || strings.Contains(string(data), "gateway") {
		t.Errorf("args = %s: the entry must run `sandbox mcp` and never a gateway", args)
	}
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, c := range x {
				for _, bad := range []string{"token", "key", "secret", "password"} {
					if strings.Contains(strings.ToLower(k), bad) {
						t.Errorf(".mcp.json has a %q field", k)
					}
				}
				walk(c)
			}
		case []any:
			for _, c := range x {
				walk(c)
			}
		}
	}
	walk(a)
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

var (
	toolMentionRE = regexp.MustCompile(`\bsandbox_[a-z_]+`)
	toolOpRE      = regexp.MustCompile("`(sandbox_[a-z_]+) op=([a-z_]+)`")
	opMentionRE   = regexp.MustCompile(`\bop=[a-z_]+`)
)

// Every sandbox_* tool the agent docs name is registered by the supervisor's MCP server,
// and every `sandbox_x op=y` names an op that tool's schema lists. The ops come from the
// static schemas (groupTable, the sandbox_instance and sandbox_scenario enums), so an op
// that only a running instance reports is not checked here.
func TestClaudeMDToolNamesExist(t *testing.T) {
	h := newHarness(t)
	res, err := h.cs.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	ops := map[string][]string{}
	for _, tool := range res.Tools {
		schema, _ := tool.InputSchema.(map[string]any)
		props, _ := schema["properties"].(map[string]any)
		op, _ := props["op"].(map[string]any)
		enum, _ := op["enum"].([]any)
		ops[tool.Name] = nil
		for _, e := range enum {
			ops[tool.Name] = append(ops[tool.Name], e.(string))
		}
	}
	if len(ops) == 0 {
		t.Fatal("the supervisor registered no tools")
	}
	root := repoRoot(t)
	mentions := 0
	for _, rel := range []string{"CLAUDE.md", "testdata/scenarios/README.md"} {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range toolMentionRE.FindAllString(string(data), -1) {
			mentions++
			if _, ok := ops[name]; !ok {
				t.Errorf("%s names %s, which the supervisor does not register", rel, name)
			}
		}
		qualified := toolOpRE.FindAllStringSubmatch(string(data), -1)
		for _, m := range qualified {
			if !slices.Contains(ops[m[1]], m[2]) {
				t.Errorf("%s names `%s op=%s`; %s has ops %v", rel, m[1], m[2], m[1], ops[m[1]])
			}
		}
		// A bare `op=x` would not be checked: name the tool each time.
		if n := len(opMentionRE.FindAllString(string(data), -1)); n != len(qualified) {
			t.Errorf("%s has %d `op=` mentions but %d are written as `sandbox_tool op=name`", rel, n, len(qualified))
		}
	}
	if mentions == 0 {
		t.Fatal("no sandbox_ tool is named in CLAUDE.md or the scenarios README")
	}
}
