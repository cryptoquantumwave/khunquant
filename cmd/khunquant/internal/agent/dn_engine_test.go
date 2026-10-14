package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral"
	"github.com/cryptoquantumwave/khunquant/pkg/journal"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex"
)

func TestStartDNEngine_NothingWithoutDeltaNeutralPlans(t *testing.T) {
	dir := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = dir
	stop := startDNEngine(cfg)
	stop()
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("a CLI session without delta-neutral plans must not create anything: %v", entries)
	}
}

// A kill flag that cannot be read is treated as engaged: the CLI engine fails closed.
func TestOpenKillFlag_FailsClosedWhenItCannotBeOpened(t *testing.T) {
	db, err := sqlitex.Open(filepath.Join(t.TempDir(), "proactive.db"))
	if err != nil {
		t.Fatal(err)
	}
	if openKillFlag(db)() {
		t.Error("a working database with no kill row must read as not killed")
	}
	_ = db.Close() // an unusable database
	if !openKillFlag(db)() {
		t.Error("an unreadable kill flag must read as engaged")
	}
}

// The CLI engine claims dn.open and never dn.recover (it cannot ask the owner), and
// it stops within its bound.
func TestRunDNEngine_ClaimsOpenNotRecover(t *testing.T) {
	cfg := config.DefaultConfig()
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

	// A second engine that knows both kinds, as the gateway does, starts a recovery.
	deps := &deltaneutral.Deps{Store: store}
	gw, err := journal.New(db, deps.Definitions(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gw.StartWithID(context.Background(), deltaneutral.KindRecover, "dnrec_1",
		deltaneutral.RecoverInput{PlanID: 1, ExecutionID: 1}); err != nil {
		t.Fatal(err)
	}

	runner, stop := runDNEngine(db, store, cfg, nil)
	if runner == nil {
		t.Fatal("no engine")
	}
	time.Sleep(1500 * time.Millisecond) // more than one scan tick
	if w, err := gw.Get(context.Background(), "dnrec_1"); err != nil || w.Status != journal.StatusPending {
		t.Errorf("the CLI engine must leave dn.recover to the gateway: %+v %v", w, err)
	}
	done := make(chan struct{})
	go func() { stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(dnEngineStopTimeout + 2*time.Second):
		t.Fatal("the engine did not stop within its bound")
	}
}
