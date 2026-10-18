package policy

import (
	"context"
	"testing"
	"time"
)

// A verdict the live hook already applied is recorded as enforced: the engine is not
// asked again and the order is not counted a second time.
func TestRecorder_RecordStoresADecidedVerdictWithoutAskingOrCounting(t *testing.T) {
	r := newRig(t, nil)
	r.cfg.TradingRisk.MaxOrdersPer10Min = 1
	store := newStore(t)
	rc := NewRecorder(r.e, store, nil, 0)
	t.Cleanup(func() { rc.Close(time.Second) })

	q := buy(owner(ClassTrade, "create_order"))
	v := Verdict{Mode: ModeDeny, Tier: T3Discretionary, Reasons: []string{"B: rate"}}
	rc.Record(context.Background(), q, v)
	if !rc.Flush(5 * time.Second) {
		t.Fatal("the queue did not drain")
	}
	rows, err := store.Recent(context.Background(), 5)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %+v, %v", rows, err)
	}
	if d := rows[0]; !d.Enforced || d.Mode != ModeDeny || !d.LegacyAllowed || len(d.Reasons) != 1 || d.Reasons[0] != "B: rate" {
		t.Fatalf("row = %+v", d)
	}
	if got := r.e.Enforce(context.Background(), q); got.Mode != ModeAct {
		t.Fatalf("Record counted the order: the first Enforce afterwards is %s %v", got.Mode, got.Reasons)
	}
	rc.Observe(context.Background(), q, false)
	rc.Flush(5 * time.Second)
	rows, _ = store.Recent(context.Background(), 5)
	if len(rows) != 2 || rows[0].Enforced {
		t.Fatalf("a shadow observation is not enforced: %+v", rows)
	}
	counts, err := store.CountsSince(context.Background(), time.Now().Add(-time.Hour))
	if err != nil || counts[ModeDeny] != 2 {
		t.Fatalf("counts = %v, %v", counts, err)
	}
	if none, _ := store.CountsSince(context.Background(), time.Now().Add(time.Hour)); len(none) != 0 {
		t.Fatalf("counts in the future = %v", none)
	}
}
