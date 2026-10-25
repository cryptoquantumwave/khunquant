package deltaneutral

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/cryptoquantumwave/khunquant/pkg/journal"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// FlowOrders is the kill switch's view of a dn.open workflow: the leg orders its
// place steps placed, and whether the workflow may be cancelled. A workflow of
// another kind answers the zero value.
//
// Once a place step has BEGUN (finished or not) the workflow is Settle: it is not
// cancelled. A cancel at the next boundary would skip finalize, which is the only
// step that settles the execution and starts the protective dn.recover, and so could
// leave a filled leg unhedged with nothing working on it. Left to run, a place step
// reads the kill flag on every attempt and refuses, which sends the workflow to
// finalize (and, with a leg open, to dn.recover). Before the first place step
// nothing is open and the workflow may be cancelled.
//
// It never makes an exchange call.
func (d *Deps) FlowOrders(ctx context.Context, w journal.Workflow, steps []journal.StepRecord) (broker.FlowOrders, error) {
	if w.Kind == KindResize {
		// Protective, but not plainly "continuing": an increase refuses at its next place
		// step while the kill flag is engaged (and ends recovery_required if cut between
		// legs); a decrease only reduces exposure.
		fo := broker.FlowOrders{Protect: true, Note: "will refuse any risk-increasing step"}
		var in ResizeInput
		if err := json.Unmarshal(w.Input, &in); err == nil {
			if in.decrease() {
				fo.Note = "decrease: reduces exposure"
			} else {
				fo.Note = "increase: refuses at its next place step"
			}
		}
		return fo, nil
	}
	if w.Kind != KindOpen {
		return broker.FlowOrders{}, nil
	}
	var in OpenInput
	if err := json.Unmarshal(w.Input, &in); err != nil || in.PlanID <= 0 {
		return broker.FlowOrders{}, errors.New("dn.open: bad input")
	}
	var fo broker.FlowOrders
	var plan *Plan
	for _, rec := range steps {
		if rec.Name != "place_first_leg" && rec.Name != "place_second_leg" {
			continue
		}
		fo.Settle = true
		if rec.FinishedAt.IsZero() || rec.Error != "" || rec.Output == nil {
			continue // dn.recover reads the exchange for a leg whose outcome is unknown
		}
		var po placeOut
		if err := json.Unmarshal(rec.Output, &po); err != nil {
			return fo, err
		}
		if po.Outcome != placedOK {
			continue
		}
		if plan == nil {
			p, err := d.Store.GetPlan(ctx, in.PlanID)
			if err != nil {
				return fo, err
			}
			plan = p
		}
		ref := broker.OrderRef{WorkflowID: w.ID, Kind: w.Kind, OrderID: po.Order.ID, Done: po.Order.Done, Side: po.Leg.Side}
		if po.Leg.Leg == LegTypeFutures {
			ref.Provider, ref.Account, ref.Symbol, ref.Market = plan.FuturesProvider, plan.FuturesAccount, plan.FuturesSymbol, broker.OrderMarketFutures
		} else {
			ref.Provider, ref.Account, ref.Symbol, ref.Market = plan.SpotProvider, plan.SpotAccount, plan.SpotSymbol, broker.OrderMarketSpot
		}
		fo.Orders = append(fo.Orders, ref)
	}
	return fo, nil
}
