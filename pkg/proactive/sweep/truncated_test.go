package sweep

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/worldmodel"
)

// A mark that is older than the change log still holds: the log keeps the newest
// worldmodel.MaxLogBuffer entries in memory (and a week in the database), and a quiet
// day does not move the mark. SinceMark then returns what is left, marked Truncated;
// the sweep must not take that for "nothing" or "noise": it asks the model once, with
// the words "older changes were lost", and moves the mark past the gap.
func TestSweep_MarkOlderThanTheChangeLogIsTruncatedAndMaterial(t *testing.T) {
	for name, extra := range map[string]int{"what is left ends where it started": 0, "what is left is a small wobble": 1} {
		t.Run(name, func(t *testing.T) {
			rig := newDayRig(t)
			ctx := context.Background()
			if err := rig.store.EnsureMark(ctx, Mark); err != nil {
				t.Fatal(err)
			}
			seq0, _, _ := rig.store.MarkInfo(Mark)

			// Far more entries than the log keeps, every one a 0.1% wobble of the
			// total: noise by the floor.
			n := worldmodel.MaxLogBuffer + 200 + extra
			for i := 0; i < n; i++ {
				rig.clk.Advance(time.Minute)
				btc := 60000.0
				if i%2 == 1 {
					btc = 60060
				}
				rig.snapshot(btc, 0)
			}

			d, err := rig.store.SinceMark(ctx, Mark)
			if err != nil {
				t.Fatal(err)
			}
			if !d.Truncated {
				t.Fatalf("the diff of a mark older than the log is not truncated: %+v (mark %d, cursor %d)", d, seq0, rig.store.Cursor())
			}
			if m, _ := Material(d); !m.Empty() {
				t.Fatalf("the test needs a diff that is noise or empty on its own: %+v", m)
			}

			turn := &fakeTurn{}
			run := New(Config{World: rig.store, Topics: &fakeTopics{}, Turn: turn.run, Now: rig.clk.Now, State: &State{}})
			res := run.Run(ctx, "hb")
			if res.Outcome != ResultCompleted || turn.calls() != 1 {
				t.Fatalf("result %+v, model calls %d: a truncated diff must reach the model once", res, turn.calls())
			}
			if p := turn.prompt(0); !strings.Contains(p, "older changes were lost") {
				t.Fatalf("the prompt does not say that older changes were lost:\n%s", p)
			}
			if seq, _, _ := rig.store.MarkInfo(Mark); seq != rig.store.Cursor() || seq <= seq0 {
				t.Fatalf("the mark is at %d, want the cursor %d", seq, rig.store.Cursor())
			}

			// The gap is behind the mark: the next sweep sees nothing and asks nobody.
			rig.clk.Advance(30 * time.Minute)
			if res := run.Run(ctx, "hb"); res.Outcome != ResultNoChanges || turn.calls() != 1 {
				t.Fatalf("next sweep = %+v, model calls %d", res, turn.calls())
			}
		})
	}
}
