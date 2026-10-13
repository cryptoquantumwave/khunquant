package snapshot

import (
	"context"
	"testing"
	"time"
)

// LatestAuto returns the newest genuinely automatic row, with positions, and
// nothing else: not the owner's rows, not look-alikes. It does not change what the
// listing and summary reads show.
func TestLatestAuto_ReturnsOnlyTheNewestAutomaticRow(t *testing.T) {
	f := newAutoFixture(t)
	ctx := context.Background()

	got, err := f.s.LatestAuto(ctx)
	if err != nil || got == nil {
		t.Fatalf("LatestAuto = %v, %v", got, err)
	}
	if got.ID != f.autoUnpriced {
		t.Fatalf("LatestAuto id = %d, want %d (the newest automatic row)", got.ID, f.autoUnpriced)
	}
	if len(got.Positions) != 1 || got.Positions[0].Asset != "ETH" {
		t.Fatalf("positions = %+v", got.Positions)
	}
	if n := len(UnpricedFromNote(got.Note)); n != 1 {
		t.Fatalf("unpriced = %d, note %q", n, got.Note)
	}

	// The reads that hide automatic rows still hide them.
	listed, err := f.s.QuerySnapshots(ctx, QueryFilter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range listed {
		if IsAuto(s.Label, s.Note) {
			t.Fatalf("QuerySnapshots returned automatic row %d", s.ID)
		}
	}
	sum, err := f.s.SnapshotSummary(ctx, SummaryFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Count != len(listed) {
		t.Fatalf("summary counts %d rows, listing shows %d", sum.Count, len(listed))
	}
}

func TestLatestAuto_NilWithoutAutomaticRows(t *testing.T) {
	s := newTestStore(t)
	at := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	saveAt(t, s, at, false, "daily")
	rawInsert(t, s, at, "auto", "mine") // legacy look-alike
	got, err := s.LatestAuto(context.Background())
	if err != nil || got != nil {
		t.Fatalf("LatestAuto = %v, %v; want nil, nil", got, err)
	}
}
