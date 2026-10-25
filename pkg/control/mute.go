package control

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/feedback"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
)

const (
	// defaultSnooze is how long /snooze with no argument silences a topic.
	defaultSnooze = time.Hour
	// muteDuration is how long /mute silences a trigger.
	muteDuration = 30 * 24 * time.Hour

	attentionOffText = "Attention is off, so no alert is being held back and there is nothing to mute. " +
		"Turn on proactive.attention to use this."
	notAnAlertText = "Nothing to snooze: that is not one of my alerts. Reply to the alert itself."
)

var snoozeDurations = map[string]time.Duration{"1h": time.Hour, "4h": 4 * time.Hour, "1d": 24 * time.Hour}

// triggerRE is what /mute and /unmute accept: a trigger kind or ID, such as
// "price_move" or "dn:12". It is a literal prefix, so it is kept to the characters
// those are made of.
var triggerRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/#-]{1,119}$`)

// subjectKeys maps the message a command replies to back to the dedup keys of the
// alert it carried. When it cannot, text is the reply to give and the outcome is set.
func (d *Dispatcher) subjectKeys(ctx context.Context, in Input) (keys []string, interventionID string, text string) {
	st := stateOf(ctx)
	if in.ReplyToMessageID == "" {
		setOutcome(ctx, outcomeUsage)
		return nil, "", "Reply to one of my alerts with this command."
	}
	if st.deps.Sent == nil || st.deps.Attention == nil {
		setOutcome(ctx, outcomeOff)
		return nil, "", attentionOffText
	}
	id, ok, err := st.deps.Sent.LookupByPlatformMessageID(ctx, in.Channel, in.ReplyToMessageID)
	if err != nil {
		logger.WarnCF("control", "Could not look up the message a reply points at", map[string]any{"error": err.Error()})
		setOutcome(ctx, outcomeError)
		return nil, "", failedText
	}
	if !ok {
		setOutcome(ctx, outcomeNotFound)
		return nil, "", notAnAlertText
	}
	keys, err = st.deps.Attention.InterventionKeys(ctx, id)
	if err != nil {
		logger.WarnCF("control", "Could not read the decision log", map[string]any{"error": err.Error()})
		setOutcome(ctx, outcomeError)
		return nil, "", failedText
	}
	return keys, id, ""
}

// cmdSnooze is "/snooze [1h|4h|1d]" as a reply to a proactive message: it mutes
// that message's dedup keys until then.
func (d *Dispatcher) cmdSnooze(ctx context.Context, in Input) (Reply, error) {
	st := stateOf(ctx)
	dur := defaultSnooze
	label := "1h"
	switch len(st.cmd.args) {
	case 0:
	case 1:
		v, ok := snoozeDurations[strings.ToLower(st.cmd.args[0])]
		if !ok {
			setOutcome(ctx, outcomeUsage)
			return Reply{Text: "Usage: /snooze [1h|4h|1d], as a reply to one of my alerts."}, nil
		}
		dur, label = v, strings.ToLower(st.cmd.args[0])
	default:
		setOutcome(ctx, outcomeUsage)
		return Reply{Text: "Usage: /snooze [1h|4h|1d], as a reply to one of my alerts."}, nil
	}
	keys, iid, text := d.subjectKeys(ctx, in)
	if text != "" {
		return Reply{Text: text}, nil
	}
	if len(keys) == 0 {
		setOutcome(ctx, outcomeNotFound)
		return Reply{Text: notAnAlertText}, nil
	}
	until := d.now().Add(dur)
	for _, k := range keys {
		if err := st.deps.Attention.MuteExact(ctx, k, until, "snoozed by the owner"); err != nil {
			return Reply{}, fmt.Errorf("snooze: %w", err)
		}
	}
	path, event := tapOrCommand(in, label)
	d.record(ctx, st.deps, in, iid, feedback.KindSnooze, label, path, event)
	return Reply{Text: fmt.Sprintf("Snoozed for %s: I will not repeat this alert until then. (/mute TRIGGER silences a whole kind.)", label)}, nil
}

// cmdMute is "/mute TRIGGER": it silences a trigger kind or ID for 30 days.
func (d *Dispatcher) cmdMute(ctx context.Context, in Input) (Reply, error) {
	st := stateOf(ctx)
	if st.cmd.viaCallback && len(st.cmd.args) == 0 {
		return d.muteMessage(ctx, in)
	}
	trigger, usage := triggerArg(st.cmd.args, "/mute")
	if usage != "" {
		setOutcome(ctx, outcomeUsage)
		return Reply{Text: usage}, nil
	}
	if st.deps.Attention == nil {
		setOutcome(ctx, outcomeOff)
		return Reply{Text: attentionOffText}, nil
	}
	if err := st.deps.Attention.Mute(ctx, trigger, d.now().Add(muteDuration), "muted by the owner"); err != nil {
		return Reply{}, fmt.Errorf("mute: %w", err)
	}
	// A /mute typed as a reply to an alert also tells the feedback store which one.
	if st.deps.Feedback != nil {
		if iid, ok := d.interventionOf(ctx, st.deps, in.Channel, in.ReplyToMessageID); ok {
			d.record(ctx, st.deps, in, iid, feedback.KindMute, "30d", "command", in.MessageID+":"+trigger)
		}
	}
	return Reply{Text: fmt.Sprintf("Muted %s for 30 days. A protective critical alert still gets through unless you "+
		"muted its exact id. /unmute %s undoes it.", trigger, trigger)}, nil
}

// muteMessage is the [Mute] button: it silences exactly the dedup keys of the alert the
// button was on for 30 days (an exact mute, like a snooze, so it needs no trigger name
// and silences nothing but this alert's own keys).
func (d *Dispatcher) muteMessage(ctx context.Context, in Input) (Reply, error) {
	st := stateOf(ctx)
	keys, iid, text := d.subjectKeys(ctx, in)
	if text != "" {
		return Reply{Text: text}, nil
	}
	if len(keys) == 0 {
		setOutcome(ctx, outcomeNotFound)
		return Reply{Text: notAnAlertText}, nil
	}
	until := d.now().Add(muteDuration)
	for _, k := range keys {
		if err := st.deps.Attention.MuteExact(ctx, k, until, "muted by the owner"); err != nil {
			return Reply{}, fmt.Errorf("mute: %w", err)
		}
	}
	path, event := tapOrCommand(in, "mute")
	d.record(ctx, st.deps, in, iid, feedback.KindMute, "30d", path, event)
	return Reply{Text: "Muted this alert for 30 days. (/mute TRIGGER silences a whole kind.)"}, nil
}

// cmdUnmute is "/unmute TRIGGER".
func (d *Dispatcher) cmdUnmute(ctx context.Context, in Input) (Reply, error) {
	st := stateOf(ctx)
	trigger, usage := triggerArg(st.cmd.args, "/unmute")
	if usage != "" {
		setOutcome(ctx, outcomeUsage)
		return Reply{Text: usage}, nil
	}
	if st.deps.Attention == nil {
		setOutcome(ctx, outcomeOff)
		return Reply{Text: attentionOffText}, nil
	}
	if err := st.deps.Attention.Unmute(ctx, trigger); err != nil {
		return Reply{}, fmt.Errorf("unmute: %w", err)
	}
	return Reply{Text: fmt.Sprintf("Unmuted %s (if it was muted).", trigger)}, nil
}

func triggerArg(args []string, verb string) (trigger, usage string) {
	if len(args) != 1 || !triggerRE.MatchString(args[0]) {
		return "", "Usage: " + verb + " TRIGGER (a trigger kind or id, for example price_move)."
	}
	return args[0], ""
}

// cmdUseful records the owner's verdict on an alert in the feedback store. With
// learning off there is no store: it is a log line, and the reply says so.
func (d *Dispatcher) cmdUseful(useful bool) Handler {
	return func(ctx context.Context, in Input) (Reply, error) {
		st := stateOf(ctx)
		if len(st.cmd.args) != 0 {
			setOutcome(ctx, outcomeUsage)
			return Reply{Text: "Usage: reply to one of my alerts with /useful or /notuseful."}, nil
		}
		if in.ReplyToMessageID == "" {
			setOutcome(ctx, outcomeUsage)
			return Reply{Text: "Reply to one of my alerts with /useful or /notuseful."}, nil
		}
		if st.deps.Sent == nil {
			setOutcome(ctx, outcomeOff)
			return Reply{Text: "Alerts are not being tracked here, so there is nothing to rate."}, nil
		}
		id, ok, err := st.deps.Sent.LookupByPlatformMessageID(ctx, in.Channel, in.ReplyToMessageID)
		if err != nil {
			return Reply{}, fmt.Errorf("feedback: %w", err)
		}
		if !ok {
			setOutcome(ctx, outcomeNotFound)
			return Reply{Text: "That is not one of my alerts. Reply to the alert itself."}, nil
		}
		verdict := "not_useful"
		if useful {
			verdict = "useful"
		}
		if st.deps.Feedback != nil {
			kind := feedback.KindNotUseful
			if useful {
				kind = feedback.KindUseful
			}
			path, event := tapOrCommand(in, verdict)
			d.record(ctx, st.deps, in, id, kind, "", path, event)
			return Reply{Text: "Noted as " + strings.ReplaceAll(verdict, "_", " ") + "."}, nil
		}
		logger.InfoCF("control", "Alert feedback", map[string]any{"intervention_id": id, "verdict": verdict})
		return Reply{Text: "Noted as " + strings.ReplaceAll(verdict, "_", " ") +
			". For now feedback is only written to the log; it is not stored or used yet."}, nil
	}
}
