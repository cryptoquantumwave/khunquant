package gateway

import (
	"context"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive/engine"
)

// attachIntents hands the engine the intent ledger's expiry sweeper, which runs
// once a minute on the pipeline goroutine. It runs only from buildPerception, so
// it exists only while perception is on; with perception off nothing sweeps, and
// every read of the ledger treats a lapsed intent as expired by itself. It adds
// nothing unless intents are effectively on, and a ledger that cannot be opened
// is logged and left out: the pipeline runs without the sweeper.
func (rt *perceptionRuntime) attachIntents(d assemblyDeps, opts *engine.Options) {
	if !d.Cfg.Proactive.Effective().Intents {
		return
	}
	db, err := proactiveDB(d)
	if err != nil {
		logger.WarnCF("perception", "Intent expiry sweeper disabled: the proactive database is unavailable",
			map[string]any{"error": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := intent.NewStore(ctx, db)
	if err != nil {
		logger.WarnCF("perception", "Intent expiry sweeper disabled: the intent store could not be opened",
			map[string]any{"error": err.Error()})
		return
	}
	opts.Housekeepers = append(opts.Housekeepers, intent.NewSweeper(store))
}
