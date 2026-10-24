package snapshot

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func ids(snaps []Snapshot) []int64 {
	out := make([]int64, len(snaps))
	for i, s := range snaps {
		out[i] = s.ID
	}
	return out
}

func rawInsert(t *testing.T, s *Store, at time.Time, label, note string) int64 {
	t.Helper()
	res, err := s.db.Exec(`INSERT INTO snapshots (taken_at, quote, total_value, label, note) VALUES (?, 'USDT', 1, ?, ?)`,
		at.UTC().Format(time.RFC3339), label, note)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

// autoFixture: the owner's snapshots, automatic ones taken after them (the newest
// rows in the table), and look-alikes that are NOT automatic.
type autoFixture struct {
	s                            *Store
	owner1, owner2, ownerLabeled int64
	auto1, auto2, autoUnpriced   int64
	legacyAutoLabel              int64 // label "auto" but another note: saved before the label was reserved
	autoNoteOtherLabel           int64 // the automatic note under another label
	autoNoteExtended             int64 // label "auto", note that merely starts like AutoNote
}

func newAutoFixture(t *testing.T) *autoFixture {
	t.Helper()
	s := newTestStore(t)
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	at := func(h int) time.Time { return base.Add(time.Duration(h) * time.Hour) }
	f := &autoFixture{s: s}
	f.owner1 = saveAt(t, s, at(0), false, "")
	f.owner2 = saveAt(t, s, at(1), false, "")
	f.ownerLabeled = saveAt(t, s, at(2), false, "daily")
	f.legacyAutoLabel = rawInsert(t, s, at(3), "auto", "mine")
	f.autoNoteOtherLabel = rawInsert(t, s, at(4), "daily", AutoNote)
	f.autoNoteExtended = rawInsert(t, s, at(5), "auto", AutoNote+" extended")
	f.auto1 = saveAt(t, s, at(10), true, "")
	f.auto2 = saveAt(t, s, at(11), true, "")
	snap := &Snapshot{TakenAt: at(12), Quote: "USDT", TotalValue: 5, Note: AutoNoteWith([]string{"binance/main:ZZZ"}),
		Positions: []Position{{Source: "binance", Account: "main", Category: "spot", Asset: "ETH", Quantity: 1, Value: 5}}}
	id, err := s.SaveAutoSnapshot(context.Background(), snap)
	if err != nil {
		t.Fatal(err)
	}
	f.autoUnpriced = id
	return f
}

func TestQuerySnapshots_AutomaticRowsAppearOnlyWhenAskedByLabel(t *testing.T) {
	f := newAutoFixture(t)
	ctx := context.Background()
	automatic := map[int64]bool{f.auto1: true, f.auto2: true, f.autoUnpriced: true}
	since := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

	for name, flt := range map[string]QueryFilter{
		"no filter":     {Limit: 100},
		"default limit": {},
		"time range":    {Since: &since, Until: &until, Limit: 100},
		"other label":   {Label: "daily", Limit: 100},
		"source":        {Source: "binance", Limit: 100},
		"asset":         {Asset: "ETH", Limit: 100},
	} {
		got, err := f.s.QuerySnapshots(ctx, flt)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, sn := range got {
			if automatic[sn.ID] {
				t.Errorf("%s: automatic snapshot %d listed", name, sn.ID)
			}
		}
	}

	// The default listing is the owner's, newest first, and the look-alikes that are
	// not automatic are in it.
	got, _ := f.s.QuerySnapshots(ctx, QueryFilter{Limit: 100})
	want := []int64{f.autoNoteExtended, f.autoNoteOtherLabel, f.legacyAutoLabel, f.ownerLabeled, f.owner2, f.owner1}
	if !reflect.DeepEqual(ids(got), want) {
		t.Errorf("default listing = %v, want %v", ids(got), want)
	}

	// Asked for by label, the automatic ones come back (and what carries the label
	// without being automatic with them).
	got, _ = f.s.QuerySnapshots(ctx, QueryFilter{Label: LabelAuto, Limit: 100})
	wantAuto := []int64{f.autoUnpriced, f.auto2, f.auto1, f.autoNoteExtended, f.legacyAutoLabel}
	if !reflect.DeepEqual(ids(got), wantAuto) {
		t.Errorf("label=auto listing = %v, want %v", ids(got), wantAuto)
	}
	// "Exactly": another spelling of the label does not ask for them.
	got, _ = f.s.QuerySnapshots(ctx, QueryFilter{Label: "Auto", Limit: 100})
	if len(got) != 0 {
		t.Errorf("label=Auto returned %v", ids(got))
	}

	// GetSnapshot reaches any row by id.
	for id := range automatic {
		if _, err := f.s.GetSnapshot(ctx, id); err != nil {
			t.Errorf("GetSnapshot(%d): %v", id, err)
		}
	}
}

func TestSnapshotSummary_CountsAutomaticRowsOnlyWhenAskedByLabel(t *testing.T) {
	f := newAutoFixture(t)
	ctx := context.Background()

	all, err := f.s.SnapshotSummary(ctx, SummaryFilter{GroupBy: "day"})
	if err != nil {
		t.Fatal(err)
	}
	if all.Count != 6 {
		t.Errorf("summary counts %d snapshots, want the 6 that are not automatic", all.Count)
	}
	total := 0
	for _, g := range all.Groups {
		total += g.Count
	}
	if total != 6 {
		t.Errorf("groups hold %d snapshots, want 6", total)
	}
	src, _ := f.s.SnapshotSummary(ctx, SummaryFilter{Source: "binance"})
	if src.Count != 3 { // the three owner snapshots with a binance position; no automatic row is counted
		t.Errorf("source summary counts %d, want 3", src.Count)
	}

	auto, err := f.s.SnapshotSummary(ctx, SummaryFilter{Label: LabelAuto})
	if err != nil {
		t.Fatal(err)
	}
	if auto.Count != 5 {
		t.Errorf("label=auto summary counts %d, want 5 (3 automatic + 2 that carry the label)", auto.Count)
	}
}

func TestAutoNote_BothDirections(t *testing.T) {
	for note, want := range map[string]bool{
		AutoNote:                             true,
		AutoNoteWith([]string{"a/b:C"}):      true,
		AutoNoteWith(nil):                    true,
		AutoNote + " extended":               false,
		AutoNote + "; unpriced":              false, // no "=": not the shape we write
		"x" + AutoNote:                       false,
		"mine":                               false,
		"":                                   false,
		strings.ToUpper(AutoNote):            false,
		"; unpriced=a/b:C":                   false,
		AutoNote + "; unpriced=a/b:C" + "\n": true, // the prefix is what is matched
		"scheduled portfolio snapshot":       false,
	} {
		if got := IsAutoNote(note); got != want {
			t.Errorf("IsAutoNote(%q) = %v, want %v", note, got, want)
		}
	}
	if IsAuto("daily", AutoNote) || IsAuto("auto", "mine") || !IsAuto("auto", AutoNote) {
		t.Error("IsAuto must need both the label and an automatic note")
	}
}

func TestAutoNoteWith_RoundTripsTheUnpricedList(t *testing.T) {
	in := []string{"okx/main:ZZZ", "binance/main:AAA", "okx/main:ZZZ", "", "bad,comma:X", "bad\nline:Y", "binance/main:BBB"}
	want := []string{"binance/main:AAA", "binance/main:BBB", "okx/main:ZZZ"}
	note := AutoNoteWith(in)
	if !IsAutoNote(note) {
		t.Fatalf("note %q is not an automatic note", note)
	}
	if got := UnpricedFromNote(note); !reflect.DeepEqual(got, want) {
		t.Errorf("UnpricedFromNote = %q, want %q", got, want)
	}
	if UnpricedFromNote(AutoNote) != nil || UnpricedFromNote("mine") != nil {
		t.Error("a note without a list yields none")
	}

	many := make([]string, MaxUnpriced+50)
	for i := range many {
		many[i] = fmt.Sprintf("x/y:T%04d", i)
	}
	if got := UnpricedFromNote(AutoNoteWith(many)); len(got) != MaxUnpriced || got[0] != "x/y:T0000" {
		t.Errorf("list of %d not capped to %d (got %d)", len(many), MaxUnpriced, len(got))
	}
}

func TestSaveAutoSnapshot_KeepsTheUnpricedListAndRejectsOtherNotes(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	note := AutoNoteWith([]string{"binance/main:ZZZ", "okx/main:OLD"})
	id, err := s.SaveAutoSnapshot(ctx, &Snapshot{TakenAt: time.Now(), Quote: "USDT", Note: note})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSnapshot(ctx, id)
	if err != nil || got.Label != LabelAuto || got.Note != note {
		t.Fatalf("stored %+v, %v", got, err)
	}
	if !reflect.DeepEqual(UnpricedFromNote(got.Note), []string{"binance/main:ZZZ", "okx/main:OLD"}) {
		t.Errorf("the list did not survive the store: %q", got.Note)
	}
	// A caller cannot smuggle a free-text note into an automatic row.
	id, _ = s.SaveAutoSnapshot(ctx, &Snapshot{TakenAt: time.Now(), Quote: "USDT", Note: "hello"})
	if got, _ := s.GetSnapshot(ctx, id); got.Note != AutoNote {
		t.Errorf("note = %q, want %q", got.Note, AutoNote)
	}
}

func TestPruneAuto_RecognisesTheUnpricedNoteAndOnlyThat(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	old := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)

	first := saveAt(t, s, old, true, "")
	withList, err := s.SaveAutoSnapshot(ctx, &Snapshot{TakenAt: old.Add(time.Hour), Quote: "USDT", Note: AutoNoteWith([]string{"a/b:C"})})
	if err != nil {
		t.Fatal(err)
	}
	// Not automatic, however old and however much they look like it.
	keep := []int64{
		rawInsert(t, s, old.Add(2*time.Hour), "auto", AutoNote+" extended"),
		rawInsert(t, s, old.Add(3*time.Hour), "auto", "mine"),
		rawInsert(t, s, old.Add(4*time.Hour), "daily", AutoNote),
		rawInsert(t, s, old.Add(5*time.Hour), "daily", AutoNoteWith([]string{"a/b:C"})),
		rawInsert(t, s, old.Add(6*time.Hour), "", ""),
	}
	n, err := s.pruneAutoAt(ctx, now, 30)
	if err != nil || n != 1 {
		t.Fatalf("pruned (%d, %v), want (1, nil): only the second automatic row of the day", n, err)
	}
	if !exists(t, s, first) || exists(t, s, withList) {
		t.Errorf("first exists=%v, withList exists=%v", exists(t, s, first), exists(t, s, withList))
	}
	for _, id := range keep {
		if !exists(t, s, id) {
			t.Errorf("row %d, which is not automatic, was pruned", id)
		}
	}
}

func TestDeleteSnapshots_AutomaticRowsDoNotTakeTheSafetyNet(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	var owner, auto []int64
	for i := 0; i < 8; i++ {
		owner = append(owner, saveAt(t, s, base.Add(time.Duration(i)*time.Hour), false, "daily"))
	}
	for i := 0; i < 20; i++ { // newer than every owner snapshot
		auto = append(auto, saveAt(t, s, base.Add(time.Duration(100+i)*time.Hour), true, ""))
	}
	before := base.AddDate(1, 0, 0)

	// A delete by time removes the owner's oldest, keeps the owner's newest 5, and
	// does not touch any automatic row.
	n, err := s.DeleteSnapshots(ctx, DeleteFilter{Before: &before})
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("deleted %d, want 3 (8 owner snapshots, the newest 5 kept)", n)
	}
	for i, id := range owner {
		if want := i >= 3; exists(t, s, id) != want {
			t.Errorf("owner snapshot %d exists=%v, want %v", i, !want, want)
		}
	}
	for _, id := range auto {
		if !exists(t, s, id) {
			t.Fatal("a delete by time removed an automatic snapshot")
		}
	}

	// A delete by another label does not either.
	if n, _ := s.DeleteSnapshots(ctx, DeleteFilter{Label: "daily", KeepLast: 1}); n != 4 {
		t.Errorf("label=daily deleted %d, want 4", n)
	}
	for _, id := range auto {
		if !exists(t, s, id) {
			t.Fatal("a delete by label removed an automatic snapshot")
		}
	}

	// By the label "auto" it works on the automatic rows, keeps the newest KeepLast of
	// them, and leaves the owner's alone.
	n, err = s.DeleteSnapshots(ctx, DeleteFilter{Label: LabelAuto, KeepLast: 3})
	if err != nil || n != 17 {
		t.Fatalf("label=auto deleted (%d, %v), want 17", n, err)
	}
	for i, id := range auto {
		if want := i >= 17; exists(t, s, id) != want {
			t.Errorf("automatic snapshot %d exists=%v, want %v", i, !want, want)
		}
	}
	if !exists(t, s, owner[7]) {
		t.Error("a delete by the label auto removed an owner snapshot")
	}

	// An ID names one explicitly: allowed.
	target := auto[19]
	if n, err := s.DeleteSnapshots(ctx, DeleteFilter{ID: &target}); err != nil || n != 1 || exists(t, s, target) {
		t.Errorf("delete by the id of an automatic row = (%d, %v), exists=%v", n, err, exists(t, s, target))
	}
}

// TestTwoHandlesOnOneFileDoNotCollide: the tools' handle, the web handler's per-request
// handle and the portfolio source's handle all write this file. With the pragmas in the
// DSN every pooled connection waits for the lock instead of failing at once.
func TestTwoHandlesOnOneFileDoNotCollide(t *testing.T) {
	dir := t.TempDir()
	a, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	ctx := context.Background()

	const rounds = 60
	errs := make(chan error, 4*rounds)
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { // the portfolio source: an automatic snapshot, over and over
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			snap := &Snapshot{TakenAt: time.Now().Add(-40 * 24 * time.Hour).Add(time.Duration(i) * time.Minute), Quote: "USDT", TotalValue: 1,
				Positions: []Position{{Source: "binance", Asset: "BTC", Quantity: 1, Value: 1}}}
			if _, err := a.SaveAutoSnapshot(ctx, snap); err != nil {
				errs <- fmt.Errorf("auto save: %w", err)
			}
		}
	}()
	go func() { // the tools' handle: the owner's snapshots and a delete
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			snap := &Snapshot{TakenAt: time.Now(), Quote: "USDT", TotalValue: 2, Label: "daily",
				Positions: []Position{{Source: "binance", Asset: "ETH", Quantity: 1, Value: 2}}}
			if _, err := b.SaveSnapshot(ctx, snap); err != nil {
				errs <- fmt.Errorf("owner save: %w", err)
			}
			if i%10 == 9 {
				before := time.Now().Add(time.Hour)
				if _, err := b.DeleteSnapshots(ctx, DeleteFilter{Before: &before}); err != nil {
					errs <- fmt.Errorf("delete: %w", err)
				}
			}
		}
	}()
	go func() { // the daily prune
		defer wg.Done()
		for i := 0; i < rounds/3; i++ {
			if _, err := b.PruneAuto(ctx, 30); err != nil {
				errs <- fmt.Errorf("prune: %w", err)
			}
			if _, err := a.QuerySnapshots(ctx, QueryFilter{Limit: 10}); err != nil {
				errs <- fmt.Errorf("query: %w", err)
			}
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
