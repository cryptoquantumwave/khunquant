package approval

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/journal"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// Cancelled names a proposal CancelPending cancelled.
type Cancelled struct {
	Code string
	Kind Kind
}

// CancelPending cancels every pending proposal, whatever it proposes (an order that
// only reduces risk included: the owner can approve a new one after /resume, and
// approving a reducing order while the kill switch is engaged is allowed anyway),
// and edits each delivered card to say so. It is the kill switch's step for
// proposals. A proposal that is no longer pending (decided in the meantime) is not
// touched. A failure on one proposal does not stop the others; the first error is
// returned with what was cancelled.
func (s *Service) CancelPending(ctx context.Context, reason string) ([]Cancelled, error) {
	ctx, cancel := context.WithTimeout(ctx, storeTimeout)
	defer cancel()
	pending, err := s.st.list(ctx, `status = 'pending' ORDER BY created_at_ms LIMIT 200`)
	if err != nil {
		return nil, err
	}
	var (
		out  []Cancelled
		errs []error
	)
	for _, p := range pending {
		changed, err := s.st.cancel(ctx, p.ID, reason, s.now())
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !changed {
			continue
		}
		p.Status, p.Outcome = StatusCancelled, reason
		s.editCard(ctx, p, OutcomeText(p.Lang, StatusCancelled))
		out = append(out, Cancelled{Code: p.Code, Kind: p.Kind})
	}
	if len(out) > 0 {
		logger.InfoCF(logComponent, "Pending proposals cancelled", map[string]any{"count": len(out), "reason": reason})
	}
	return out, errors.Join(errs...)
}

// FlowOrders is the kill switch's view of an approval.order workflow: the order its
// place step placed, or may have placed, and whether the workflow may be cancelled.
// A workflow of another kind answers the zero value.
//
//   - A proposal that only reduces risk is Protect: the kill switch lets its
//     workflow run, because it is getting the owner out.
//   - A place step that finished is Settle: the workflow goes on to verify, record
//     and tell the owner, and places nothing more.
//   - A place step started and not finished (or finished in an error that says the
//     order may exist) is a cut one: the workflow may be cancelled, and the order is
//     described so that it can be looked up by its client order ID.
//
// An error means the proposal could not be read; the caller treats the workflow as
// one that adds risk.
func (s *Service) FlowOrders(ctx context.Context, w journal.Workflow, steps []journal.StepRecord) (broker.FlowOrders, error) {
	if w.Kind != WorkflowKind {
		return broker.FlowOrders{}, nil
	}
	var in workflowInput
	if err := json.Unmarshal(w.Input, &in); err != nil || in.ProposalID == "" {
		return broker.FlowOrders{}, errors.New("approval: the workflow input is not a proposal id")
	}
	p, err := s.st.get(ctx, in.ProposalID)
	if err != nil {
		return broker.FlowOrders{}, err
	}
	fo := broker.FlowOrders{Protect: p.Reducing}
	op, err := decodeVerified(p)
	if err != nil {
		return fo, err
	}
	for _, rec := range steps {
		if rec.Name != "place" {
			continue
		}
		ref := broker.OrderRef{
			WorkflowID: w.ID, Kind: w.Kind, Provider: op.Provider, Account: op.Account,
			Market: op.Market, Symbol: op.Symbol, Side: op.Side,
			Exit: p.Reducing || op.ReduceOnly, Paper: p.Mode == ModePaper,
		}
		finished := !rec.FinishedAt.IsZero()
		switch {
		case finished && rec.Error == "":
			var placed Placed
			if err := json.Unmarshal(rec.Output, &placed); err != nil {
				return fo, err
			}
			ref.OrderID, ref.Done = placed.OrderID, placed.Fill.Done || verifiedDone(steps)
			fo.Settle = true
		case finished && !strings.Contains(rec.Error, tagMaybe):
			return fo, nil // failed for good and nothing was placed
		default:
			ref.Request = &broker.OrderRequest{
				Market: op.Market, Symbol: op.Symbol, Type: op.Type, Side: op.Side, Amount: op.Amount, Price: op.Price,
				ReduceOnly: op.ReduceOnly, MarginMode: op.MarginMode, PositionSide: op.PositionSide,
			}
			ref.IdemKey = journal.StepKey(w.ID, rec.Seq, rec.Name)
			ref.FirstStartedAt = rec.FirstStartedAt
		}
		fo.Orders = append(fo.Orders, ref)
	}
	return fo, nil
}

// verifiedDone reports whether the verify step read the order back as over. The
// answer to a placement often says nothing about the status, and without this every
// order of the last week would be looked up on each /stop.
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
