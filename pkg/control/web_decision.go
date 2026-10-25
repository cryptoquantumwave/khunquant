package control

import (
	"context"
	"errors"

	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

// The web launcher's approve and reject buttons.
//
// The launcher proxies a click to the gateway, whose route calls DecideFromLauncher.
// The click is treated exactly as a message typed in the authenticated dashboard
// chat would be: the same input, the same web-owner test (config.IsWebOwnerTurn, with
// every condition checked now), and the same decideProposal, which stays the one
// place that calls the approval service's Decide and sets the web marker. The service
// then re-proves the decider, holds an order approval while the kill switch is
// engaged, and applies the expiry and hash checks as it does for a chat /approve.

// launcherSender is who the dashboard chat is: the pico channel's own user. It is not
// the configured owner, so a decision from it counts only through the web route, and
// is recorded as "web".
const (
	launcherSender = "pico:pico-user"
	launcherChat   = "pico:launcher"
)

// LauncherResult says what became of a decision from the launcher.
type LauncherResult string

const (
	// LauncherDecided: the proposal was approved or rejected.
	LauncherDecided LauncherResult = "decided"
	// LauncherNotOwner: the web is not the owner now (a condition of the web route
	// does not hold); nothing was decided.
	LauncherNotOwner LauncherResult = "not_owner"
	// LauncherHeld: the kill switch is engaged and the proposal does not only reduce
	// risk; it stays pending.
	LauncherHeld LauncherResult = "held"
	// LauncherNotFound: no pending, unexpired proposal has that code.
	LauncherNotFound LauncherResult = "not_found"
	// LauncherSettled: the proposal was decided or ended already.
	LauncherSettled LauncherResult = "settled"
	// LauncherUnavailable: approvals are off, or the workflow engine is not running.
	LauncherUnavailable LauncherResult = "unavailable"
	// LauncherBadCode: the code is not a proposal code.
	LauncherBadCode LauncherResult = "bad_code"
	// LauncherFailed: the decision could not be recorded.
	LauncherFailed LauncherResult = "failed"
)

// LauncherDecision is the answer. Text is the same sentence the chat path replies
// with; Status is the proposal's status after a decision, or the status it already
// had for LauncherSettled.
type LauncherDecision struct {
	Result LauncherResult
	Text   string
	Status string
}

type decisionKey struct{}

// decisionSlot receives the approval service's answer from decideProposal.
type decisionSlot struct {
	p   approval.Proposal
	err error
}

// noteDecision hands the service's answer to a caller that asked for it (see
// DecideFromLauncher); for any other caller it does nothing.
func noteDecision(ctx context.Context, p approval.Proposal, err error) {
	if s, _ := ctx.Value(decisionKey{}).(*decisionSlot); s != nil {
		s.p, s.err = p, err
	}
}

// DecideFromLauncher approves or rejects the pending proposal that holds code, as
// the owner acting through the authenticated web launcher. It takes no sender and
// no nonce from its caller: the identity is the dashboard chat's, and it counts
// only while cfg.WebOwnerActive() holds, which is checked here and again by the
// approval service.
func (d *Dispatcher) DecideFromLauncher(ctx context.Context, code string, approve bool) LauncherDecision {
	deps := d.deps.Load()
	code = approval.NormalizeCode(code)
	if !approval.ValidCode(code) {
		return LauncherDecision{Result: LauncherBadCode, Text: "That is not a proposal code."}
	}
	if deps.Approvals == nil {
		return LauncherDecision{Result: LauncherUnavailable, Text: approvalsOffText}
	}
	in := Input{
		Channel: config.WebChannel, ChatID: launcherChat, SenderID: launcherSender, PeerKind: "direct",
		Sender: bus.SenderInfo{Platform: "pico", PlatformID: "pico-user", CanonicalID: launcherSender, WebAuthenticated: true},
	}
	if !isWebOwner(deps, in) {
		return LauncherDecision{Result: LauncherNotOwner, Text: notAuthorizedText}
	}

	slot := &decisionSlot{}
	st := &callState{deps: deps, outcome: outcomeOK, owner: true}
	hctx, cancel := context.WithTimeout(context.WithValue(context.WithValue(ctx, callKey{}, st), decisionKey{}, slot), handleTimeout)
	defer cancel()
	rep := d.decideProposal(hctx, deps, in, code, "", approve)

	var se *approval.StatusError
	switch {
	case slot.err == nil && st.outcome == outcomeOK:
		return LauncherDecision{Result: LauncherDecided, Text: rep.Text, Status: string(slot.p.Status)}
	case errors.Is(slot.err, approval.ErrNotOwner):
		return LauncherDecision{Result: LauncherNotOwner, Text: rep.Text}
	case errors.Is(slot.err, approval.ErrKilled):
		return LauncherDecision{Result: LauncherHeld, Text: rep.Text}
	case errors.Is(slot.err, approval.ErrUnavailable):
		return LauncherDecision{Result: LauncherUnavailable, Text: rep.Text}
	case errors.Is(slot.err, approval.ErrNotFound):
		return LauncherDecision{Result: LauncherNotFound, Text: rep.Text}
	case errors.As(slot.err, &se):
		return LauncherDecision{Result: LauncherSettled, Text: rep.Text, Status: string(se.Status)}
	}
	return LauncherDecision{Result: LauncherFailed, Text: rep.Text}
}
