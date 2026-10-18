package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

// policyRows returns how many verdicts the shared database holds, and whether the
// table exists at all.
func policyRows(t *testing.T, workspace string) (n int, exists bool) {
	t.Helper()
	db, err := sqlitex.Shared(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'policy_decisions'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		return 0, false
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM policy_decisions`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n, true
}

// callReadTool runs a harmless read-class tool through the registry, the funnel
// every model-driven call goes through.
func callReadTool(t *testing.T, f *assemblyFixture) {
	t.Helper()
	ctx := tools.WithToolOrigin(context.Background(), tools.OriginInternal)
	res := f.al.GetRegistry().GetDefaultAgent().Tools.ExecuteWithContext(
		ctx, tools.NameListDir, map[string]any{"path": f.workspace}, "cli", "direct", nil)
	if res == nil || res.IsError {
		t.Fatalf("list_dir: %+v", res)
	}
	// The record is written by the recorder's own goroutine; wait for it.
	if p := f.services.Policy; p != nil && !p.rec.Flush(5*time.Second) {
		t.Fatal("the policy recorder did not drain")
	}
}

func withPolicy(mode string, proactiveOn bool) func(*config.Config) {
	return func(cfg *config.Config) {
		if proactiveOn {
			withOwner("1001")(cfg)
		}
		cfg.Proactive.Policy.Mode = mode
	}
}

// The shadow record exists only while proactive is on AND the mode is shadow or
// live. With either off, no request is built, the engine is not called and no table
// or row is created.
func TestPolicyDecisions_OnlyWhenProactiveOn(t *testing.T) {
	t.Cleanup(func() { tools.SetPolicyShadow(nil) })
	for _, tt := range []struct {
		name      string
		mode      string
		proactive bool
		records   bool
	}{
		{"proactive off, mode shadow", config.ProactiveModeShadow, false, false},
		{"proactive off, mode live", config.ProactiveModeLive, false, false},
		{"proactive on, mode off", config.ProactiveModeOff, true, false},
		{"proactive on, mode unset", "", true, false},
		{"proactive on, mode shadow", config.ProactiveModeShadow, true, true},
		{"proactive on, mode live behaves as shadow", config.ProactiveModeLive, true, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newAssemblyFixture(t, withPolicy(tt.mode, tt.proactive))
			if got := f.services.Policy != nil; got != tt.records {
				t.Fatalf("policy runtime present = %v, want %v", got, tt.records)
			}
			callReadTool(t, f)
			n, exists := policyRows(t, f.workspace)
			if tt.records && (!exists || n < 1) {
				t.Fatalf("no verdict was recorded (table exists %v, rows %d)", exists, n)
			}
			if !tt.records && (exists || n != 0) {
				t.Fatalf("a verdict was recorded with the engine off (table exists %v, rows %d)", exists, n)
			}
		})
	}
}

// A reload that turns the mode off stops recording at once; turning it on starts it.
func TestPolicyDecisions_FollowTheModeAcrossReloads(t *testing.T) {
	t.Cleanup(func() { tools.SetPolicyShadow(nil) })
	f := newAssemblyFixture(t, withPolicy(config.ProactiveModeShadow, true))
	callReadTool(t, f)
	before, _ := policyRows(t, f.workspace)
	if before < 1 {
		t.Fatal("nothing recorded in shadow mode")
	}

	if err := f.reload(withPolicy(config.ProactiveModeOff, true)); err != nil {
		t.Fatal(err)
	}
	callReadTool(t, f)
	if after, _ := policyRows(t, f.workspace); after != before {
		t.Fatalf("rows grew from %d to %d with the mode off", before, after)
	}

	if err := f.reload(withPolicy(config.ProactiveModeShadow, true)); err != nil {
		t.Fatal(err)
	}
	callReadTool(t, f)
	if again, _ := policyRows(t, f.workspace); again <= before {
		t.Fatalf("rows stayed at %d after the mode came back on", again)
	}
}
