package worldmodel

import (
	"context"
	"testing"
)

// The store is busy while a checkpoint is in flight (here held by another connection's
// write lock) and idle before it starts, between checkpoints and after it completes.
func TestWorldModel_IdleCoversACheckpoint(t *testing.T) {
	db := openDB(t)
	clk := newClock()
	s := newDBStore(t, db, clk)
	ctx := context.Background()
	apply(t, s, snapshotEvent(t0, 1000))
	if !s.Idle() {
		t.Fatal("a store with a checkpoint not yet due is idle")
	}

	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	clk.Advance(CheckpointEvery)
	s.Housekeep(ctx, clk.Now())
	waitFor(t, "the checkpoint to be in flight", func() bool { return s.ckBusy.Load() })
	before := s.Stats()
	if s.Idle() || s.Idle() {
		t.Fatal("idle while a checkpoint is waiting on the database")
	}
	if got := s.Stats(); got != before {
		t.Fatal("Idle changed the store")
	}

	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the checkpoint to finish", func() bool { return s.Stats().Checkpoints == 1 })
	waitFor(t, "idle after the checkpoint", s.Idle)
}

// A memory-only store has nothing to checkpoint.
func TestWorldModel_IdleMemoryOnly(t *testing.T) {
	if !newMemStore(t, newClock()).Idle() {
		t.Fatal("a memory-only store is idle")
	}
}
