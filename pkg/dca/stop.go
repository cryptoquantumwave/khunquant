package dca

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/cryptoquantumwave/khunquant/pkg/journal"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// FlowOrders is the kill switch's view of a dca.execute workflow: the buy its place
// step placed, or may have placed, and whether the workflow may be cancelled. A
// workflow of another kind answers the zero value.
//
//   - A place step that finished is Settle: the workflow goes on to verify and
//     record the run (so the period's usage is counted) and places nothing more,
//     because every place attempt reads the kill flag.
//   - A place step started and not finished is a cut one: the workflow may be
//     cancelled, and the buy is described so that it can be looked up by its client
//     order ID.
//   - A step that finished saying the order may exist is described the same way.
//
// It never makes an exchange call.
func (d *Deps) FlowOrders(ctx context.Context, w journal.Workflow, steps []journal.StepRecord) (broker.FlowOrders, error) {
	if w.Kind != KindExecute {
		return broker.FlowOrders{}, nil
	}
	var in ExecInput
	if err := json.Unmarshal(w.Input, &in); err != nil || in.PlanID <= 0 {
		return broker.FlowOrders{}, errors.New("dca.execute: bad input")
	}
	var (
		chk   checkOut
		place *journal.StepRecord
	)
	for i := range steps {
		switch steps[i].Name {
		case "check_mandate":
			if steps[i].Output != nil {
				if err := json.Unmarshal(steps[i].Output, &chk); err != nil {
					return broker.FlowOrders{}, err
				}
			}
		case "place":
			place = &steps[i]
		}
	}
	if place == nil {
		return broker.FlowOrders{}, nil
	}
	plan, err := d.Store.GetPlan(ctx, in.PlanID)
	if err != nil {
		return broker.FlowOrders{}, err
	}
	ref := broker.OrderRef{
		WorkflowID: w.ID, Kind: w.Kind, Provider: plan.Provider, Account: plan.Account,
		Market: broker.OrderMarketSpot, Symbol: plan.Symbol, Side: "buy", Paper: chk.Quote.Paper,
	}
	lookup := func() {
		ref.Request = &broker.OrderRequest{Market: broker.OrderMarketSpot, Symbol: plan.Symbol, Type: "market", Side: "buy", Amount: chk.Quote.BaseAmount}
		ref.IdemKey = journal.StepKey(w.ID, place.Seq, place.Name)
		ref.FirstStartedAt = place.FirstStartedAt
	}
	fo := broker.FlowOrders{}
	switch {
	case place.FinishedAt.IsZero(): // cut
		lookup()
	case place.Error != "": // retries ran out on an error that said the order may exist
		fo.Settle = true
		lookup()
	default:
		fo.Settle = true
		var po placeOut
		if err := json.Unmarshal(place.Output, &po); err != nil {
			return fo, err
		}
		switch po.Outcome {
		case placedOK:
			ref.OrderID, ref.Done = po.Fill.OrderID, po.Fill.Done || verifiedDone(steps)
		case placeUnknown:
			lookup()
		default:
			return fo, nil // refused: nothing was placed
		}
	}
	fo.Orders = []broker.OrderRef{ref}
	return fo, nil
}

// verifiedDone reports whether the verify step read the order back as over (the
// answer to a placement often says nothing about the status).
func verifiedDone(steps []journal.StepRecord) bool {
	for _, rec := range steps {
		if rec.Name != "verify" || rec.FinishedAt.IsZero() || rec.Error != "" || rec.Output == nil {
			continue
		}
		var f Fill
		return json.Unmarshal(rec.Output, &f) == nil && f.Done
	}
	return false
}
