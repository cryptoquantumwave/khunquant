//go:build !windows

package sandboxlab

import (
	"strings"
	"testing"
)

func (h *mcpHarness) upNamed(name string) {
	h.t.Helper()
	h.must("sandbox_instance", map[string]any{"op": "up", "name": name, "fixtures_from": repoFixtures(h.t)})
}

// With one instance a call routes implicitly. With several, every proxied tool,
// sandbox_wait_for and the per-instance sandbox_instance ops refuse to guess and name the
// running instances; with `instance` they work.
func TestProxy_RequiresInstanceWhenSeveral(t *testing.T) {
	h := newHarnessMax(t, 2)
	h.upNamed("a")
	if out := h.must("sandbox_chat", map[string]any{"op": "echo", "text": "one"}); !strings.Contains(out, "one") {
		t.Fatalf("implicit call with one instance = %s", out)
	}
	h.upNamed("b")

	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{"sandbox_chat", map[string]any{"op": "echo", "text": "hi"}},
		{"sandbox_chat", map[string]any{"op": "echo", "instance": ""}},
		{"service_status", map[string]any{}},
		{"read_config", map[string]any{}},
		{"sandbox_wait_for", map[string]any{"match": map[string]any{"kind": "x"}, "timeout_wall": "1s"}},
		{"sandbox_run_until", map[string]any{"target": "+1h"}},
		{"sandbox_instance", map[string]any{"op": "logs"}},
		{"sandbox_instance", map[string]any{"op": "restart", "mode": "kill"}},
		{"sandbox_instance", map[string]any{"op": "down"}},
		{"sandbox_instance", map[string]any{"op": "snapshot"}},
		{"sandbox_instance", map[string]any{"op": "restart", "mode": "kill_at", "kill_at": map[string]any{"workflow": "dn.open", "step": "s", "point": "after_fn"}}},
	} {
		out, isErr := h.call(c.tool, c.args)
		if !isErr || !strings.Contains(out, "several instances are running") || !strings.Contains(out, "a, b") {
			t.Errorf("%s %v = %q (error=%v), want a refusal naming a, b", c.tool, c.args, out, isErr)
		}
	}
	if out := h.must("sandbox_chat", map[string]any{"op": "echo", "instance": "b", "text": "named"}); !strings.Contains(out, "named") {
		t.Errorf("named call = %s", out)
	}
	if out := h.must("service_status", map[string]any{"instance": "a"}); out != "ok" {
		t.Errorf("named dev tool = %q", out)
	}
	if out, isErr := h.call("sandbox_instance", map[string]any{"op": "up", "name": "c", "fixtures_from": repoFixtures(t)}); !isErr || !strings.Contains(out, "instance limit reached (2)") {
		t.Errorf("third up = %q (error=%v), want the cap named", out, isErr)
	}
	if out, isErr := h.call("sandbox_instance", map[string]any{"op": "up", "name": "a", "fixtures_from": repoFixtures(t)}); !isErr || !strings.Contains(out, "already running") {
		t.Errorf("up of a running name = %q (error=%v)", out, isErr)
	}
	h.must("sandbox_instance", map[string]any{"op": "down", "name": "b"})
	if out := h.must("sandbox_chat", map[string]any{"op": "echo", "text": "back"}); !strings.Contains(out, "back") {
		t.Errorf("implicit call after b went down = %s", out)
	}
}

// The snapshot ops are in the schema; an empty store lists nothing, and a restore of a
// snapshot that is not there, or whose id tries to leave the store, is refused.
func TestMCP_SnapshotOps(t *testing.T) {
	h := newHarness(t)
	if out := h.must("sandbox_instance", map[string]any{"op": "snapshots"}); out != `{"snapshots":[]}` {
		t.Errorf("snapshots = %s", out)
	}
	for _, id := range []string{"nope", "../x", ""} {
		if out, isErr := h.call("sandbox_instance", map[string]any{"op": "restore", "snapshot": id}); !isErr {
			t.Errorf("restore %q = %s, want an error", id, out)
		}
	}
	if out, isErr := h.call("sandbox_instance", map[string]any{"op": "snapshot"}); !isErr || !strings.Contains(out, "no instance is running") {
		t.Errorf("snapshot with no instance = %q (error=%v)", out, isErr)
	}
}
