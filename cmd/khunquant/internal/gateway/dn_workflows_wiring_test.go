package gateway

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/control"
	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex"
)

// dn.open's kill flag is the SAME control.State the approval service gets
// (controlRuntime.killer), not a second one: engaging it stops the place steps.
func TestBuildDNFlow_KillFlagIsTheControlState(t *testing.T) {
	store, err := deltaneutral.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	db, err := sqlitex.Open(filepath.Join(t.TempDir(), "proactive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	st, err := control.NewState(context.Background(), db, control.WithRefresh(time.Nanosecond))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultConfig()
	cfg.Tools.OpenDeltaNeutralPosition.Enabled = true
	d := assemblyDeps{Cfg: cfg, DNStore: store, Control: &controlRuntime{state: st}}

	rt := buildDNFlow(d, nil, nil)
	if rt == nil || rt.deps.Killed == nil {
		t.Fatalf("the kill flag is not wired: %+v", rt)
	}
	if rt.deps.Killed() {
		t.Fatal("killed before anything engaged it")
	}
	if _, err := st.Engage(context.Background(), "test", "stop"); err != nil {
		t.Fatal(err)
	}
	if !rt.deps.Killed() {
		t.Error("engaging the control state must stop dn.open")
	}
	// Without a control state (proactive off) it is nil, as the approval service's Killer is.
	d.Control = &controlRuntime{}
	if rt := buildDNFlow(d, nil, nil); rt == nil || rt.deps.Killed != nil {
		t.Error("no control state: no kill flag, as for approvals")
	}
}
