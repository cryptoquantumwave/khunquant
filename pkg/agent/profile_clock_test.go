package agent

import (
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex"
)

// The agent's profile and intent store reads the process clock the gateway binds, so a
// locked instance's intents are created and expired in sim time like the gateway's own
// stores, which share the ledger (151-2).
func TestOpenProfileStore_ReadsTheProcessClock(t *testing.T) {
	t.Cleanup(func() { SetProcessClock(nil) })
	t.Cleanup(func() { _ = sqlitex.CloseShared() })
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = t.TempDir()
	store := openProfileStore(cfg)
	if store == nil {
		t.Fatal("openProfileStore returned nil")
	}

	// Bound after the store opened, as the gateway does: NewAgentLoop runs first.
	sim := time.Date(2026, 10, 20, 9, 30, 0, 0, time.UTC)
	SetProcessClock(func() time.Time { return sim })
	if got := store.Now(); !got.Equal(sim) {
		t.Fatalf("store.Now() = %v, want the process clock's %v", got, sim)
	}

	SetProcessClock(nil)
	if d := time.Since(store.Now()); d < 0 || d > time.Minute {
		t.Errorf("unbound: store.Now() is %v off the wall clock", d)
	}
}
