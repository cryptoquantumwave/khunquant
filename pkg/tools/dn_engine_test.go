package tools

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral"
	"github.com/cryptoquantumwave/khunquant/pkg/journal"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex"
)

// armDNOpen runs a real journal engine (wall clock) for dn.open and dn.recover over a
// throwaway database and attaches it to open_delta_neutral_position, so tests of the
// confirmed path run the tool end to end. The fill polls do not wait. Nothing to ask
// the owner with is attached: dn.recover alerts nobody and proposes nothing.
func armDNOpen(t *testing.T, cfg *config.Config, store *deltaneutral.Store) {
	t.Helper()
	db, err := sqlitex.Open(filepath.Join(t.TempDir(), "proactive.db"))
	if err != nil {
		t.Fatal(err)
	}
	deps := &deltaneutral.Deps{
		Store: store, Exchange: NewDNExchange(cfg, nil), PollInterval: -1,
		// The failure seam the leg-save tests use: production code never replaces it.
		SaveLeg: func(ctx context.Context, s *deltaneutral.Store, leg *deltaneutral.ExecutionLeg) (int64, error) {
			return saveExecutionLegFn(ctx, s, leg)
		},
		Tag: func(ctx context.Context, origin, sender string) context.Context {
			return WithToolSender(WithToolOrigin(ctx, origin), sender)
		},
	}
	eng, err := journal.New(db, deps.Definitions(), nil, journal.WithTick(5*time.Millisecond), journal.WithWaitPoll(2*time.Millisecond),
		journal.WithShutdownWait(200*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	deps.SetStarter(eng)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = eng.Run(ctx) }()
	SetDNWorkflows(eng)
	t.Cleanup(func() {
		SetDNWorkflows(nil)
		cancel()
		<-done
		_ = db.Close()
	})
}
