package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/cognition"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/routing"
)

// This file routes the loop's model-driven turns through the cognition
// scheduler. With no scheduler installed (proactive.cognition.enabled=false, the
// default, and every CLI run) none of it is reached: each entry point checks
// al.scheduler() first and takes the path it always took.

const (
	// legacyTurnDeadline bounds a scheduled turn that is not a proactive one: a
	// user turn, a legacy cron or heartbeat turn, a system follow-up. The
	// scheduler's own default for non-user kinds is 60s, which would newly cut off
	// turns that run for minutes today.
	legacyTurnDeadline = 10 * time.Minute

	deadlineNotice = "⚠️ That request took longer than the allowed time and was stopped."
)

// turnScheduler is the part of *cognition.Scheduler the loop uses.
type turnScheduler interface {
	Submit(ctx context.Context, j cognition.Job) (<-chan cognition.Result, error)
}

type schedulerHolder struct{ s turnScheduler }

// SetScheduler installs the cognition scheduler every model-driven turn is
// submitted to. nil removes it, and the loop is then exactly what it is without
// cognition. The gateway sets it per generation of services.
func (al *AgentLoop) SetScheduler(s *cognition.Scheduler) {
	if s == nil {
		al.sched.Store(nil)
		return
	}
	al.sched.Store(&schedulerHolder{s: s})
}

func (al *AgentLoop) scheduler() turnScheduler {
	if h := al.sched.Load(); h != nil {
		return h.s
	}
	return nil
}

type inScheduledJobKey struct{}

// inScheduledJob reports whether ctx belongs to a running scheduled job. An
// entry point reached from inside one runs inline: submitting and waiting from
// the only worker would deadlock.
func inScheduledJob(ctx context.Context) bool {
	v, _ := ctx.Value(inScheduledJobKey{}).(bool)
	return v
}

// turnSpec describes the job of one entry point.
type turnSpec struct {
	Kind     proactive.TurnKind
	Priority cognition.Priority
	Session  string
	Budgeted bool
	Deadline time.Duration
	// Fallback runs when the job is skipped (budget) or overran its deadline; nil
	// logs one line and does nothing else. It is never a model call.
	Fallback func(ctx context.Context, reason string) error
}

type jobState struct {
	started atomic.Bool
	reason  string // set by the Fallback, read after the result
	err     error  // what the body returned, set before the result
}

// submit queues body as a job. The body runs on the job's context, which
// carries the Submit ctx's values (origin, sender, scope, ...), the deadline and
// the cancellation of /stop, and a usage aggregate the job reports to the
// scheduler.
func (al *AgentLoop) submit(
	s turnScheduler, ctx context.Context, spec turnSpec, body func(ctx context.Context) error,
) (<-chan cognition.Result, *jobState, error) {
	st := &jobState{}
	fb := spec.Fallback
	if fb == nil {
		fb = func(context.Context, string) error {
			logger.InfoCF("agent", "Scheduled turn skipped", map[string]any{"kind": string(spec.Kind), "reason": st.reason})
			return nil
		}
	}
	ch, err := s.Submit(ctx, cognition.Job{
		Kind: spec.Kind, Priority: spec.Priority, SessionKey: spec.Session,
		Deadline: spec.Deadline, Budgeted: spec.Budgeted,
		Run: func(jctx context.Context) (cognition.Usage, error) {
			st.started.Store(true)
			agg := &usageAgg{}
			jctx = withUsageAgg(context.WithValue(jctx, inScheduledJobKey{}, true), agg)
			st.err = body(jctx)
			u := agg.close()
			return cognition.Usage{InputTokens: u.PromptTokens, OutputTokens: u.CompletionTokens}, st.err
		},
		Fallback: func(fctx context.Context, reason string) error {
			st.reason = reason
			return fb(fctx, reason)
		},
	})
	return ch, st, err
}

// result is the error a caller sees for a finished job: nil, the body's own
// error, proactive.ErrTurnSkipped for a job the budget skipped, or the
// scheduler's cancellation.
func (st *jobState) result(res cognition.Result) error {
	switch {
	case res.Fallback && st.reason == cognition.ReasonBudgetExhausted:
		return fmt.Errorf("%w (%s)", proactive.ErrTurnSkipped, st.reason)
	case res.Fallback: // overran its deadline
		if st.err != nil {
			return st.err
		}
		return cognition.ErrDeadline
	case res.Err == nil:
		return nil
	case st.err != nil && !errors.Is(res.Err, st.err):
		return fmt.Errorf("%w: %w", res.Err, st.err)
	}
	return res.Err
}

// submitTurn runs body as a job and waits for it. scheduled is false when
// nothing ran through the scheduler (none installed, called from inside a job,
// or it stopped before the job started) and the caller must run today's path.
func (al *AgentLoop) submitTurn(
	ctx context.Context, spec turnSpec, body func(ctx context.Context) error,
) (err error, scheduled bool) {
	s := al.scheduler()
	if s == nil || inScheduledJob(ctx) {
		return nil, false
	}
	ch, st, serr := al.submit(s, ctx, spec, body)
	if serr != nil {
		switch {
		case errors.Is(serr, cognition.ErrQueueFull):
			return fmt.Errorf("%w (queue full)", proactive.ErrTurnSkipped), true
		case spec.Budgeted: // never run background model work around the budget
			return fmt.Errorf("%w (%v)", proactive.ErrTurnSkipped, serr), true
		}
		return nil, false
	}
	var res cognition.Result
	select {
	case res = <-ch:
	case <-ctx.Done():
		if !st.started.Load() {
			// Still queued behind a busy worker: the caller's time is up. The
			// scheduler drops the job unrun when it reaches it (its ctx is done), or
			// it starts on a done ctx and fails at once; the caller reads nothing
			// of the body after this, so there is nothing to race.
			return fmt.Errorf("waiting for the model: %w", context.Cause(ctx)), true
		}
		res = <-ch
	}
	if !st.started.Load() && errors.Is(res.Err, cognition.ErrStopped) && !spec.Budgeted {
		return nil, false
	}
	return st.result(res), true
}

// turnBox carries what a job's body returned to the caller that waits for it. The
// caller may stop waiting while the job is still queued (its ctx ended), so the
// hand-over is locked: a caller that gave up never reads what the body writes.
type turnBox[T any] struct {
	mu  sync.Mutex
	v   T
	err error
	set bool
}

func (b *turnBox[T]) put(v T, err error) {
	b.mu.Lock()
	b.v, b.err, b.set = v, err, true
	b.mu.Unlock()
}

// outcome is what the body returned if it ran, else the scheduler's error.
func (b *turnBox[T]) outcome(schedErr error) (T, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.set {
		return b.v, b.err
	}
	var zero T
	return zero, schedErr
}

// dispatchInbound submits one inbound message as a job and returns at once. It
// reports false when the message must be handled inline, as without cognition.
func (al *AgentLoop) dispatchInbound(ctx context.Context, msg bus.InboundMessage) bool {
	s := al.scheduler()
	if s == nil {
		return false
	}
	ch, st, err := al.submit(s, ctx, al.inboundSpec(msg), func(jctx context.Context) error {
		err := al.handleInbound(jctx, msg)
		if err != nil && jctx.Err() != nil {
			return err // cancelled or out of time: the scheduler tells which
		}
		return nil
	})
	if err != nil {
		return false
	}
	go func() {
		// A scheduler that stops drops what it has not started. A message must not
		// be lost to a reload, so it is handled as it would be without one.
		if res := <-ch; !st.started.Load() && errors.Is(res.Err, cognition.ErrStopped) {
			al.handleInbound(ctx, msg)
		}
	}()
	return true
}

// inboundSpec is the job of an inbound message. The route and the session key
// are resolved here, before the job is queued: it is cheap, makes no model call,
// and is what the job is serialised on. They are the key runAgentLoop locks.
func (al *AgentLoop) inboundSpec(msg bus.InboundMessage) turnSpec {
	if msg.Channel == "system" {
		spec := turnSpec{Kind: proactive.TurnApprovalFollowUp, Priority: cognition.PriApprovalFollowUp, Deadline: legacyTurnDeadline}
		if agent := al.GetRegistry().GetDefaultAgent(); agent != nil {
			spec.Session = routing.BuildAgentMainSessionKey(agent.ID)
		}
		return spec
	}
	spec := turnSpec{Kind: proactive.TurnUser, Priority: cognition.PriUser}
	if route, _, err := al.resolveMessageRoute(msg); err == nil {
		spec.Session = resolveScopeKey(route, msg.SessionKey)
	}
	spec.Fallback = func(ctx context.Context, reason string) error {
		logger.WarnCF("agent", "User turn overran its deadline", map[string]any{"session_key": spec.Session})
		return al.bus.PublishOutbound(ctx, bus.OutboundMessage{Channel: msg.Channel, ChatID: msg.ChatID, Content: deadlineNotice})
	}
	return spec
}

// directSpec is the job of a ProcessDirect* call: a cron job's agent turn is a
// legacy one, anything else (the CLI, an internal caller) is the user's.
func (al *AgentLoop) directSpec(msg bus.InboundMessage, cronOrigin bool) turnSpec {
	spec := al.inboundSpec(msg)
	if cronOrigin {
		return turnSpec{Kind: proactive.TurnCronLegacy, Priority: cognition.PriLegacy, Session: spec.Session, Deadline: legacyTurnDeadline}
	}
	spec.Fallback = nil
	return spec
}

// HasScheduler reports whether the loop's turns go through a cognition scheduler.
func (al *AgentLoop) HasScheduler() bool { return al.scheduler() != nil }
