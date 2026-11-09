package sandboxlab

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// A panic in one call is that call's error, and the server keeps serving: go-sdk does not
// recover handlers, so a panic used to end the process without Close (144-2).
func TestMCP_APanickingCallIsAnError(t *testing.T) {
	srv := newMCPServer(fakeSupervisor(t))
	srv.AddTool(&mcp.Tool{Name: "boom", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) { panic("boom") })
	cr, sw := io.Pipe()
	sr, cw := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if _, err := srv.Connect(ctx, &mcp.IOTransport{Reader: sr, Writer: sw}, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, &mcp.IOTransport{Reader: cr, Writer: cw}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })

	if _, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "boom", Arguments: map[string]any{}}); err == nil || !strings.Contains(err.Error(), "internal error") {
		t.Fatalf("a panicking call = %v, want an internal error", err)
	}
	if _, err := cs.ListTools(ctx, nil); err != nil {
		t.Fatalf("after the panic: %v", err)
	}
}

// sandbox_scenario run is bounded and runs one at a time (162-1).
func TestScenarioTool_RunIsSingleFlightAndBounded(t *testing.T) {
	m := &mcpServer{sup: fakeSupervisor(t)}
	inline := strings.ReplaceAll(goodScenario, "%ID%", "bounded")
	if res, _ := m.runOp(context.Background(), scenarioArgs{Op: "run", Inline: inline, Runs: maxMCPRuns + 1}); !res.IsError {
		t.Error("a run asking for more than maxMCPRuns runs was accepted")
	}
	m.runMu.Lock()
	res, _ := m.runOp(context.Background(), scenarioArgs{Op: "run", Inline: inline})
	m.runMu.Unlock()
	if !res.IsError {
		t.Error("a second run started while one was in progress")
	}
}

// The run has a wall cap: one that cannot finish ends as an infrastructure failure (exit 3)
// instead of holding the call (162-1).
func TestScenarioTool_RunHasAWallCap(t *testing.T) {
	m := &mcpServer{sup: fakeSupervisor(t), runTimeout: time.Nanosecond}
	res, _ := m.runOp(context.Background(), scenarioArgs{Op: "run", Inline: strings.ReplaceAll(goodScenario, "%ID%", "capped")})
	if res.IsError || len(res.Content) == 0 {
		t.Fatalf("result = %+v", res)
	}
	text := res.Content[0].(*mcp.TextContent).Text
	if !strings.Contains(text, `"exit_code":3`) {
		t.Fatalf("a run past its wall cap = %s, want exit_code 3", text)
	}
}
