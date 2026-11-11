package sandboxlab

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// sandbox_instance has the op snapshot_delete: it removes one snapshot from the store, says
// so, and refuses an id the store does not hold.
func TestInstanceTool_SnapshotDelete(t *testing.T) {
	m := &mcpServer{sup: fakeSupervisor(t)}
	dir := filepath.Join(os.Getenv("TMPDIR"), "kq-sbx-snapshots", "s-1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"id":"s-1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	call := func(args string) *mcp.CallToolResult {
		res, err := m.instanceHandler(context.Background(), &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: json.RawMessage(args)}})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if !strings.Contains(string(instanceSchema()), "snapshot_delete") {
		t.Error("snapshot_delete is not in the op enum")
	}
	if res := call(`{"op":"snapshot_delete","snapshot":"nope-1"}`); !res.IsError {
		t.Error("deleting an absent snapshot was accepted")
	}
	if res := call(`{"op":"snapshot_delete","snapshot":"../s-1"}`); !res.IsError {
		t.Error("a path was accepted as a snapshot id")
	}
	res := call(`{"op":"snapshot_delete","snapshot":"s-1"}`)
	if res.IsError || !strings.Contains(res.Content[0].(*mcp.TextContent).Text, `"deleted":"s-1"`) {
		t.Fatalf("delete = %+v", res)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the snapshot is still in the store: %v", err)
	}
}
