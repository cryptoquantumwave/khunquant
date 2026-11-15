package journal

import (
	"context"
	"testing"
	"time"
)

// List filters by status and age, newest first, bounded; StepKey is the key a step
// sends with its side effect.
func TestList_FiltersByStatusAndAge(t *testing.T) {
	ctx := context.Background()
	n := newNode(t, dbPath(t), t0, []Definition{fiveSteps(newLedger(), newExchange())})

	old := mustStart(t, n.Engine, "five", nil)
	n.clock.Advance(48 * time.Hour)
	mid := mustStart(t, n.Engine, "five", nil)
	n.clock.Advance(time.Hour)
	recent := mustStart(t, n.Engine, "five", nil)
	if err := n.Cancel(ctx, mid, "test"); err != nil {
		t.Fatal(err)
	}

	ids := func(ws []Workflow) []string {
		var out []string
		for _, w := range ws {
			out = append(out, w.ID)
		}
		return out
	}
	all, err := n.List(ctx, ListFilter{})
	if err != nil || len(all) != 3 || all[0].ID != recent || all[2].ID != old {
		t.Fatalf("all = %v, %v; want newest first", ids(all), err)
	}
	active, err := n.List(ctx, ListFilter{Statuses: ActiveStatuses})
	if err != nil || len(active) != 2 {
		t.Fatalf("active = %v, %v", ids(active), err)
	}
	fresh, err := n.List(ctx, ListFilter{Since: t0.Add(24 * time.Hour)})
	if err != nil || len(fresh) != 2 {
		t.Fatalf("since = %v, %v", ids(fresh), err)
	}
	if one, err := n.List(ctx, ListFilter{Limit: 1}); err != nil || len(one) != 1 || one[0].ID != recent {
		t.Fatalf("limit = %v, %v", ids(one), err)
	}
	if cancelled, err := n.List(ctx, ListFilter{Statuses: []Status{StatusCancelled}}); err != nil || len(cancelled) != 1 || cancelled[0].ID != mid {
		t.Fatalf("cancelled = %v, %v", ids(cancelled), err)
	}
	if got := StepKey("wf_x", 3, "place"); got != "wf_x:3:place" {
		t.Errorf("StepKey = %q", got)
	}
}

// A Kind filter is part of the query, so Limit counts only workflows of that kind: a kind
// older than Limit newer workflows of another kind is still found.
func TestList_KindIsFilteredBeforeTheLimit(t *testing.T) {
	ctx := context.Background()
	other := fiveSteps(newLedger(), newExchange())
	other.Kind = "other"
	n := newNode(t, dbPath(t), t0, []Definition{fiveSteps(newLedger(), newExchange()), other})

	first := mustStart(t, n.Engine, "five", nil)
	for range 3 {
		n.clock.Advance(time.Minute)
		mustStart(t, n.Engine, "other", nil)
	}
	got, err := n.List(ctx, ListFilter{Kind: "five", Limit: 2})
	if err != nil || len(got) != 1 || got[0].ID != first {
		t.Fatalf("Kind five, Limit 2 = %v, %v; want the one workflow behind three newer ones", got, err)
	}
	if got, err := n.List(ctx, ListFilter{Limit: 2}); err != nil || len(got) != 2 || got[0].Kind != "other" {
		t.Fatalf("no kind, Limit 2 = %v, %v", got, err)
	}
	if got, err := n.List(ctx, ListFilter{Kind: "nope"}); err != nil || len(got) != 0 {
		t.Fatalf("an unknown kind = %v, %v", got, err)
	}
}
