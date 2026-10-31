package inspect_test

import (
	"context"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab/inspect"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/quiesce"
)

func settleDeps(reg *quiesce.Registry, gotCap *time.Duration) func() inspect.Deps {
	return func() inspect.Deps {
		return inspect.Deps{
			SettleWait: func(ctx context.Context, c time.Duration) (quiesce.Report, error) {
				if gotCap != nil {
					*gotCap = c
				}
				return reg.SettleReport(ctx, c)
			},
			Snapshot: reg.Snapshot,
		}
	}
}

// settle returns settled:true on an idle registry, names a stuck quiescer when the cap
// passes, and clamps wall_cap to 25 s.
func TestInspectSettle_Op(t *testing.T) {
	reg := quiesce.NewRegistry()
	reg.Register(quiesce.Func("ok", func() bool { return true }))
	var gotCap time.Duration
	r := newRig(t, settleDeps(reg, &gotCap))

	out := r.mustCall("settle", `{}`)
	if out["settled"] != true || len(out["busy"].([]any)) != 0 {
		t.Fatalf("idle lab: %v", out)
	}
	if gotCap != 10*time.Second {
		t.Fatalf("default cap = %v, want 10s", gotCap)
	}

	reg.Register(quiesce.Func("stuck", func() bool { return false }))
	out = r.mustCall("settle", `{"wall_cap":"40ms"}`)
	busy := out["busy"].([]any)
	if out["settled"] != false || len(busy) != 1 || busy[0] != "stuck" {
		t.Fatalf("stuck lab: %v", out)
	}

	reg.Register(quiesce.Func("stuck", func() bool { return true }))
	r.mustCall("settle", `{"wall_cap":"10m"}`)
	if gotCap != 25*time.Second {
		t.Fatalf("a cap of 10m was passed as %v, want 25s", gotCap)
	}
	if _, err := r.call("settle", `{"wall_cap":"soon"}`); err == nil {
		t.Fatal("a bad wall_cap was accepted")
	}
	if _, err := r.call("settle", `{"wall_cap":"-1s"}`); err == nil {
		t.Fatal("a negative wall_cap was accepted")
	}
}

func TestInspectIdle_Op(t *testing.T) {
	reg := quiesce.NewRegistry()
	reg.Register(quiesce.Func("b", func() bool { return false }))
	reg.Register(quiesce.Func("a", func() bool { return true }))
	r := newRig(t, settleDeps(reg, nil))
	out := r.mustCall("idle", `{}`)
	qs := out["quiescers"].([]any)
	if len(qs) != 2 || qs[0].(map[string]any)["name"] != "a" || qs[0].(map[string]any)["idle"] != true ||
		qs[1].(map[string]any)["name"] != "b" || qs[1].(map[string]any)["idle"] != false {
		t.Fatalf("quiescers = %v", qs)
	}

	bare := newRig(t, nil)
	if _, err := bare.call("idle", `{}`); err == nil {
		t.Fatal("idle without a registry should be unavailable")
	}
	var _ lab.Group
}
