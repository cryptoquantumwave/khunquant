package control

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/journal"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// Stopper runs the whole of /stop (see Stop). *Kill is the implementation.
type Stopper interface {
	Stop(ctx context.Context, by, reason string) StopSummary
}

// TurnCanceller cancels the running model turns (*agent.AgentLoop). The control
// package does not import the agent.
type TurnCanceller interface {
	CancelAllTurns(reason string) int
}

// QueueCanceller drops the model work queued behind the running turns and cancels
// the scheduled ones (*cognition.Scheduler). nil when cognition is off.
type QueueCanceller interface {
	CancelAll(reason string)
}

// Flows is the part of *journal.Engine the kill switch uses.
type Flows interface {
	List(ctx context.Context, f journal.ListFilter) ([]journal.Workflow, error)
	Steps(ctx context.Context, id string) ([]journal.StepRecord, error)
	Get(ctx context.Context, id string) (journal.Workflow, error)
	Cancel(ctx context.Context, id, reason string) error
}

// MandateRevoker revokes every active mandate (*dca.Store).
type MandateRevoker interface {
	RevokeAll(ctx context.Context, reason string) (int, error)
}

// ProposalCanceller cancels every pending proposal and edits its card (*approval.Service).
type ProposalCanceller interface {
	CancelPending(ctx context.Context, reason string) ([]approval.Cancelled, error)
}

// FlowOrderer says, for a workflow and its journal, which orders it placed and
// whether it may be cancelled. The gateway builds it from the workflow packages'
// FlowOrders methods; a kind nobody describes answers the zero value.
type FlowOrderer func(ctx context.Context, w journal.Workflow, steps []journal.StepRecord) (broker.FlowOrders, error)

// OrderState is what the exchange showed for an order.
type OrderState struct {
	// ID is the exchange's id for the order (found by a lookup when the ref had none).
	ID string
	// Found: the exchange has the order. Open: it can still fill (and can be cancelled).
	Found, Open bool
	// Simulated: the order lives in the paper-mode simulation only.
	Simulated bool
}

// OrderDesk looks up and withdraws orders.
type OrderDesk interface {
	// Lookup never places or changes anything. An error means it could not tell.
	Lookup(ctx context.Context, ref broker.OrderRef) (OrderState, error)
	Cancel(ctx context.Context, ref broker.OrderRef, id string) error
}

// Defaults of the kill switch.
const (
	// DefaultReplyBudget is how long /stop works before it answers; what is not done
	// by then goes on in the background and its outcome is sent as a second message.
	DefaultReplyBudget = 2 * time.Second
	// DefaultSettleWait is how long a cancelled workflow that is mid-step is given to
	// reach its next boundary before the orders are looked for.
	DefaultSettleWait = time.Second
	// DefaultLateWait bounds the background wait for workflows still mid-step.
	DefaultLateWait = 90 * time.Second
	// DefaultLookback is how far back the agent's orders are looked for.
	DefaultLookback = 7 * 24 * time.Hour
	// DefaultMaxOrders bounds the exchange lookups of one run.
	DefaultMaxOrders = 40

	stopReason  = "kill switch"
	maxFlows    = 500
	pollEvery   = 25 * time.Millisecond
	stepTimeout = 20 * time.Second
)

// Kill is the /stop sequence. Every dependency but Flag may be nil (its step is then
// skipped and left out of the summary).
type Kill struct {
	Flag  KillSwitch
	Turns TurnCanceller
	// Queue is the cognition scheduler: what waits for a model turn is dropped, with
	// no fallback delivery, and what runs is cancelled. A message sent after /stop
	// is queued as usual.
	Queue     QueueCanceller
	Flows     Flows
	Orders    FlowOrderer
	Mandates  MandateRevoker
	Proposals ProposalCanceller
	Desk      OrderDesk
	// Followup sends the owner a message (the outcome of the steps that outlasted the
	// reply budget); nil sends none.
	Followup func(ctx context.Context, text string)
	Clock    journal.Clock

	ReplyBudget, SettleWait, LateWait, Lookback time.Duration
	MaxOrders                                   int

	running sync.Mutex
}

// Stop is /stop, in this order, each step bounded and none stopped by another's
// failure (a failure is reported in the summary):
//
//  1. engage the kill flag, before anything else, so every executor refuses new acts;
//  2. cancel the running model turns;
//  3. cancel the non-protective workflows at their next step boundary (a protective
//     one, an approved exit, and one whose place step has finished keep running: see
//     broker.FlowOrders);
//  4. revoke every mandate and cancel every pending proposal;
//  5. withdraw the open orders the agent placed in the last 7 days (found in the
//     journal, never the owner's own), and name those that may exist;
//  6. the summary.
//
// It returns when the sequence ends or the reply budget does, whichever is first.
// The sequence is not cut at the budget: it goes on in the background and Followup
// carries the final summary. A second Stop while one runs answers Busy.
func (k *Kill) Stop(ctx context.Context, by, reason string) StopSummary {
	if !k.running.TryLock() {
		return k.busy(ctx, by, reason)
	}
	r := &stopRun{}
	bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), k.lateWait()+6*stepTimeout)
	done := make(chan struct{})
	go func() {
		defer k.running.Unlock()
		defer cancel()
		defer close(done)
		k.execute(bg, r, by, reason)
		if send := r.finish(); send != nil && k.Followup != nil {
			k.Followup(bg, "Update on /stop:\n"+send.Text())
		}
	}()
	select {
	case <-done:
	case <-k.clock().After(k.replyBudget()):
	case <-ctx.Done():
	}
	return r.detach()
}

// busy answers a /stop that arrives while another still runs. The earlier run may
// hold the lock for a long while, and /resume does not touch it, so the flag is set
// here as well, and running turns are cancelled again (both are cheap): a /stop must
// always leave the switch engaged, whatever else is going on.
func (k *Kill) busy(ctx context.Context, by, reason string) StopSummary {
	sum := StopSummary{Busy: true}
	if k.Flag != nil {
		_, err := k.Flag.Engage(ctx, by, reason)
		sum.Engaged = err == nil || k.Flag.Killed()
	}
	if k.Turns != nil {
		sum.TurnsCancelled = k.Turns.CancelAllTurns(stopReason)
	}
	k.dropQueue()
	return sum
}

func (k *Kill) dropQueue() {
	if k.Queue != nil {
		k.Queue.CancelAll(stopReason)
	}
}

func (k *Kill) clock() journal.Clock {
	if k.Clock == nil {
		return journal.RealClock{}
	}
	return k.Clock
}

func orDefault(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}

func (k *Kill) replyBudget() time.Duration { return orDefault(k.ReplyBudget, DefaultReplyBudget) }
func (k *Kill) settleWait() time.Duration  { return orDefault(k.SettleWait, DefaultSettleWait) }
func (k *Kill) lateWait() time.Duration    { return orDefault(k.LateWait, DefaultLateWait) }
func (k *Kill) lookback() time.Duration    { return orDefault(k.Lookback, DefaultLookback) }

// stopRun is the summary under construction, shared by the worker and the reply.
type stopRun struct {
	mu       sync.Mutex
	sum      StopSummary
	finished bool
	detached bool
}

func (r *stopRun) edit(f func(s *StopSummary)) {
	r.mu.Lock()
	f(&r.sum)
	r.mu.Unlock()
}

func (r *stopRun) fail(step string, err error) {
	logger.WarnCF("control", "A /stop step failed", map[string]any{"step": step, "error": err.Error()})
	r.edit(func(s *StopSummary) { s.Errors = append(s.Errors, step+": "+shorten(err.Error())) })
}

// detach is the reply: the summary so far. If the sequence is not over it is marked
// pending, and finish will hand the final summary back for a second message.
func (r *stopRun) detach() StopSummary {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.finished {
		r.detached, r.sum.Pending = true, true
	}
	return r.sum.clone()
}

// finish ends the run; it returns the final summary when a reply already went out
// without it.
func (r *stopRun) finish() *StopSummary {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.finished = true
	if !r.detached {
		return nil
	}
	r.sum.Pending = false
	s := r.sum.clone()
	return &s
}

func shorten(s string) string {
	r := []rune(s)
	if len(r) > 160 {
		return string(r[:160]) + "..."
	}
	return s
}

// guard runs one step, turning a panic into a reported failure.
func (r *stopRun) guard(step string, f func() error) {
	defer func() {
		if rec := recover(); rec != nil {
			r.fail(step, fmt.Errorf("panic: %v", rec))
		}
	}()
	if err := f(); err != nil {
		r.fail(step, err)
	}
}

func (k *Kill) execute(ctx context.Context, r *stopRun, by, reason string) {
	// 1. The flag first: from here every executor refuses new acts.
	if k.Flag != nil {
		r.guard("kill flag", func() error {
			changed, err := k.Flag.Engage(ctx, by, reason)
			if err != nil {
				return err
			}
			r.edit(func(s *StopSummary) { s.Engaged, s.AlreadyEngaged = true, !changed })
			return nil
		})
		// A flag that did not write may still read engaged (a second process set it).
		r.edit(func(s *StopSummary) { s.Engaged = s.Engaged || k.Flag.Killed() })
	}
	// 2. Running turns.
	if k.Turns != nil {
		r.guard("turns", func() error {
			n := k.Turns.CancelAllTurns(stopReason)
			r.edit(func(s *StopSummary) { s.TurnsCancelled = n })
			return nil
		})
	}
	// 2b. What waits for a turn, and any job that started since: after the turns, so
	// the count above is of turns that were running when /stop came.
	k.dropQueue()
	// 3. Workflows.
	var stopping []stopEntry // cancelled workflows that are mid-step
	if k.Flows != nil {
		r.guard("workflows", func() error {
			var err error
			stopping, err = k.cancelFlows(ctx, r)
			return err
		})
	}
	// 4. Mandates and proposals.
	if k.Mandates != nil {
		r.guard("mandates", func() error {
			n, err := k.Mandates.RevokeAll(ctx, stopReason)
			r.edit(func(s *StopSummary) { s.MandatesRevoked = n })
			return err
		})
	}
	if k.Proposals != nil {
		r.guard("proposals", func() error {
			cancelled, err := k.Proposals.CancelPending(ctx, stopReason)
			r.edit(func(s *StopSummary) { s.ProposalsCancelled = len(cancelled) })
			return err
		})
	}
	// 5. Orders, once the cancelled workflows have left their steps: a place step that
	// was in flight when the flag went up lands its order before its workflow stops.
	if k.Flows == nil || k.Desk == nil || k.Orders == nil {
		return
	}
	late := k.settle(ctx, r, stopping, k.settleWait())
	lateSet := map[string]bool{}
	for _, e := range late {
		lateSet[e.id] = true
	}
	budget := k.maxOrders()
	r.guard("orders", func() error { return k.cancelOrders(ctx, r, lateSet, &budget) })
	if len(late) == 0 {
		return
	}
	still := k.settle(ctx, r, late, k.lateWait())
	stillSet := map[string]bool{}
	for _, e := range still {
		stillSet[e.id] = true
	}
	// What has now stopped is examined; what has not is named.
	only := map[string]bool{}
	for _, e := range late {
		if !stillSet[e.id] {
			only[e.id] = true
		}
	}
	r.guard("orders (late)", func() error { return k.cancelOrdersOf(ctx, r, only, &budget) })
	if len(still) > 0 {
		r.edit(func(s *StopSummary) {
			s.Errors = append(s.Errors, fmt.Sprintf("%d workflow(s) were still mid-step after %s; check the exchange for orders they placed", len(still), k.lateWait()))
		})
	}
}

func noteOf(n string) string {
	if n == "" {
		return ""
	}
	return " (" + n + ")"
}

func (k *Kill) maxOrders() int {
	if k.MaxOrders > 0 {
		return k.MaxOrders
	}
	return DefaultMaxOrders
}

// stopEntry is a workflow whose cancel was requested while it was mid-step.
type stopEntry struct{ id, kind string }

// cancelFlows cancels every active workflow that is not protective and has not
// begun to settle. It returns those that are mid-step.
func (k *Kill) cancelFlows(ctx context.Context, r *stopRun) ([]stopEntry, error) {
	ctx, cancel := context.WithTimeout(ctx, stepTimeout)
	defer cancel()
	active, err := k.Flows.List(ctx, journal.ListFilter{Statuses: journal.ActiveStatuses, Limit: maxFlows})
	if err != nil {
		return nil, err
	}
	var (
		stopping []stopEntry
		errs     []error
	)
	for _, w := range active {
		var fo broker.FlowOrders
		if k.Orders != nil {
			steps, serr := k.Flows.Steps(ctx, w.ID)
			if serr == nil {
				fo, serr = k.Orders(ctx, w, steps)
			}
			if serr != nil {
				// Unreadable: treat it as one that adds risk (cancel it) and say that
				// its orders were not examined.
				r.edit(func(s *StopSummary) {
					s.OrdersMayExist = append(s.OrdersMayExist, w.Kind+" workflow (its journal could not be read)")
				})
				errs = append(errs, fmt.Errorf("%s: %w", w.Kind, serr))
			}
		}
		switch {
		case w.Protective || fo.Protect:
			r.edit(func(s *StopSummary) { s.WorkflowsProtective = append(s.WorkflowsProtective, w.Kind+noteOf(fo.Note)) })
		case fo.Settle:
			r.edit(func(s *StopSummary) { s.WorkflowsFinishing = append(s.WorkflowsFinishing, w.Kind) })
		default:
			if err := k.Flows.Cancel(ctx, w.ID, stopReason); err != nil && !errors.Is(err, journal.ErrNotFound) &&
				!errors.Is(err, journal.ErrAlreadyFinished) {
				errs = append(errs, fmt.Errorf("cancel %s: %w", w.Kind, err))
				continue
			}
			if cur, gerr := k.Flows.Get(ctx, w.ID); gerr == nil && cur.Status == journal.StatusRunning {
				stopping = append(stopping, stopEntry{w.ID, w.Kind})
				r.edit(func(s *StopSummary) { s.WorkflowsStopping = append(s.WorkflowsStopping, w.Kind) })
				continue
			}
			r.edit(func(s *StopSummary) { s.WorkflowsCancelled = append(s.WorkflowsCancelled, w.Kind) })
		}
	}
	return stopping, errors.Join(errs...)
}

// settle waits up to d for each workflow in entries to leave the running state, and
// returns those that have not. A workflow that has stopped moves from "stopping" to
// "cancelled" in the summary. One that cannot be read counts as settled (there is
// nothing more to wait for).
func (k *Kill) settle(ctx context.Context, r *stopRun, entries []stopEntry, d time.Duration) []stopEntry {
	if len(entries) == 0 {
		return nil
	}
	deadline := k.clock().Now().Add(d)
	pending := entries
	for {
		var next []stopEntry
		for _, e := range pending {
			w, err := k.Flows.Get(ctx, e.id)
			if err == nil && w.Status == journal.StatusRunning {
				next = append(next, e)
				continue
			}
			r.edit(func(s *StopSummary) {
				s.WorkflowsStopping = removeOne(s.WorkflowsStopping, e.kind)
				s.WorkflowsCancelled = append(s.WorkflowsCancelled, e.kind)
			})
		}
		pending = next
		if len(pending) == 0 || !k.clock().Now().Before(deadline) {
			return pending
		}
		select {
		case <-ctx.Done():
			return pending
		case <-k.clock().After(pollEvery):
		}
	}
}

func removeOne(list []string, v string) []string {
	for i, x := range list {
		if x == v {
			return append(list[:i:i], list[i+1:]...)
		}
	}
	return list
}

func (k *Kill) cancelOrders(ctx context.Context, r *stopRun, skip map[string]bool, budget *int) error {
	return k.scanOrders(ctx, r, func(id string) bool { return !skip[id] }, budget)
}

func (k *Kill) cancelOrdersOf(ctx context.Context, r *stopRun, only map[string]bool, budget *int) error {
	return k.scanOrders(ctx, r, func(id string) bool { return only[id] }, budget)
}

// scanOrders walks the workflows of the look-back window, newest first, and
// withdraws what is still open among the orders the wanted ones placed.
func (k *Kill) scanOrders(ctx context.Context, r *stopRun, want func(id string) bool, budget *int) error {
	ctx, cancel := context.WithTimeout(ctx, 4*stepTimeout)
	defer cancel()
	flows, err := k.Flows.List(ctx, journal.ListFilter{Since: k.clock().Now().Add(-k.lookback()), Limit: maxFlows})
	if err != nil {
		return err
	}
	var errs []error
	for _, w := range flows {
		if !want(w.ID) || w.Protective {
			continue
		}
		steps, err := k.Flows.Steps(ctx, w.ID)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", w.Kind, err))
			continue
		}
		fo, err := k.Orders(ctx, w, steps)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", w.Kind, err))
			continue
		}
		// A protective workflow's own orders are exits (Exit refs): handleOrder names
		// them and leaves them, so they are not skipped here.
		for _, ref := range fo.Orders {
			k.handleOrder(ctx, r, ref, budget)
		}
	}
	return errors.Join(errs...)
}

func (k *Kill) handleOrder(ctx context.Context, r *stopRun, ref broker.OrderRef, budget *int) {
	label := orderLabel(ref)
	if ref.Done {
		return // the record says it is over
	}
	if ref.Exit {
		r.edit(func(s *StopSummary) { s.ExitOrdersLeft = append(s.ExitOrdersLeft, label) })
		return
	}
	if *budget <= 0 {
		r.edit(func(s *StopSummary) { s.OrdersMayExist = append(s.OrdersMayExist, label+" (not checked: too many)") })
		return
	}
	*budget--
	cctx, cancel := context.WithTimeout(ctx, stepTimeout)
	defer cancel()
	st, err := k.Desk.Lookup(cctx, ref)
	if err != nil || !st.Found {
		if err != nil {
			logger.WarnCF("control", "Could not look up an order of the agent", map[string]any{"workflow": ref.WorkflowID, "error": err.Error()})
		}
		r.edit(func(s *StopSummary) { s.OrdersMayExist = append(s.OrdersMayExist, label) })
		return
	}
	if !st.Open {
		r.edit(func(s *StopSummary) { s.OrdersClosed = append(s.OrdersClosed, label) })
		return
	}
	if err := k.Desk.Cancel(cctx, ref, st.ID); err != nil {
		logger.WarnCF("control", "Could not cancel an order of the agent", map[string]any{"workflow": ref.WorkflowID, "error": err.Error()})
		r.edit(func(s *StopSummary) {
			s.OrdersMayExist = append(s.OrdersMayExist, label+" (still open: cancelling failed)")
		})
		return
	}
	if st.Simulated {
		label += " (simulated)"
	}
	r.edit(func(s *StopSummary) { s.OrdersCancelled = append(s.OrdersCancelled, label) })
}

func orderLabel(ref broker.OrderRef) string {
	side := ref.Side
	if side == "" {
		side = "order"
	}
	return fmt.Sprintf("%s %s on %s", side, ref.Symbol, ref.Provider)
}
