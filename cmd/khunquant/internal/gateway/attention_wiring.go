package gateway

import (
	"context"
	"fmt"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/attention"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/notify"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive/engine"
)

// attentionStopTimeout bounds how long stopServices waits for the outbox's final
// flush (Run bounds the flush itself to a few seconds).
const attentionStopTimeout = 10 * time.Second

// ownerNotifier is the part of *notify.Service the attention adapter uses. A nil
// *notify.Service satisfies it and reports "disabled".
type ownerNotifier interface {
	Deliver(ctx context.Context, n notify.Notification) (notify.Result, error)
}

// attentionDeliverer adapts the notify service to attention.Deliverer, so that
// pkg/attention does not import pkg/notify. A message carries no footer and, except
// for the memory suggestions of a digest (digestActions), no actions; a digest is sent
// without a notification sound.
type attentionDeliverer struct{ svc ownerNotifier }

func (a attentionDeliverer) Deliver(ctx context.Context, m attention.Message) (bool, error) {
	n := notify.Notification{
		Severity:            m.Severity,
		Title:               m.Title,
		Body:                m.Body,
		DedupKey:            m.DedupKey,
		InterventionID:      m.InterventionID,
		DisableNotification: m.Silent,
	}
	n.Actions = digestActions(ctx, m) // Save and Discard for the memory suggestions of a digest
	if m.SessionRestricted {
		// The owner is sent the message as it is; the session, which the model reads
		// on the owner's next turn, gets the form without text from outside.
		sb := m.SessionBody
		n.SessionBody = &sb
	}
	if len(m.Decisions) > 0 {
		c := m.Decisions[0].Candidate
		n.Candidate = &c
	}
	res, err := a.svc.Deliver(ctx, n)
	if err != nil {
		return false, err
	}
	if !res.Delivered {
		return false, fmt.Errorf("not delivered: %s", res.Reason)
	}
	return true, nil
}

// attentionRunner is the goroutine that drives an outbox's Run loop.
type attentionRunner struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// stop ends the Run loop and waits, at most attentionStopTimeout, for its final
// flush. It is safe on a nil runner.
func (r *attentionRunner) stop() {
	if r == nil {
		return
	}
	r.cancel()
	select {
	case <-r.done:
	case <-time.After(attentionStopTimeout):
		logger.WarnCF("attention", "Outbox did not stop in time", nil)
	}
}

// buildAttention constructs the governor and the outbox for this generation, and
// starts the outbox's Run loop when it can ever deliver.
//
// Nothing is built, and nothing is migrated, when proactive is disabled, when the
// effective attention mode is off, or when the shared database is unavailable: a
// default install gets no governor. In shadow mode the governor records decisions
// and the outbox exists for callers to Submit to, but nothing is ever delivered
// (shadow decisions are logged, never buffered), so no Run loop is started.
//
// In live mode the outbox is returned only together with a Run loop that can
// deliver. When the notify service cannot reach the owner, attention is
// unavailable (nil, nil) and a WARN says so: an outbox nobody flushes would only
// hold decisions in flight, and callers (the heartbeat) must stay on their legacy
// path rather than lose output.
//
// The profile comes from an intent store over the same shared database as the
// profile tools (the agent loop's own handle on it is private to pkg/agent).
func buildAttention(d assemblyDeps, svc *notify.Service) (*attention.Outbox, *attentionRunner) {
	eff := d.Cfg.Proactive.Effective()
	if !eff.Enabled || eff.AttentionMode == config.ProactiveModeOff {
		return nil, nil
	}
	live := eff.AttentionMode == config.ProactiveModeLive
	if live && (svc == nil || !svc.CanDeliver()) {
		logger.WarnCF("attention", "Attention unavailable: it is live but the owner cannot be reached", nil)
		return nil, nil
	}
	db, err := proactiveDB(d)
	if err != nil {
		logger.ErrorCF("attention", "Attention disabled: cannot open the proactive database",
			map[string]any{"error": err.Error()})
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	profiles, err := intent.NewStore(ctx, db)
	if err != nil {
		logger.ErrorCF("attention", "Attention disabled: cannot open the profile store",
			map[string]any{"error": err.Error()})
		return nil, nil
	}
	acfg := d.Cfg.Proactive.Attention
	gov, err := attention.New(db, profiles, acfg, eff.AttentionMode, nil)
	if err != nil {
		logger.ErrorCF("attention", "Attention disabled: cannot build the governor",
			map[string]any{"error": err.Error()})
		return nil, nil
	}
	window := time.Duration(max(acfg.GroupWindowSeconds, 0)) * time.Second
	ob := attention.NewOutbox(gov, attentionDeliverer{svc: svc}, window, nil)
	// The composer for escalated groups exists only when the perception pipeline
	// does (nothing else produces an escalate), and is set here, before the Run
	// loop below starts, because the field is read by it.
	if eff.Perception && d.AgentLoop != nil {
		ob.Composer = engine.NewComposer(agentTurnRunner{run: d.AgentLoop})
	}

	if !live {
		return ob, nil
	}
	if !acfg.Digest {
		logger.WarnCF("attention", "Daily digest is switched off (proactive.attention.digest=false): "+
			"items held back for quiet hours, Info severity or the budget are recorded and never sent", nil)
	}
	runCtx, stop := context.WithCancel(context.Background())
	r := &attentionRunner{cancel: stop, done: make(chan struct{})}
	go func() {
		defer close(r.done)
		ob.Run(runCtx)
	}()
	return ob, r
}
