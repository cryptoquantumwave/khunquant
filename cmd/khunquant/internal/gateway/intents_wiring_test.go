package gateway

import (
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive/engine"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex"
)

// The pipeline gets the intent expiry sweeper only when the ledger is effectively
// on; with the default config the options are left untouched.
func TestAttachIntents_OnlyWhenTheLedgerIsOn(t *testing.T) {
	t.Cleanup(func() { _ = sqlitex.CloseShared() })
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = t.TempDir()
	rt := &perceptionRuntime{}

	var opts engine.Options
	rt.attachIntents(assemblyDeps{Cfg: cfg}, &opts)
	if len(opts.Housekeepers) != 0 {
		t.Fatal("the default config got a sweeper")
	}

	cfg.Proactive.Intents.Enabled = true // proactive.enabled is still off
	rt.attachIntents(assemblyDeps{Cfg: cfg}, &opts)
	if len(opts.Housekeepers) != 0 {
		t.Fatal("proactive.intents without proactive.enabled got a sweeper")
	}

	cfg.Proactive.Enabled = true
	rt.attachIntents(assemblyDeps{Cfg: cfg}, &opts)
	if len(opts.Housekeepers) != 1 {
		t.Fatalf("%d housekeepers with the ledger on, want 1", len(opts.Housekeepers))
	}
}
