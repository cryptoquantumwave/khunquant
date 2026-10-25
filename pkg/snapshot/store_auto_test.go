package snapshot

import (
	"context"
	"errors"
	"testing"
	"time"
)

func saveAt(t *testing.T, s *Store, at time.Time, auto bool, label string) int64 {
	t.Helper()
	snap := &Snapshot{
		TakenAt: at, Quote: "USDT", TotalValue: 100, Label: label,
		Positions: []Position{{Source: "binance", Account: "main", Category: "spot", Asset: "BTC", Quantity: 1, Value: 100}},
	}
	var id int64
	var err error
	if auto {
		id, err = s.SaveAutoSnapshot(context.Background(), snap)
	} else {
		id, err = s.SaveSnapshot(context.Background(), snap)
	}
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	return id
}

func exists(t *testing.T, s *Store, id int64) bool {
	t.Helper()
	_, err := s.GetSnapshot(context.Background(), id)
	return err == nil
}

func positionCount(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM positions`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPruneAuto_KeepsDailyFirst(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	old := now.AddDate(0, 0, -40)
	young := now.AddDate(0, 0, -2)
	day := func(d time.Time, delta int, h, m int) time.Time {
		return time.Date(d.Year(), d.Month(), d.Day()+delta, h, m, 0, 0, time.UTC)
	}

	oldFirst := saveAt(t, s, day(old, 0, 0, 5), true, "")
	oldSecond := saveAt(t, s, day(old, 0, 0, 20), true, "")
	oldThird := saveAt(t, s, day(old, 0, 23, 50), true, "")
	nextDayFirst := saveAt(t, s, day(old, 1, 0, 5), true, "")
	youngFirst := saveAt(t, s, young, true, "")
	youngSecond := saveAt(t, s, young.Add(15*time.Minute), true, "")
	// Snapshots the owner or the model took, old and on the same day as autos.
	userOld := saveAt(t, s, day(old, 0, 1, 0), false, "daily")
	userNoLabel := saveAt(t, s, day(old, 0, 2, 0), false, "")

	n, err := s.pruneAutoAt(context.Background(), now, 30)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("pruned %d, want 2", n)
	}
	for name, want := range map[string]struct {
		id   int64
		keep bool
	}{
		"old first of day": {oldFirst, true},
		"old second":       {oldSecond, false},
		"old third":        {oldThird, false},
		"next day first":   {nextDayFirst, true},
		"young first":      {youngFirst, true},
		"young second":     {youngSecond, true}, // inside the window
		"user, old":        {userOld, true},
		"user, no label":   {userNoLabel, true},
	} {
		if got := exists(t, s, want.id); got != want.keep {
			t.Errorf("%s: exists=%v, want %v", name, got, want.keep)
		}
	}
	if got := positionCount(t, s); got != 6 {
		t.Errorf("%d position rows left, want 6 (one per surviving snapshot): the pruned snapshots' positions must go with them", got)
	}
}

func TestPruneAuto_IgnoresALegacyAutoLabelWithoutTheNote(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	old := now.AddDate(0, 0, -60)
	// Rows labelled "auto" by a user before the label was reserved: written raw.
	for i := 0; i < 2; i++ {
		if _, err := s.db.Exec(`INSERT INTO snapshots (taken_at, quote, total_value, label, note) VALUES (?, 'USDT', 1, 'auto', 'mine')`,
			old.Add(time.Duration(i)*time.Hour).Format(time.RFC3339)); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.pruneAutoAt(context.Background(), now, 30)
	if err != nil || n != 0 {
		t.Fatalf("pruned (%d, %v), want (0, nil)", n, err)
	}
}

func TestPruneAuto_RejectsNonPositiveKeepDays(t *testing.T) {
	s := newTestStore(t)
	for _, d := range []int{0, -1} {
		if _, err := s.PruneAuto(context.Background(), d); err == nil {
			t.Errorf("keepDays %d accepted", d)
		}
	}
}

func TestSaveSnapshot_RefusesTheReservedLabel(t *testing.T) {
	s := newTestStore(t)
	for _, l := range []string{"auto", "Auto", "  AUTO "} {
		_, err := s.SaveSnapshot(context.Background(), &Snapshot{TakenAt: time.Now(), Quote: "USDT", Label: l})
		if !errors.Is(err, ErrReservedLabel) {
			t.Errorf("label %q: err = %v, want ErrReservedLabel", l, err)
		}
	}
	id := saveAt(t, s, time.Now(), true, "ignored")
	got, err := s.GetSnapshot(context.Background(), id)
	if err != nil || got.Label != LabelAuto || got.Note != AutoNote {
		t.Fatalf("auto snapshot = %+v, %v", got, err)
	}
}
