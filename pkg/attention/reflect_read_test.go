package attention

import (
	"context"
	"testing"
	"time"
)

func TestDecisionsAndMutesBetween(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	start := e.clock.Now()
	d1 := e.send(cand("a", notice))
	e.clock.Add(time.Hour)
	d2 := e.send(cand("b", notice))
	if err := e.g.Mute(ctx, "price:", e.clock.Now().Add(time.Hour), "owner"); err != nil {
		t.Fatal(err)
	}
	if err := e.g.MuteExact(ctx, "price:BTC:above", e.clock.Now().Add(time.Hour), "snooze"); err != nil {
		t.Fatal(err)
	}

	ds, err := e.g.DecisionsBetween(ctx, start, e.clock.Now().Add(time.Minute), 0)
	if err != nil || len(ds) != 2 || ds[0].ID != d1.ID || ds[1].ID != d2.ID {
		t.Fatalf("decisions = %+v, %v", ds, err)
	}
	if !ds[0].Delivered || ds[0].TriggerID == "" || ds[0].Severity != notice {
		t.Errorf("row = %+v", ds[0])
	}
	if ds, _ := e.g.DecisionsBetween(ctx, start, start.Add(30*time.Minute), 0); len(ds) != 1 {
		t.Errorf("the window [start, +30m) holds %d decisions, want 1", len(ds))
	}
	ms, err := e.g.MuteEventsBetween(ctx, start, e.clock.Now().Add(time.Minute))
	if err != nil || len(ms) != 2 {
		t.Fatalf("mutes = %+v, %v", ms, err)
	}
	var exact, prefix int
	for _, m := range ms {
		if m.Exact {
			exact++
			if m.Key != "price:BTC:above" {
				t.Errorf("exact key = %q: the stored marker leaked", m.Key)
			}
		} else {
			prefix++
		}
	}
	if exact != 1 || prefix != 1 {
		t.Errorf("exact %d prefix %d", exact, prefix)
	}
}
