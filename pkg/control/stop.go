package control

import (
	"context"
	"fmt"

	"github.com/cryptoquantumwave/khunquant/pkg/control/whoami"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
)

const (
	stoppedText = "Stopped acting: approvals and automatic actions are refused until /resume. " +
		"Monitoring and notifications stay on."
	killOffText = "The kill switch is not available here (proactive features are off or the state could not be opened)."
)

// cmdStop engages the kill switch. In this change that is all it does: the flag
// is read by the approval service (a risk-increasing order cannot be proposed,
// approved or executed while it is set) and by the executors that can act on
// their own. Cancelling what is already running is a later change's work.
func (d *Dispatcher) cmdStop(ctx context.Context, in Input) (Reply, error) {
	st := stateOf(ctx)
	if st.deps.Kill == nil {
		setOutcome(ctx, outcomeOff)
		return Reply{Text: killOffText}, nil
	}
	changed, err := st.deps.Kill.Engage(ctx, canonicalSender(in), "/stop on "+in.Channel)
	if err != nil {
		return Reply{}, fmt.Errorf("stop: %w", err)
	}
	logger.WarnCF("control", "Kill switch engaged", map[string]any{"channel": in.Channel, "was_engaged": !changed})
	if !changed {
		return Reply{Text: "Already stopped. " + stoppedText}, nil
	}
	return Reply{Text: stoppedText}, nil
}

// cmdResume clears the kill switch.
func (d *Dispatcher) cmdResume(ctx context.Context, in Input) (Reply, error) {
	st := stateOf(ctx)
	if st.deps.Kill == nil {
		setOutcome(ctx, outcomeOff)
		return Reply{Text: killOffText}, nil
	}
	changed, err := st.deps.Kill.Clear(ctx, canonicalSender(in))
	if err != nil {
		return Reply{}, fmt.Errorf("resume: %w", err)
	}
	if !changed {
		return Reply{Text: "I was not stopped, so there is nothing to resume."}, nil
	}
	return Reply{Text: "Resumed: approvals and automatic actions are allowed again."}, nil
}

// cmdWhoami answers any allow-listed sender: it is how the owner is configured in
// the first place. The text is the same one the /whoami alias in pkg/commands gives.
func (d *Dispatcher) cmdWhoami(ctx context.Context, in Input) (Reply, error) {
	st := stateOf(ctx)
	sender := in.SenderID
	if sender == "" {
		sender = in.Sender.CanonicalID
	}
	return Reply{Text: whoami.Text(whoami.Query{
		Channel: in.Channel, ChatID: in.ChatID, SenderID: sender, PeerKind: in.PeerKind,
	}, st.deps.Cfg)}, nil
}
