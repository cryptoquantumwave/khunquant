package dca

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestStore_TwoHandlesWriteConcurrently is the regression for the plan store
// opening SQLite with no busy timeout: the tools' store, the perception source,
// the gateway and the web launcher each hold their own handle on dca.db, and a
// second writer failed at once with SQLITE_BUSY instead of waiting for the lock.
func TestStore_TwoHandlesWriteConcurrently(t *testing.T) {
	ws := t.TempDir()
	a, err := NewStore(ws)
	if err != nil {
		t.Fatalf("NewStore a: %v", err)
	}
	t.Cleanup(func() { a.Close() })
	b, err := NewStore(ws)
	if err != nil {
		t.Fatalf("NewStore b: %v", err)
	}
	t.Cleanup(func() { b.Close() })

	const perHandle = 300
	ctx := context.Background()
	errs := make(chan error, 2*perHandle)
	var wg sync.WaitGroup
	for h, s := range []*Store{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perHandle; i++ {
				if _, err := s.SavePlan(ctx, minimalPlan(fmt.Sprintf("h%d-p%d", h, i))); err != nil {
					errs <- fmt.Errorf("handle %d write %d: %w", h, i, err)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if t.Failed() {
		return
	}

	plans, err := a.ListPlans(ctx, QueryFilter{Limit: 10000})
	if err != nil {
		t.Fatalf("ListPlans: %v", err)
	}
	if len(plans) != 2*perHandle {
		t.Fatalf("plans = %d, want %d (a write was lost)", len(plans), 2*perHandle)
	}
}

// TestNewStore_OpensDatabaseCreatedByOldOpener proves a dca.db written by the
// bare sql.Open the store used before opens unchanged: same data, same schema,
// still WAL, and the new open adds no tables.
func TestNewStore_OpensDatabaseCreatedByOldOpener(t *testing.T) {
	ws := t.TempDir()
	dir := filepath.Join(ws, "memory", "dca")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "dca.db")

	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"PRAGMA journal_mode=WAL", "PRAGMA synchronous=NORMAL", "PRAGMA foreign_keys=ON", "PRAGMA cache_size=-2000", schema,
	} {
		if _, err := legacy.Exec(q); err != nil {
			t.Fatalf("legacy %q: %v", q, err)
		}
	}
	for _, m := range migrations {
		if _, err := legacy.Exec(m); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			t.Fatalf("legacy migration: %v", err)
		}
	}
	old := &Store{db: legacy}
	ctx := context.Background()
	id, err := old.SavePlan(ctx, minimalPlan("old-plan"))
	if err != nil {
		t.Fatalf("legacy SavePlan: %v", err)
	}
	if _, err := old.SaveExecution(ctx, minimalExecution(id)); err != nil {
		t.Fatalf("legacy SaveExecution: %v", err)
	}
	var tablesBefore int
	if err := legacy.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE ` + legacyObjects).Scan(&tablesBefore); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := NewStore(ws)
	if err != nil {
		t.Fatalf("NewStore over an old-opener database: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	got, err := s.GetPlan(ctx, id)
	if err != nil || got.Name != "old-plan" {
		t.Fatalf("GetPlan = %+v, %v; want old-plan", got, err)
	}
	n, err := s.CountExecutions(ctx, id)
	if err != nil || n != 1 {
		t.Fatalf("CountExecutions = %d, %v; want 1", n, err)
	}
	var tablesAfter int
	var mode string
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE ` + legacyObjects).Scan(&tablesAfter); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if tablesAfter != tablesBefore {
		t.Errorf("sqlite_master rows = %d after the new open, %d before: the open changed the schema", tablesAfter, tablesBefore)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want wal", mode)
	}
}

// legacyObjects selects the schema objects an old opener knew: the mandate tables
// (and the workflow-id column index) are added by the open on purpose, so they are
// left out of the "the open changed nothing else" count.
const legacyObjects = `name NOT LIKE '%mandate%' AND name NOT LIKE '%unproven%' AND name NOT LIKE '%slot_asks%'
	AND name NOT LIKE '%notice_days%' AND name NOT LIKE '%workflow%'`
