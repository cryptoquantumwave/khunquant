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

// With Options.After set, the housekeeper runs on that timer, not on a wall ticker.
func TestHousekeepLoop_PacedByAfter(t *testing.T) {
	f := newFixture(t)
	src := &fakeChanges{}
	f.fb.changes = src
	fire := make(chan time.Time)
	waits := make(chan time.Duration, 4)
	f.fb.after = func(d time.Duration) <-chan time.Time { waits <- d; return fire }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go f.fb.housekeepLoop(ctx, done)
	if d := <-waits; d != housekeepEvery { // the first pass ran, now it waits on the timer
		t.Fatalf("waited %v, want %v", d, housekeepEvery)
	}
	fire <- time.Time{}
	<-waits // a second pass ran and waits again
	cancel()
	<-done
	if len(src.since) < 2 {
		t.Fatalf("change source read %d times, want a pass per fire", len(src.since))
	}
}
