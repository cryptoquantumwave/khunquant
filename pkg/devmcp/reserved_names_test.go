package devmcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab"
)

// The reserved list in pkg/sandbox/lab is a copy (lab cannot import devmcp); this keeps
// it complete: no existing dev-MCP tool name, bare or as sandbox_<group>, is a legal
// group name, so a lab tool can never shadow one.
func TestReservedToolNamesCoverEveryDevMCPTool(t *testing.T) {
	tools := toolsOf(t, connect(t, Deps{Cfg: &config.Config{}}))
	if len(tools) < 15 {
		t.Fatalf("only %d dev-MCP tools listed", len(tools))
	}
	noop := func(context.Context, json.RawMessage) (any, error) { return nil, nil }
	for name := range tools {
		group := strings.TrimPrefix(name, "sandbox_") // the group whose tool would be this one
		for _, n := range []string{name, group} {
			if err := (&lab.Lab{}).AddGroup(lab.Group{Name: n, Ops: []lab.Op{{Run: noop}}}); err == nil {
				t.Errorf("group name %q (dev-MCP tool %q) was accepted", n, name)
			}
		}
	}
}
