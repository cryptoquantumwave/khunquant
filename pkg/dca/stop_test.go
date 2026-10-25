package dca

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/journal"
)

func stepRec(t *testing.T, seq int, name string, out any, finished bool, errText string) journal.StepRecord {
	t.Helper()
	r := journal.StepRecord{Seq: seq, Name: name, Error: errText, StartedAt: time.Unix(1000, 0), FirstStartedAt: time.Unix(1000, 0)}
	if out != nil {
		b, err := json.Marshal(out)
		if err != nil {
			t.Fatal(err)
		}
		r.Output = b
	}
	if finished {
		r.FinishedAt = time.Unix(1001, 0)
	}
	return r
}

// FlowOrders describes a dca.execute workflow to the kill switch: nothing before the
// place step, a lookup description for a cut one, and the order for a finished one
// (which settles instead of being cancelled).
func TestFlowOrders_DCAExecute(t *testing.T) {
	st, planID := storeWithPlan(t)
	d := &Deps{Store: st}
	in, _ := json.Marshal(ExecInput{PlanID: planID, DueMS: 1, MandateID: "m1"})
	wf := journal.Workflow{ID: "dcax_1_1", Kind: KindExecute, Input: in}
	chk := stepRec(t, 0, "check_mandate", checkOut{Verdict: verdictOK, Quote: Quote{BaseAmount: 0.001, USD: 50}}, true, "")
	ctx := context.Background()

	if fo, err := d.FlowOrders(ctx, wf, []journal.StepRecord{chk}); err != nil || fo.Settle || len(fo.Orders) != 0 {
		t.Errorf("before the place step: %+v, %v", fo, err)
	}
	if fo, err := d.FlowOrders(ctx, journal.Workflow{Kind: "other"}, nil); err != nil || fo.Settle || len(fo.Orders) != 0 {
		t.Errorf("another kind: %+v, %v", fo, err)
	}

	t.Run("cut", func(t *testing.T) {
		fo, err := d.FlowOrders(ctx, wf, []journal.StepRecord{chk, stepRec(t, 3, "place", nil, false, "")})
		if err != nil || fo.Settle || len(fo.Orders) != 1 {
			t.Fatalf("%+v, %v", fo, err)
		}
		r := fo.Orders[0]
		if r.OrderID != "" || r.Request == nil || r.Request.Amount != 0.001 || r.Request.Side != "buy" || r.Request.Type != "market" ||
			r.IdemKey != "dcax_1_1:3:place" || r.Symbol != "BTC/THB" || r.Provider != "bitkub" || r.Exit {
			t.Errorf("ref = %+v", r)
		}
	})
	t.Run("placed", func(t *testing.T) {
		out := placeOut{Outcome: placedOK, Fill: Fill{OrderID: "o1", Done: false}}
		fo, err := d.FlowOrders(ctx, wf, []journal.StepRecord{chk, stepRec(t, 3, "place", out, true, "")})
		if err != nil || !fo.Settle || len(fo.Orders) != 1 || fo.Orders[0].OrderID != "o1" || fo.Orders[0].Done {
			t.Errorf("%+v, %v", fo, err)
		}
	})
	t.Run("the verify step's fill says it is over", func(t *testing.T) {
		out := placeOut{Outcome: placedOK, Fill: Fill{OrderID: "o1"}}
		verify := stepRec(t, 4, "verify", Fill{OrderID: "o1", Known: true, Filled: 0.001, Done: true}, true, "")
		fo, err := d.FlowOrders(ctx, wf, []journal.StepRecord{chk, stepRec(t, 3, "place", out, true, ""), verify})
		if err != nil || len(fo.Orders) != 1 || !fo.Orders[0].Done {
			t.Errorf("%+v, %v", fo, err)
		}
	})
	t.Run("refused places nothing", func(t *testing.T) {
		out := placeOut{Outcome: placeRefused, Reason: "kill"}
		fo, err := d.FlowOrders(ctx, wf, []journal.StepRecord{chk, stepRec(t, 3, "place", out, true, "")})
		if err != nil || !fo.Settle || len(fo.Orders) != 0 {
			t.Errorf("%+v, %v", fo, err)
		}
	})
	t.Run("unknown outcome is looked up", func(t *testing.T) {
		out := placeOut{Outcome: placeUnknown}
		fo, err := d.FlowOrders(ctx, wf, []journal.StepRecord{chk, stepRec(t, 3, "place", out, true, "")})
		if err != nil || !fo.Settle || len(fo.Orders) != 1 || fo.Orders[0].Request == nil {
			t.Errorf("%+v, %v", fo, err)
		}
	})
}
