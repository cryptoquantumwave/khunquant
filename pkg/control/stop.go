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

// cmdStop is /stop. With a Stopper (the gateway installs one) it runs the whole
// sequence of Kill.Stop and replies with its summary; without one it only engages
// the kill flag, which every executor that can act on its own reads. Either way the
// flag is the first thing set, and a /stop while already stopped runs the sequence
// again: it may find something new.
func (d *Dispatcher) cmdStop(ctx context.Context, in Input) (Reply, error) {
	st := stateOf(ctx)
	if st.deps.Stop != nil {
		sum := st.deps.Stop.Stop(ctx, canonicalSender(in), "/stop on "+in.Channel)
		logger.WarnCF("control", "Kill switch run", map[string]any{"channel": in.Channel, "engaged": sum.Engaged, "pending": sum.Pending})
		if !sum.Engaged {
			setOutcome(ctx, outcomeError)
		}
		return Reply{Text: sum.Text()}, nil
	}
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
