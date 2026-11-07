package gateway

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

// The saved world and the crash marker are in <home>/lab, outside the workspace the sandbox
// profile confines the instance's file tools to: an agent cannot rewrite the venue's memory,
// the budget, the boot counter or the marker.
func TestWorldIsOutsideTheFileTools(t *testing.T) {
	home := t.TempDir()
	workspace := filepath.Join(home, "workspace")
	for _, d := range []string{workspace, filepath.Join(home, "lab", "world")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	target := filepath.Join(home, "lab", "world", "exchange.json")
	if err := os.WriteFile(target, []byte("saved"), 0o600); err != nil {
		t.Fatal(err)
	}
	w := tools.NewWriteFileTool(workspace, true)
	for _, p := range []string{
		target,
		filepath.Join(workspace, "..", "lab", "world", "exchange.json"),
		filepath.Join(home, "lab", "boot"),
		filepath.Join(home, "lab", "budget.json"),
		filepath.Join(home, "lab", "crash.json"),
	} {
		res := w.Execute(context.Background(), map[string]any{"path": p, "content": "forged", "overwrite": true})
		if res == nil || !res.IsError {
			t.Errorf("write_file wrote %s: %+v", p, res)
		}
		if _, err := os.Stat(p); err == nil && p != target {
			t.Errorf("%s exists after the write", p)
		}
	}
	if b, _ := os.ReadFile(target); string(b) != "saved" {
		t.Errorf("the saved world was changed to %q", b)
	}
}
