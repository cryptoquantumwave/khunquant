package worldmodel

import (
	"context"
	"testing"
	"time"
)

// TestHousekeep_PipelineDoesNotWaitForTheDatabase holds the database's write lock
// from another connection, so the checkpoint's transaction cannot start, and shows
// that Housekeep returns at once and Apply keeps running at full speed until the
// lock is released, when the checkpoint completes.
func TestHousekeep_PipelineDoesNotWaitForTheDatabase(t *testing.T) {
	db := openDB(t)
	clk := newClock()
	s := newDBStore(t, db, clk)
	ctx := context.Background()
	apply(t, s, snapshotEvent(t0, 1000))

	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}

	clk.Advance(CheckpointEvery)
	start := time.Now()
	s.Housekeep(ctx, clk.Now())
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("Housekeep took %v: it must only start the checkpoint", d)
	}
	waitFor(t, "the checkpoint to be in flight", func() bool { return s.ckBusy.Load() })

	// The pipeline keeps going while the write is blocked.
	start = time.Now()
	feedMinutes(t, s, t0, "binance", "BTC/USDT", 20, noisy)
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("120 applies took %v while the checkpoint waited for the database", d)
	}
	if s.Stats().Checkpoints != 0 {
		t.Fatal("the checkpoint finished although the write lock was held")
	}

	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the checkpoint after the lock was released", func() bool { return s.Stats().Checkpoints == 1 })
	if tableCount(t, db, "wm_facts") == 0 {
		t.Fatal("nothing written")
	}
}
