package snapshot

import (
	"context"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestSaveAndGetSnapshot(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Second)
	snap := &Snapshot{
		TakenAt:    now,
		Quote:      "USDT",
		TotalValue: 12345.67,
		Label:      "daily",
		Note:       "test snapshot",
		Positions: []Position{
			{Source: "binance", Account: "main", Category: "spot", Asset: "BTC", Quantity: 0.5, Price: 20000, Value: 10000},
			{Source: "binance", Account: "main", Category: "spot", Asset: "ETH", Quantity: 10, Price: 234.567, Value: 2345.67, Meta: map[string]string{"locked": "2"}},
		},
	}

	id, err := store.SaveSnapshot(ctx, snap)
	if err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}
	if id <= 0 {
		t.Fatalf("expected positive ID, got %d", id)
	}
	if snap.ID != id {
		t.Fatalf("expected snap.ID=%d, got %d", id, snap.ID)
	}

	got, err := store.GetSnapshot(ctx, id)
	if err != nil {
		t.Fatalf("GetSnapshot: %v", err)
	}
	if got.TotalValue != 12345.67 {
		t.Errorf("TotalValue = %f, want 12345.67", got.TotalValue)
	}
	if got.Quote != "USDT" {
		t.Errorf("Quote = %q, want USDT", got.Quote)
	}
	if got.Label != "daily" {
		t.Errorf("Label = %q, want daily", got.Label)
	}
	if len(got.Positions) != 2 {
		t.Fatalf("len(Positions) = %d, want 2", len(got.Positions))
	}
	if got.Positions[1].Meta["locked"] != "2" {
		t.Errorf("Meta[locked] = %q, want 2", got.Positions[1].Meta["locked"])
	}
}

func TestGetSnapshot_NotFound(t *testing.T) {
	store := newTestStore(t)
	_, err := store.GetSnapshot(context.Background(), 999)
	if err == nil {
		t.Fatal("expected error for missing snapshot")
	}
}

func TestQuerySnapshots(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		snap := &Snapshot{
			TakenAt:    base.Add(time.Duration(i) * 24 * time.Hour),
			Quote:      "USDT",
			TotalValue: float64(1000 + i*100),
			Label:      "daily",
			Positions: []Position{
				{Source: "binance", Asset: "BTC", Quantity: 1, Value: float64(1000 + i*100)},
			},
		}
		if _, err := store.SaveSnapshot(ctx, snap); err != nil {
			t.Fatalf("SaveSnapshot %d: %v", i, err)
		}
	}

	// Query all.
	results, err := store.QuerySnapshots(ctx, QueryFilter{})
	if err != nil {
		t.Fatalf("QuerySnapshots: %v", err)
	}
	if len(results) != 5 {
		t.Errorf("expected 5 results, got %d", len(results))
	}

	// Query with time range.
	since := base.Add(48 * time.Hour)
	results, err = store.QuerySnapshots(ctx, QueryFilter{Since: &since})
	if err != nil {
		t.Fatalf("QuerySnapshots since: %v", err)
	}
	if len(results) != 3 {
		t.Errorf("expected 3 results with since filter, got %d", len(results))
	}

	// Query with limit.
	results, err = store.QuerySnapshots(ctx, QueryFilter{Limit: 2})
	if err != nil {
		t.Fatalf("QuerySnapshots limit: %v", err)
	}
	if len(results) != 2 {
		t.Errorf("expected 2 results with limit, got %d", len(results))
	}

	// Query by source.
	results, err = store.QuerySnapshots(ctx, QueryFilter{Source: "binance"})
	if err != nil {
		t.Fatalf("QuerySnapshots source: %v", err)
	}
	if len(results) != 5 {
		t.Errorf("expected 5 results with source filter, got %d", len(results))
	}

	// Query by non-existent source.
	results, err = store.QuerySnapshots(ctx, QueryFilter{Source: "kraken"})
	if err != nil {
		t.Fatalf("QuerySnapshots no-source: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results for non-existent source, got %d", len(results))
	}
}

func TestSnapshotSummary(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		snap := &Snapshot{
			TakenAt:    base.Add(time.Duration(i) * 24 * time.Hour),
			Quote:      "USDT",
			TotalValue: float64(1000 + i*500),
			Label:      "daily",
			Positions: []Position{
				{Source: "binance", Asset: "BTC", Quantity: 1, Value: float64(1000 + i*500)},
			},
		}
		store.SaveSnapshot(ctx, snap)
	}

	summary, err := store.SnapshotSummary(ctx, SummaryFilter{})
	if err != nil {
		t.Fatalf("SnapshotSummary: %v", err)
	}
	if summary.Count != 3 {
		t.Errorf("Count = %d, want 3", summary.Count)
	}
	if summary.MinValue != 1000 {
		t.Errorf("MinValue = %f, want 1000", summary.MinValue)
	}
	if summary.MaxValue != 2000 {
		t.Errorf("MaxValue = %f, want 2000", summary.MaxValue)
	}
	if summary.Change != 1000 {
		t.Errorf("Change = %f, want 1000", summary.Change)
	}
	if summary.ChangePct != 100 {
		t.Errorf("ChangePct = %f, want 100", summary.ChangePct)
	}

	// Summary with group_by=day.
	summary, err = store.SnapshotSummary(ctx, SummaryFilter{GroupBy: "day"})
	if err != nil {
		t.Fatalf("SnapshotSummary group: %v", err)
	}
	if len(summary.Groups) != 3 {
		t.Errorf("expected 3 groups, got %d", len(summary.Groups))
	}
}

func TestSnapshotSummary_Empty(t *testing.T) {
	store := newTestStore(t)
	summary, err := store.SnapshotSummary(context.Background(), SummaryFilter{})
	if err != nil {
		t.Fatalf("SnapshotSummary: %v", err)
	}
	if summary.Count != 0 {
		t.Errorf("Count = %d, want 0", summary.Count)
	}
}

func TestDeleteSnapshots(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 10; i++ {
		snap := &Snapshot{
			TakenAt:    base.Add(time.Duration(i) * 24 * time.Hour),
			Quote:      "USDT",
			TotalValue: float64(1000 + i*100),
			Label:      "daily",
			Positions: []Position{
				{Source: "binance", Asset: "BTC", Quantity: 1},
			},
		}
		store.SaveSnapshot(ctx, snap)
	}

	// Delete before day 5, keep_last=5 (should keep the 5 most recent regardless).
	before := base.Add(5 * 24 * time.Hour)
	n, err := store.DeleteSnapshots(ctx, DeleteFilter{Before: &before, KeepLast: 5})
	if err != nil {
		t.Fatalf("DeleteSnapshots: %v", err)
	}
	if n != 5 {
		t.Errorf("deleted %d, want 5", n)
	}

	// Verify remaining.
	remaining, err := store.QuerySnapshots(ctx, QueryFilter{Limit: 100})
	if err != nil {
		t.Fatalf("QuerySnapshots: %v", err)
	}
	if len(remaining) != 5 {
		t.Errorf("remaining = %d, want 5", len(remaining))
	}
}

func TestDeleteSnapshots_KeepLastSafety(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		snap := &Snapshot{
			TakenAt:    base.Add(time.Duration(i) * 24 * time.Hour),
			Quote:      "USDT",
			TotalValue: float64(1000),
			Label:      "daily",
		}
		store.SaveSnapshot(ctx, snap)
	}

	// Try to delete all with label=daily, keep_last=5 should protect all 3.
	n, err := store.DeleteSnapshots(ctx, DeleteFilter{Label: "daily", KeepLast: 5})
	if err != nil {
		t.Fatalf("DeleteSnapshots: %v", err)
	}
	if n != 0 {
		t.Errorf("deleted %d, want 0 (all protected by keep_last)", n)
	}
}

func TestDeleteSnapshots_RequiresFilter(t *testing.T) {
	store := newTestStore(t)
	_, err := store.DeleteSnapshots(context.Background(), DeleteFilter{})
	if err == nil {
		t.Fatal("expected error when no filter provided")
	}
}
