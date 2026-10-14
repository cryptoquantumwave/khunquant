package control

import (
	"context"
	"errors"
	"fmt"

	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
)

const approvalsOffText = "Order approvals are not available: proactive.approvals is off or the workflow engine is not running."

func isNotFound(err error) bool { return errors.Is(err, approval.ErrNotFound) }

// cmdApprove is "/approve CODE" (and the Approve button).
func (d *Dispatcher) cmdApprove(ctx context.Context, in Input) (Reply, error) {
	return d.decideCommand(ctx, in, true), nil
}

// cmdReject is "/reject CODE" (and the Reject button).
func (d *Dispatcher) cmdReject(ctx context.Context, in Input) (Reply, error) {
	return d.decideCommand(ctx, in, false), nil
}

// cmdYes is a reply to an approval card that says "yes": TryHandle has already
// established that it points at a pending card, and put that card's code in the
// command.
func (d *Dispatcher) cmdYes(ctx context.Context, in Input) (Reply, error) {
	return d.decideCommand(ctx, in, true), nil
}

func (d *Dispatcher) decideCommand(ctx context.Context, in Input, approve bool) Reply {
	st := stateOf(ctx)
	verb := "/reject"
	if approve {
		verb = "/approve"
	}
	if len(st.cmd.args) != 1 || !approval.ValidCode(approval.NormalizeCode(st.cmd.args[0])) {
		setOutcome(ctx, outcomeUsage)
		return Reply{Text: "Usage: " + verb + " CODE (the 4-character code on the proposal)."}
	}
	if st.deps.Approvals == nil {
		setOutcome(ctx, outcomeOff)
		return Reply{Text: approvalsOffText}
	}
	return d.decideProposal(ctx, st.deps, in, approval.NormalizeCode(st.cmd.args[0]), st.cmd.nonce, approve)
}

// decideProposal is the control path's one call of approval.Service.Decide: the owner's
// decision on a proposal, from a message that did not go through a model. The
// service checks the owner again, and the proposal's own conditions (pending,
// unexpired, the right nonce when there is one, the kill switch).
func (d *Dispatcher) decideProposal(ctx context.Context, deps *Deps, in Input, code, nonce string, approve bool) Reply {
	p, err := deps.Approvals.Decide(ctx, code, nonce, approve, canonicalSender(in))
	var se *approval.StatusError
	switch {
	case err == nil:
		return Reply{Text: decidedText(p, approve)}
	case errors.Is(err, approval.ErrNotOwner):
		setOutcome(ctx, outcomeDenied)
		return Reply{Text: notAuthorizedText}
	case errors.Is(err, approval.ErrKilled):
		setOutcome(ctx, outcomeRefused)
		return Reply{Text: fmt.Sprintf("Refused: the kill switch is engaged (/stop), and proposal %s does not only "+
			"reduce risk. Only an order that reduces risk can be approved now. Send /resume to allow approvals "+
			"again, or /reject %s.", code, code)}
	case isNotFound(err):
		setOutcome(ctx, outcomeNotFound)
		return Reply{Text: fmt.Sprintf("No pending proposal %s. It may have expired or been decided already.", code)}
	case errors.As(err, &se):
		setOutcome(ctx, outcomeRefused)
		return Reply{Text: fmt.Sprintf("Proposal %s is already %s.", se.Code, se.Status)}
	}
	logger.WarnCF("control", "A decision on a proposal failed", map[string]any{"error": err.Error()})
	setOutcome(ctx, outcomeError)
	return Reply{Text: failedText}
}

func decidedText(p approval.Proposal, approve bool) string {
	summary := firstLine(p.Summary, 200)
	if !approve {
		return fmt.Sprintf("Rejected proposal %s: %s\nNothing was placed.", p.Code, summary)
	}
	switch p.Status {
	case approval.StatusExecuted:
		return fmt.Sprintf("Approved proposal %s: %s\nIt has been executed; the receipt follows.", p.Code, summary)
	case approval.StatusFailed:
		return fmt.Sprintf("Approved proposal %s: %s\nIt did not go through; the message that follows says why.", p.Code, summary)
	}
	return fmt.Sprintf("Approved proposal %s: %s\nPlacing the order now; you will get a receipt here.", p.Code, summary)
}
