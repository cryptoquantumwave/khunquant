package feedback

import (
	"context"
	"testing"
	"time"
)

type fakeChanges struct {
	list  []Change
	since []time.Time
}

func (f *fakeChanges) ChangesSince(_ context.Context, since time.Time) ([]Change, error) {
	f.since = append(f.since, since)
	out := f.list
	f.list = nil
	return out, nil
}

// The housekeeper reads the change source and records what it reports as acted_on,
// asking for the changes since its last look.
func TestHousekeepRecordsChangesFromTheSource(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	id, _ := f.deliver("BTC/USDT", "price:btc")
	src := &fakeChanges{}
	f.fb.changes = src
	start := f.clock.Now()
	f.fb.polled = start

	f.clock.Add(time.Hour)
	src.list = []Change{{Source: ChangeIntent, Subject: "BTC/USDT", Event: "e1", At: f.clock.Now()}}
	if _, err := f.fb.Housekeep(ctx); err != nil {
		t.Fatal(err)
	}
	f.clock.Add(time.Hour)
	if _, err := f.fb.Housekeep(ctx); err != nil {
		t.Fatal(err)
	}
	rows, _ := f.fb.Responses(ctx, id)
	if len(rows) != 1 || rows[0].Kind != string(KindActedOn) || rows[0].Source != ChangeIntent {
		t.Fatalf("rows = %+v, want one acted_on from %s", rows, ChangeIntent)
	}
	if len(src.since) != 2 || !src.since[0].Equal(start) || !src.since[1].Equal(start.Add(time.Hour)) {
		t.Fatalf("asked for changes since %v, want %v then one hour later", src.since, start)
	}
}
