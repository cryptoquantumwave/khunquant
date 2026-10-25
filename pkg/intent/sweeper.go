package intent

import (
	"context"
	"sync"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/logger"
)

// SweepEvery is how often the Sweeper expires lapsed intents.
const SweepEvery = time.Minute

// Sweeper expires lapsed intents. It is a pipeline housekeeper (the engine calls
// Housekeep every second on its own goroutine) and does its work once a minute,
// using the time it is given. It starts no goroutine of its own.
//
// Reads do not wait for it: GetIntent, ListIntents and the digest already treat
// an intent past its ExpiresAt as expired. The sweeper only keeps the stored rows
// and the event history current, and so works the same with the pipeline off,
// where it simply never runs.
type Sweeper struct {
	store *Store

	mu   sync.Mutex
	last time.Time
}

// NewSweeper returns a Sweeper over store.
func NewSweeper(store *Store) *Sweeper { return &Sweeper{store: store} }

// Housekeep expires what has lapsed by now, at most once per SweepEvery.
func (w *Sweeper) Housekeep(ctx context.Context, now time.Time) {
	if w == nil || w.store == nil {
		return
	}
	w.mu.Lock()
	if !w.last.IsZero() && now.Sub(w.last) < SweepEvery {
		w.mu.Unlock()
		return
	}
	w.last = now
	w.mu.Unlock()

	n, err := w.store.ExpireDue(ctx, now)
	if err != nil {
		logger.WarnCF("intent", "Could not expire lapsed intents", map[string]any{"error": err.Error()})
		return
	}
	if n > 0 {
		logger.InfoCF("intent", "Expired lapsed intents", map[string]any{"count": n})
	}
}
