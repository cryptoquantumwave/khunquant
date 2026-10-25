package approval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/identity"
	"github.com/cryptoquantumwave/khunquant/pkg/journal"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/notify"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// Definitions returns the journal definitions this package brings: register them
// with the gateway's journal registry. Approvals off means the gateway never
// builds the service, so the registry gets none.
func (s *Service) Definitions() []journal.Definition {
	return []journal.Definition{{Kind: WorkflowKind, Run: s.runOrder}}
}

// Tags the place step puts in front of the error text it journals, so that the
// text alone (all a replay and the sweeper have) says whether the order may exist.
const (
	tagNone  = "[order-not-placed] "
	tagMaybe = "[order-may-exist] "
)

var (
	// errRateLimited: the provider's rate limiter refused; the throttle step retries.
	errRateLimited = errors.New("approval: the provider's order rate limit is spent")
	// errStale: the approval is older than the proposal's own window.
	errStale = errors.New("approval: the approval is too old")
)

const (
	throttleRetries = 5
	throttleBackoff = 20 * time.Second
	placeRetries    = 3
)

// runOrder is the approval.order workflow: recheck, throttle, place, verify,
// record, receipt. The only input is the proposal id; every parameter comes from
// the stored row, re-read and re-verified at every step that needs it.
func (s *Service) runOrder(c *journal.Ctx, input json.RawMessage) error {
	var in workflowInput
	if err := json.Unmarshal(input, &in); err != nil || in.ProposalID == "" {
		return errors.New("approval: the workflow input is not a proposal id")
	}
	err := s.runSteps(c, in.ProposalID)
	var stepErr *journal.StepError
	if !errors.As(err, &stepErr) {
		return err // nil, or the workflow cannot go on in this process (suspended, cancelled, lease lost)
	}
	// A step failed for good. Tell the owner now, in a step of its own; if the
	// failure was a panic the engine refuses further steps and the sweeper does it.
	if serr := c.Step("settle_failure", nil, func(ctx context.Context, _ int) (any, error) {
		p, gerr := s.st.get(ctx, in.ProposalID)
		if gerr != nil {
			return nil, nil
		}
		s.settleFailed(ctx, p, s.describeFailure(ctx, s.workflows(), p, journal.Workflow{Error: err.Error()}))
		return nil, nil
	}); serr != nil {
		return err
	}
	return err
}

func (s *Service) runSteps(c *journal.Ctx, id string) error {
	var rc Rechecked
	if err := c.Step("recheck", &rc, func(ctx context.Context, _ int) (any, error) {
		return s.stepRecheck(ctx, id)
	}); err != nil {
		return err
	}
	if err := c.Step("throttle", nil, func(ctx context.Context, _ int) (any, error) {
		return nil, s.stepThrottle(ctx, id)
	}, journal.Retries(throttleRetries), journal.Backoff(throttleBackoff),
		journal.NonRetryable(func(e error) bool { return !errors.Is(e, errRateLimited) })); err != nil {
		return err
	}
	var placed Placed
	if err := c.Step("place", &placed, func(ctx context.Context, attempt int) (any, error) {
		return s.stepPlace(ctx, c, id, attempt, rc)
	}, journal.Retries(placeRetries), journal.Backoff(broker.ReconcileSettleDelay),
		journal.NonRetryable(func(e error) bool { return !broker.Retryable(e) })); err != nil {
		return err
	}
	var fill Fill
	if err := c.Step("verify", &fill, func(ctx context.Context, _ int) (any, error) {
		return s.stepVerify(ctx, id, placed), nil
	}); err != nil {
		return err
	}
	if err := c.Step("record", nil, func(ctx context.Context, _ int) (any, error) {
		return nil, s.stepRecord(ctx, id, rc, placed, fill)
	}); err != nil {
		return err
	}
	return c.Step("receipt", nil, func(ctx context.Context, _ int) (any, error) {
		return nil, s.stepReceipt(ctx, id, rc, placed, fill)
	})
}

// executable is the proposal a step may act on, re-read and re-proven: it is
// approved or executing, was decided by the CURRENT owner, and its params match
// its hash exactly.
func (s *Service) executable(ctx context.Context, id string) (Proposal, OrderParams, error) {
	p, err := s.st.get(ctx, id)
	if err != nil {
		return Proposal{}, OrderParams{}, err
	}
	if p.Status != StatusApproved && p.Status != StatusExecuting {
		return Proposal{}, OrderParams{}, fmt.Errorf("approval: proposal %s is %s, not approved", p.Code, p.Status)
	}
	ownerID, ok := s.o.Cfg.OwnerDecider(p.DecidedBy)
	if !ok || p.DecidedAt == nil || !identity.SameSender(p.DecidedBy, ownerID) {
		return Proposal{}, OrderParams{}, fmt.Errorf("approval: proposal %s was not decided by the configured owner", p.Code)
	}
	op, err := decodeVerified(p)
	if err != nil {
		return Proposal{}, OrderParams{}, err
	}
	return p, op, nil
}

// stale reports whether the approval is older than the proposal's own window: the
// owner agreed to an order now, not to the same order after a gateway outage.
func (s *Service) stale(p Proposal) error {
	window := p.ExpiresAt.Sub(p.CreatedAt)
	age := s.now().Sub(*p.DecidedAt)
	if age <= window {
		return nil
	}
	return fmt.Errorf("%w: it was given %s ago and an approval is good for %s. Nothing was placed. "+
		"A reduce-only order is not exempt in this change (a stale exit at a stale size can be wrong too): propose it again if you still want it",
		errStale, age.Round(time.Second), window)
}

func (s *Service) stepRecheck(ctx context.Context, id string) (Rechecked, error) {
	p, op, err := s.executable(ctx, id)
	if err != nil {
		return Rechecked{}, err
	}
	if err := s.stale(p); err != nil {
		return Rechecked{}, err
	}
	rc, err := s.o.Backend.Recheck(ctx, op)
	if err != nil {
		return Rechecked{}, err
	}
	if rc.Mode != p.Mode {
		// The card said how the order would be placed (PAPER, REAL...). An approval
		// of a simulated order is not an approval of a real one, nor the reverse.
		return Rechecked{}, fmt.Errorf("the trading mode changed since the proposal (it was %s, it is now %s)", p.Mode, rc.Mode)
	}
	if p.Reducing && !rc.Reducing {
		// The card promised an exit that is not abandoned for a price move. An order
		// that no longer only reduces risk is not that order, and gets the collar.
		return Rechecked{}, errors.New("the order no longer only reduces risk, so it is not the exit the owner approved")
	}
	if p.Origin == OriginProactive && !rc.Reducing {
		return Rechecked{}, ErrNotRiskReducing
	}
	if s.killed() && !rc.Reducing {
		return Rechecked{}, ErrKilled
	}
	return rc, nil
}

func (s *Service) stepThrottle(ctx context.Context, id string) error {
	p, op, err := s.executable(ctx, id)
	if err != nil {
		return err
	}
	if err := s.stale(p); err != nil {
		return err
	}
	if ok, after := s.o.Backend.Throttle(op); !ok {
		return fmt.Errorf("%w (it refills in about %s)", errRateLimited, after.Round(time.Second))
	}
	return nil
}

// stepPlace places the order. Attempt 1 means no earlier attempt was ever sent, so
// a refusal here is "nothing was placed"; on a later attempt an earlier one may
// have reached the exchange, so a refusal there says the order may exist.
func (s *Service) stepPlace(ctx context.Context, c *journal.Ctx, id string, attempt int, rc Rechecked) (Placed, error) {
	refuse := func(err error) (Placed, error) {
		if attempt > 1 {
			return Placed{}, fmt.Errorf("%s%w", tagMaybe, err)
		}
		return Placed{}, fmt.Errorf("%s%w", tagNone, err)
	}
	p, op, err := s.executable(ctx, id)
	if err != nil {
		return refuse(err)
	}
	if err := s.stale(p); err != nil {
		return refuse(err)
	}
	// Every attempt reads the flag, not only the first: a retry can place (a capable
	// adapter that finds no order by its client id places one), so after /stop an order
	// that adds risk must not go out on a retry either. On a retry the refusal says the
	// order may exist (refuse tags it so), and the owner is told to check the exchange.
	// An order that only reduces risk is never stopped by the flag.
	if s.killed() && !rc.Reducing {
		return refuse(ErrKilled)
	}
	placed, err := s.o.Backend.PlaceApprovedOrder(ctx, op, c.IdempotencyKey("place"), attempt, c.StepStartedAt(), rc)
	if err != nil {
		tag := tagNone
		for _, k := range []error{broker.ErrOutcomeUnknown, broker.ErrReconcileUnavailable, broker.ErrReconcileAmbiguous, broker.ErrReconcileUnproven} {
			if errors.Is(err, k) {
				tag = tagMaybe
			}
		}
		return Placed{}, fmt.Errorf("%s%w", tag, err)
	}
	return placed, nil
}

// stepVerify reads the order back. It never fails the workflow: the order has
// been placed, and a fill that cannot be confirmed is reported as such.
func (s *Service) stepVerify(ctx context.Context, id string, placed Placed) Fill {
	p, err := s.st.get(ctx, id)
	if err != nil || placed.OrderID == "" {
		return Fill{}
	}
	op, err := decodeVerified(p)
	if err != nil {
		return Fill{}
	}
	last := placed.Fill // the answer to the placement, until a fetch says better
	for i := 0; i < s.o.VerifyPolls; i++ {
		if f, err := s.o.Backend.Fetch(ctx, op, placed.OrderID); err == nil {
			last = f
			if f.Done || op.Type == "limit" {
				break
			}
		}
		if i == s.o.VerifyPolls-1 {
			break
		}
		if s.o.VerifyPollInterval > 0 {
			select {
			case <-ctx.Done():
				return last
			case <-s.o.Clock.After(s.o.VerifyPollInterval):
			}
		}
	}
	return last
}

func (s *Service) stepRecord(ctx context.Context, id string, rc Rechecked, placed Placed, fill Fill) error {
	p, err := s.st.get(ctx, id)
	if err != nil {
		return nil
	}
	if op, err := decodeVerified(p); err == nil {
		s.o.Backend.Record(ctx, op, rc, placed, fill)
	}
	return nil
}

func (s *Service) stepReceipt(ctx context.Context, id string, rc Rechecked, placed Placed, fill Fill) error {
	p, err := s.st.get(ctx, id)
	if err != nil {
		return nil
	}
	op, err := decodeVerified(p)
	if err != nil {
		return nil
	}
	s.settleExecuted(ctx, p, receiptText(p, op, rc, placed, fill))
	return nil
}

// settleExecuted records the final status of a placed order and tells the owner.
func (s *Service) settleExecuted(ctx context.Context, p Proposal, text string) {
	if text == "" {
		text = msgs[langOf(p.Lang)].executedGeneric(p.Code)
	}
	if changed, err := s.st.finish(ctx, p.ID, StatusExecuted, text, s.now()); err != nil {
		logger.WarnCF(logComponent, "Could not record the executed status", map[string]any{"code": p.Code, "error": err.Error()})
		return
	} else if changed {
		p.Status, p.Outcome = StatusExecuted, text
	}
	s.notifyOutcome(ctx, p)
}

// failure is why an execution ended without an order, in words for the owner.
type failure struct {
	reason   string
	mayExist bool
}

// settleFailed records the failure and tells the owner. It is idempotent: only
// the call that moves the row out of executing writes the outcome.
func (s *Service) settleFailed(ctx context.Context, p Proposal, f failure) {
	op, _ := decodeVerified(p)
	text := failureText(p, op, f)
	changed, err := s.st.finish(ctx, p.ID, StatusFailed, text, s.now())
	if err != nil {
		logger.WarnCF(logComponent, "Could not record the failed status", map[string]any{"code": p.Code, "error": err.Error()})
		return
	}
	if !changed {
		return
	}
	p.Status, p.Outcome = StatusFailed, text
	s.notifyOutcome(ctx, p)
}

// notifyOutcome tells the owner how an execution ended and marks it told. A
// message that could not be delivered stays unmarked, and Recover sends it again.
func (s *Service) notifyOutcome(ctx context.Context, p Proposal) {
	if cur, err := s.st.get(ctx, p.ID); err == nil && cur.Notified {
		return // a replay of a step that already told the owner
	}
	sev := proactive.SeverityNotice
	title := msgs[langOf(p.Lang)].executedTitle(p.Code)
	if p.Status == StatusFailed {
		sev = proactive.SeverityWarning
		title = msgs[langOf(p.Lang)].failedTitle(p.Code)
	}
	res, err := s.o.Notifier.Deliver(context.WithoutCancel(ctx), notify.Notification{
		Severity: sev, Title: title, Body: p.Outcome, DedupKey: "approval-outcome:" + p.ID, InterventionID: p.InterventionID,
	})
	if err != nil || !res.Delivered {
		logger.WarnCF(logComponent, "The outcome of an approved order could not be delivered; it will be sent again",
			map[string]any{"code": p.Code, "status": string(p.Status)})
		return
	}
	if err := s.st.markNotified(context.WithoutCancel(ctx), p.ID); err != nil {
		logger.WarnCF(logComponent, "Could not mark an outcome as sent", map[string]any{"code": p.Code, "error": err.Error()})
	}
}

// renotify sends again the outcome of every finished execution the owner has not
// been told about (the process died between the final status and the message).
func (s *Service) renotify(ctx context.Context) {
	rows, err := s.st.list(ctx, `status IN ('executed','failed') AND notified = 0 AND finished_at_ms > ? LIMIT 20`,
		ms(s.now().Add(-24*time.Hour)))
	if err != nil {
		return
	}
	for _, p := range rows {
		s.notifyOutcome(ctx, p)
	}
}

// describeFailure says why an execution ended without a completed order, from the
// journal alone (all a replay and the sweeper have): the step that failed, the tag
// the place step left on its error, and whether the place step ever started.
func (s *Service) describeFailure(ctx context.Context, wf Workflows, p Proposal, w journal.Workflow) failure {
	var steps []journal.StepRecord
	if wf != nil && p.WorkflowID != "" {
		steps, _ = wf.Steps(ctx, p.WorkflowID)
	} else if wf != nil {
		steps, _ = wf.Steps(ctx, WorkflowID(p.ID))
	}
	var failed *journal.StepRecord
	placeStarted := false
	for i := range steps {
		r := &steps[i]
		if r.Name == "place" {
			placeStarted = true
		}
		if r.Error != "" && r.Output == nil && r.Name != "settle_failure" {
			failed = r
		}
	}
	if failed == nil {
		if placeStarted {
			return failure{reason: msgs[langOf(p.Lang)].stoppedWhilePlacing, mayExist: true}
		}
		reason := w.Error
		if reason == "" {
			reason = msgs[langOf(p.Lang)].stoppedBeforePlacing
		}
		return failure{reason: summarize(reason)}
	}
	msg := failed.Error
	mayExist := false
	if failed.Name == "place" {
		mayExist = !strings.Contains(msg, strings.TrimSpace(tagNone))
	}
	return failure{reason: summarize(msg), mayExist: mayExist}
}

// summarize makes a journaled error readable for the owner: the tags and the
// journal's own prefix are removed and the rest is cut, so an exchange's long
// answer is not forwarded wholesale.
func summarize(msg string) string {
	msg = logger.Redact(msg)
	for _, t := range []string{tagNone, tagMaybe} {
		msg = strings.ReplaceAll(msg, t, "")
	}
	if i := strings.Index(msg, "failed after "); i >= 0 {
		if j := strings.Index(msg[i:], "): "); j >= 0 {
			msg = msg[i+j+3:]
		}
	}
	msg = strings.Join(strings.Fields(msg), " ")
	if r := []rune(msg); len(r) > 240 {
		msg = string(r[:239]) + "..."
	}
	return msg
}
