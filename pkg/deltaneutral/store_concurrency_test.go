package deltaneutral

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func concurrencyTestPlan(name string) *Plan {
	now := time.Now()
	return &Plan{
		Name:                name,
		Asset:               "ETH",
		Status:              PlanStatusDraft,
		Mode:                ExecutionModeApproval,
		SpotProvider:        "binance",
		SpotSymbol:          "ETHUSDT",
		SpotSide:            "buy",
		FuturesProvider:     "binance",
		FuturesSymbol:       "ETHUSDT",
		FuturesSide:         "short",
		FuturesMarginMode:   "cross",
		FuturesLeverage:     1,
		CapitalUSDT:         10000,
		SpotNotionalUSDT:    5000,
		FuturesNotionalUSDT: 5000,
		MonitorInterval:     "5m",
		Enabled:             true,
		RiskPolicy:          DefaultRiskPolicy(),
		CreatedAt:           now,
		UpdatedAt:           now,
	}
}

// TestStore_TwoHandlesWriteConcurrently is the regression for the plan store
// opening SQLite with no busy timeout: the tools' store, the perception source,
// the gateway's DN sync and the web launcher each hold their own handle on
// delta_neutral.db, and a second writer failed at once with SQLITE_BUSY
// instead of waiting for the lock.
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

	ctx := context.Background()
	planID, err := a.SavePlan(ctx, concurrencyTestPlan("p"))
	if err != nil {
		t.Fatalf("SavePlan: %v", err)
	}

	const perHandle = 300
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for h, s := range []*Store{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perHandle; i++ {
				now := time.Now()
				_, err := s.SaveExecution(ctx, &Execution{
					PlanID: planID, AttemptID: fmt.Sprintf("h%d-a%d", h, i), State: "pending",
					RequestedAt: now, CreatedAt: now, UpdatedAt: now,
				})
				if err != nil {
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

	var n int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM delta_neutral_executions WHERE plan_id = ?`, planID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2*perHandle {
		t.Fatalf("executions = %d, want %d (a write was lost)", n, 2*perHandle)
	}
}

// TestNewStore_OpensDatabaseCreatedByOldOpener proves a delta_neutral.db written
// by the bare sql.Open the store used before opens unchanged: same data, same
// schema, still WAL, and the new open adds no tables.
func TestNewStore_OpensDatabaseCreatedByOldOpener(t *testing.T) {
	ws := t.TempDir()
	dir := filepath.Join(ws, "memory", "delta_neutral")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "delta_neutral.db")

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
	id, err := old.SavePlan(ctx, concurrencyTestPlan("old-plan"))
	if err != nil {
		t.Fatalf("legacy SavePlan: %v", err)
	}
	var tablesBefore int
	if err := legacy.QueryRow(`SELECT COUNT(*) FROM sqlite_master`).Scan(&tablesBefore); err != nil {
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
	if err != nil || got == nil || got.Name != "old-plan" {
		t.Fatalf("GetPlan = %+v, %v; want old-plan", got, err)
	}
	var tablesAfter int
	var mode string
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master`).Scan(&tablesAfter); err != nil {
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
