package gateway

import (
	"context"
	"fmt"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/attention"
	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral"
	"github.com/cryptoquantumwave/khunquant/pkg/journal"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/notify"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

func init() {
	// dn.open, dn.recover, dn.unwind and dn.resize join the journal registry while the
	// delta-neutral tools are on: a confirmed open, unwind or resize is a durable
	// workflow, so it needs the engine. The gateway runs all four kinds (it has the
	// owner channel and the approval service dn.recover needs).
	journalSources = append(journalSources, func(d assemblyDeps) []journal.Definition {
		if d.DNFlow == nil {
			return nil
		}
		return d.DNFlow.deps.Definitions()
	})
}

// dnFlowRuntime is the delta-neutral workflow side of one generation of services.
// It owns no goroutine: the journal engine runs the workflows.
type dnFlowRuntime struct {
	deps *deltaneutral.Deps
}

// buildDNFlow constructs the dependencies of the delta-neutral workflows, or nil when
// the delta-neutral tools are off or their store could not be opened. Nothing is
// started: attach does that once the engine exists.
//
// dn.recover asks through the approval service while approvals are on; without
// it, it still reads the exchange and wakes the owner with a Critical protective
// candidate, and closes nothing. The kill flag is Deps.Killed, the SAME
// control.State the approval service gets (controlRuntime.killer, never a second
// instance): the place steps of dn.open and of a dn.resize increase refuse while it is
// engaged; dn.recover, dn.unwind and a dn.resize decrease ignore it, because they only
// reduce exposure or ask.
// It is nil (never killed) only when the control state is unavailable, exactly as the
// approval service's Killer then is.
func buildDNFlow(d assemblyDeps, outbox *attention.Outbox, ns *notify.Service) *dnFlowRuntime {
	if d.DNStore == nil || !dnToolsEnabled(d.Cfg) {
		return nil
	}
	deps := &deltaneutral.Deps{
		Store:    d.DNStore,
		Exchange: tools.NewDNExchange(d.Cfg, d.nowFn()),
		Clock:    d.Shared.clk(),
		Tag: func(ctx context.Context, origin, sender string) context.Context {
			return tools.WithToolSender(tools.WithToolOrigin(ctx, origin), sender)
		},
	}
	if k := d.Control.killer(); k != nil {
		deps.Killed = k.Killed
	}
	if outbox != nil {
		deps.Alerts = outboxAlerts{ob: outbox, now: d.nowFn()}
	}
	if d.Approval != nil && d.Approval.svc != nil {
		deps.Proposer = tools.NewApprovalCloser(d.Approval.svc)
	}
	if ns != nil {
		deps.Tell = func(ctx context.Context, title, body string) {
			res, err := ns.Deliver(ctx, notify.Notification{Severity: proactive.SeverityInfo, Title: title, Body: body})
			if err != nil || !res.Delivered {
				logger.WarnCF("gateway", "Could not tell the owner how a delta-neutral operation ended", map[string]any{"plan_title": title})
			}
		}
	}
	return &dnFlowRuntime{deps: deps}
}

// attach connects the workflows to the engine (they start dn.recover through it) and
// opens the open, unwind and resize tools. Without an engine they refuse: a two-leg
// operation that a crash could not resume is never started.
func (rt *dnFlowRuntime) attach(j *journalRuntime) {
	if rt == nil {
		return
	}
	if j == nil {
		logger.ErrorCF("gateway", "open_delta_neutral_position will refuse to open: the workflow engine is not running", nil)
		return
	}
	rt.deps.SetStarter(j.engine)
	tools.SetDNWorkflows(j.engine)
}

// stop closes the tool's door. Safe on nil.
func (rt *dnFlowRuntime) stop() {
	if rt == nil {
		return
	}
	tools.SetDNWorkflows(nil)
}

// outboxAlerts delivers a recovery alert as a Critical protective candidate through
// the attention outbox: the path a delta-neutral breach takes. A protective
// Critical bypasses quiet hours and the budget and is flushed at once, so the owner
// is woken. The decision it returns is what a proposal for the same situation is
// bound to (a background process may only propose on a candidate attention decided
// to deliver now).
type outboxAlerts struct {
	ob  *attention.Outbox
	now func() time.Time // the gateway clock; nil is time.Now
}

func (a outboxAlerts) clock() time.Time {
	if a.now != nil {
		return a.now()
	}
	return time.Now()
}

func (a outboxAlerts) Alert(ctx context.Context, al deltaneutral.RecoveryAlert) (deltaneutral.AlertResult, error) {
	c := proactive.Candidate{
		TriggerID:  fmt.Sprintf("dn_recover:%d", al.PlanID),
		DedupKey:   fmt.Sprintf("dn_recover:%d:%d:%d", al.PlanID, al.ExecutionID, al.Round),
		Severity:   proactive.SeverityCritical,
		Protective: true,
		Salience:   0.95,
		Subject:    fmt.Sprintf("plan:%d", al.PlanID),
		Title:      al.Title,
		Detail:     al.Detail,
		Trust:      proactive.TrustSystem,
		Compose:    proactive.ComposeTemplate,
		At:         a.clock(),
	}
	dec, err := a.ob.Submit(ctx, attention.Item{Candidate: c})
	if err != nil {
		return deltaneutral.AlertResult{}, err
	}
	now := dec.Disposition == proactive.DispNotify || dec.Disposition == proactive.DispEscalate
	return deltaneutral.AlertResult{CandidateID: dec.Candidate.ID, DeliverNow: now}, nil
}
