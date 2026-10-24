package sources_test

import (
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/perception/sources"
)

// While the cron job still runs next to the source, the job saves the snapshots; the
// source saves them only when told the triggers are live.
func TestDNPlans_SavesSnapshotsOnlyWhenToldTo(t *testing.T) {
	var live bool
	var mu sync.Mutex
	r := newDNRig(t, func(o *sources.DNPlansOptions) {
		o.SaveSnapshots = func() bool { mu.Lock(); defer mu.Unlock(); return live }
	}, dnPlan(1, "5m"))
	advance := r.run(t)
	waitFor(t, "the first event", func() bool { return len(r.out.events()) == 1 })
	if r.store.saved() != 0 {
		t.Fatalf("%d snapshots saved in shadow", r.store.saved())
	}
	mu.Lock()
	live = true
	mu.Unlock()
	advance(5 * time.Minute)
	waitFor(t, "the second event", func() bool { return len(r.out.events()) == 2 })
	if r.store.saved() != 1 {
		t.Fatalf("%d snapshots saved when live, want 1", r.store.saved())
	}
}
