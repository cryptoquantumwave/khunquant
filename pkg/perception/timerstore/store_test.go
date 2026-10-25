package timerstore

import (
	"context"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex/sqlitextest"
)

func TestStore_RoundTripReplaceAndDelete(t *testing.T) {
	ctx := context.Background()
	db := sqlitextest.Open(t)
	s, err := New(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(ctx, db); err != nil {
		t.Fatalf("a second open: %v", err)
	}
	at := time.Date(2026, 10, 16, 2, 0, 0, 0, time.UTC)
	one := perception.Timer{ID: "one", At: at}
	rec := perception.Timer{ID: "rec", At: at, Cron: "0 9 * * 5", TZ: "Asia/Bangkok", Until: at.Add(72 * time.Hour), Budget: 3, Fired: 1}
	for _, tm := range []perception.Timer{one, rec} {
		if err := s.Save(ctx, tm); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Load(ctx)
	if err != nil || len(got) != 2 {
		t.Fatalf("Load = %+v, %v", got, err)
	}
	if got[0].ID != "one" || !got[0].At.Equal(at) || got[0].Recurring() {
		t.Fatalf("one-shot = %+v", got[0])
	}
	if g := got[1]; g.Cron != rec.Cron || g.TZ != rec.TZ || !g.Until.Equal(rec.Until) || g.Budget != 3 || g.Fired != 1 || !g.At.Equal(at) {
		t.Fatalf("recurring = %+v", g)
	}
	// A save with the same id replaces.
	one.At = at.Add(time.Hour)
	if err := s.Save(ctx, one); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Load(ctx)
	if len(got) != 2 || !got[0].At.Equal(one.At) {
		t.Fatalf("after replace: %+v", got)
	}
	if err := s.Delete(ctx, "one"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "never-was"); err != nil {
		t.Fatalf("deleting nothing: %v", err)
	}
	if got, _ = s.Load(ctx); len(got) != 1 || got[0].ID != "rec" {
		t.Fatalf("after delete: %+v", got)
	}
}

func TestStore_NilDatabase(t *testing.T) {
	if _, err := New(context.Background(), nil); err == nil {
		t.Fatal("a nil database was accepted")
	}
}

func TestStore_NoCrossPackageCallsInTransaction(t *testing.T) {
	// The store opens no transaction; the guard checks nothing here calls out while
	// one is held.
	sqlitextest.AssertNoCrossPackageCallsInTx(t, "pkg/perception/timerstore", "pkg/triggers/intents")
}
