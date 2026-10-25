package tools

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

// OrderProposer is what propose_order needs from the approval service.
// *approval.Service satisfies it.
type OrderProposer interface {
	Propose(ctx context.Context, r approval.ProposeRequest) (approval.Proposal, error)
}

// proposerHolder is the late-bound approval service. The tool is registered with
// the agent loop, which exists before the gateway builds the service (the service
// needs the journal engine, the notify service and the channels); the gateway sets
// it on every build and reload and clears it when the services stop.
var proposerHolder atomic.Pointer[OrderProposer]

// SetOrderProposer attaches the approval service to propose_order; nil detaches it.
func SetOrderProposer(p OrderProposer) {
	if p == nil {
		proposerHolder.Store(nil)
		return
	}
	proposerHolder.Store(&p)
}

func currentProposer() OrderProposer {
	if p := proposerHolder.Load(); p != nil {
		return *p
	}
	return nil
}

// ProposeOrderTool suggests an order to the owner. It places nothing: it stores a
// proposal whose parameters are fixed by code, sends the owner a card built from
// them, and the order is placed only if the owner approves exactly that card.
type ProposeOrderTool struct{ cfg *config.Config }

// NewProposeOrderTool builds the tool. It is registered only while approvals are
// on; cfg is read on every call.
func NewProposeOrderTool(cfg *config.Config) *ProposeOrderTool { return &ProposeOrderTool{cfg: cfg} }

func (t *ProposeOrderTool) Name() string { return NameProposeOrder }

func (t *ProposeOrderTool) Description() string {
	return "Suggest an order to the owner for approval. Nothing is placed: the owner receives a card built by the system from " +
		"these parameters with Approve and Reject buttons, and the order is placed only if they approve it before it expires. " +
		"The system fills in the reference price, the USD value and the safety checks; do not describe the order in the rationale, " +
		"which is optional, one plain sentence, and is shown under a Why label. Futures sizes are given as notional_usd (the " +
		"system computes the contracts). There is no confirm flag and no extra order params. A proposal made from a background " +
		"turn can only reduce risk."
}

func (t *ProposeOrderTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"provider": map[string]any{"type": "string", "description": "Provider/exchange name."},
			"account":  map[string]any{"type": "string", "description": "Account name (empty = default)."},
			"market": map[string]any{
				"type": "string", "enum": []string{"spot", "futures"},
				"description": "spot (default) or futures (a perpetual swap symbol such as BTC/USDT:USDT).",
			},
			"symbol": map[string]any{"type": "string", "description": "Trading pair, e.g. 'BTC/USDT' or 'BTC/USDT:USDT'."},
			"side":   map[string]any{"type": "string", "enum": []string{"buy", "sell"}, "description": "Order side."},
			"type": map[string]any{
				"type": "string", "enum": []string{"market", "limit"}, "description": "Order type (default market).",
			},
			"amount":       map[string]any{"type": "number", "description": "Spot only: quantity in base currency units."},
			"notional_usd": map[string]any{"type": "number", "description": "Futures only: the order's size in USD; contracts are computed by the system."},
			"price":        map[string]any{"type": "number", "description": "Limit price (limit orders only)."},
			"reduce_only": map[string]any{
				"type": "boolean", "description": "Futures only: the order can only reduce an existing position.",
			},
			"rationale": map[string]any{
				"type": "string", "maxLength": approval.MaxRationaleRunes,
				"description": "Optional. One plain sentence on why, at most 300 characters. Shown under 'Why'; never executed.",
			},
		},
		"required": []string{"provider", "symbol", "side"},
	}
}

func (t *ProposeOrderTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	svc := currentProposer()
	if svc == nil {
		return ErrorResult("propose_order is not available: order approvals are not running")
	}
	// The same predicate that hides the definition. A turn that could not see the
	// tool cannot use it by naming it.
	if !proposeOrderOpen(ctx) {
		return ErrorResult("propose_order is not available in this turn")
	}
	req, perr := t.requestFor(ctx)
	if perr != nil {
		return ErrorResult(perr.Error())
	}
	if _, has := args["params"]; has {
		return ErrorResult("propose_order takes no params argument: the order's parameters are fixed by the system")
	}
	if _, has := args["confirm"]; has {
		return ErrorResult("propose_order has no confirm flag: the owner's approval is the confirmation")
	}

	order := approval.OrderRequest{
		Provider: stringArg(args, "provider"), Account: stringArg(args, "account"),
		Market: stringArg(args, "market"), Symbol: stringArg(args, "symbol"),
		Side: stringArg(args, "side"), Type: stringArg(args, "type"),
		Amount: numberArg(args, "amount"), NotionalUSD: numberArg(args, "notional_usd"),
	}
	if v, ok := args["price"].(float64); ok {
		order.Price = &v
	}
	order.ReduceOnly, _ = args["reduce_only"].(bool)
	req.Order = order
	req.Rationale = stringArg(args, "rationale")

	p, err := svc.Propose(ctx, req)
	if err != nil {
		return ErrorResult(refusalMessage(err)).WithError(err)
	}
	if s := ProposalScopeFrom(ctx); s != nil {
		s.NoteCreated(p.Code)
	}
	return SilentResult(fmt.Sprintf("Proposal %s sent for approval (expires %s). Nothing has been placed; the owner decides.",
		p.Code, p.ExpiresAt.Format("15:04 MST")))
}

// requestFor reads the facts of the turn from its context: who it is and, for a
// background turn, the trust and delivery decision of its candidates. None of it
// comes from an argument.
func (t *ProposeOrderTool) requestFor(ctx context.Context) (approval.ProposeRequest, error) {
	if ToolOrigin(ctx) == OriginProactive {
		s := ProposalScopeFrom(ctx)
		if s == nil {
			return approval.ProposeRequest{}, errors.New("propose_order is not available in this turn")
		}
		return approval.ProposeRequest{
			Origin: approval.OriginProactive, Trust: s.Trust(), DeliverNow: s.deliverNow,
			CandidateID: s.candidateID, InterventionID: s.intervention,
		}, nil
	}
	if _, v := checkOwnerTurn(ctx, t.cfg, true); v != ownerYes {
		return approval.ProposeRequest{}, errors.New("propose_order is available only in the owner's own conversation")
	}
	return approval.ProposeRequest{Origin: approval.OriginOwner}, nil
}

// refusalMessage words a Propose error for the model: what failed, so that it can
// say so to the owner, and nothing about how to get around it.
func refusalMessage(err error) string {
	var pending *approval.PendingError
	switch {
	case errors.As(err, &pending):
		return err.Error()
	case errors.Is(err, approval.ErrNotRiskReducing):
		return "refused: a proposal from a background turn can only reduce risk (sell what the account holds, or a reduce-only futures order); nothing was proposed"
	case errors.Is(err, approval.ErrUntrustedOrigin), errors.Is(err, approval.ErrNotDeliverNow):
		return "refused: this turn's inputs do not allow proposing an order; nothing was proposed"
	}
	return "refused: " + err.Error()
}
