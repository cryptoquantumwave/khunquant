package devmcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab"
)

// The reserved list in pkg/sandbox/lab is a copy (lab cannot import devmcp); this keeps
// it complete: no existing dev-MCP tool name, bare or as sandbox_<group>, is a legal
// group name, so a future lab tool can never shadow one.
func TestReservedToolNamesCoverEveryDevMCPTool(t *testing.T) {
	ctx := context.Background()
	ct, st := mcp.NewInMemoryTransports()
	ss, err := NewMCPServer(Deps{Cfg: &config.Config{}}).Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Tools) < 15 {
		t.Fatalf("only %d dev-MCP tools listed", len(res.Tools))
	}
	noop := func(context.Context, json.RawMessage) (any, error) { return nil, nil }
	for _, tool := range res.Tools {
		group := strings.TrimPrefix(tool.Name, "sandbox_") // the group whose tool would be this one
		for _, n := range []string{tool.Name, group} {
			if err := (&lab.Lab{}).AddGroup(lab.Group{Name: n, Ops: []lab.Op{{Run: noop}}}); err == nil {
				t.Errorf("group name %q (dev-MCP tool %q) was accepted", n, tool.Name)
			}
		}
	}
}
