package attention

import (
	"context"
	"errors"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/logger"
)

// Redelivery of a message the deliverer did not take.
//
// A channel that is briefly unreachable must not cost the owner an alert: the
// trigger that raised it has already been committed (a one-shot is switched off),
// so nobody will raise it again. A message that fails is therefore tried again
// after retryDelays[0], then retryDelays[1], and so on; its decisions stay
// reserved (in flight) meanwhile. When the last try fails the decisions are moved
// to the digest, where the owner reads them marked as late. Retries are held in
// memory and driven by tick, so they never block the outbox loop and use the
// outbox's clock.
//
// Two bounds keep a retry inside the decisions' pendingLease, after which their
// reservation lapses and the budget and dedup could count them twice: the delays
// below add up to four and a half minutes, and a message whose oldest decision
// is past maxQueueAge is moved to the digest instead of being tried again (checked
// when the retry is scheduled and again when it runs, because a slow channel can
// spend deliverTimeout on each try). That is why the schedule is shorter than
// the 10 minute lease would suggest.
//
// A protective Critical (a liquidation risk, a breached stop) is the alert that
// must not be given up after a few minutes, and the digest never takes it (see
// DeferToDigest). It is retried on the protective lane: after the ordinary delays
// it keeps trying every protectiveRetryEvery until protectiveRetryWindow has
// passed since its decision, and only then is it released and logged at ERROR. The
// lease bound does not apply to it: it costs no budget slot. Its retries do not
// compete with the ordinary ones for room: they have their own cap.

// retryDelays are the waits before the first, second and third retry. A message
// is tried four times at most (a protective one goes on; see above).
var retryDelays = [...]time.Duration{30 * time.Second, 90 * time.Second, 150 * time.Second}

const (
	// protectiveRetryEvery is the wait between the tries of a protective message
	// once the ordinary delays are used up.
	protectiveRetryEvery = 5 * time.Minute
	// protectiveRetryWindow is how long, from its decision, a protective message
	// is tried before it is released.
	protectiveRetryWindow = time.Hour

	// maxPendingRetries caps the ordinary messages waiting for a retry. Past it a
	// failed message goes straight to the digest, so nothing grows without bound
	// while the channel is down.
	maxPendingRetries = 64
	// maxPendingProtectiveRetries caps the protective messages waiting for a
	// retry, apart from the ordinary cap so that a full ordinary queue never
	// refuses one. A protective alert is superseded by a newer one with its dedup
	// key, so this is generous.
	maxPendingProtectiveRetries = 16
)

// retryEntry is a message waiting for its next try.
type retryEntry struct {
	m        Message
	composed bool
	failed   int // tries that have failed so far
	due      time.Time
	since    time.Time // a protective message: when its retry window started
}

// protective reports whether the message carries a decision that must never be
// moved to the digest.
func (r *retryEntry) protective() bool {
	for _, d := range r.m.Decisions {
		if isProtectiveDecision(d) {
			return true
		}
	}
	return false
}

// isProtectiveDecision reports whether d is a protective alert, which the digest
// never takes.
func isProtectiveDecision(d Decision) bool {
	return isProtectiveCritical(d.Candidate) || d.Reason == ReasonProtective
}

// protectiveSince is when the retry window of a message with protective members
// starts: the time of its oldest protective decision, or now for one without a
// time.
func protectiveSince(decs []Decision, now time.Time) time.Time {
	since := now
	for _, d := range decs {
		if isProtectiveDecision(d) && !d.At.IsZero() && d.At.Before(since) {
			since = d.At
		}
	}
	return since
}

// pastLease reports whether delivering decs at time at would be too close to the
// end of the oldest decision's lease (see maxQueueAge).
func pastLease(decs []Decision, at time.Time) bool {
	for _, d := range decs {
		if !d.At.IsZero() && at.Sub(d.At) > maxQueueAge {
			return true
		}
	}
	return false
}

// deliveryFailed deals with a try that the deliverer did not take; failed counts
// the tries that have failed, this one included. It schedules the next try, or,
// when there is none to schedule, moves the message's decisions to the digest.
func (o *Outbox) deliveryFailed(ctx context.Context, m Message, composed bool, failed int, since time.Time, derr error) error {
	fields := map[string]any{"decisions": len(m.Decisions), "intervention_id": m.InterventionID, "attempt": failed}
	if derr != nil {
		fields["error"] = derr.Error()
	}
	delay, why, ok := o.scheduleRetry(m, composed, failed, since)
	if ok {
		fields["retry_in"] = delay.String()
		logger.WarnCF("attention", "message not delivered; retrying", fields)
		return nil
	}
	if (&retryEntry{m: m}).protective() {
		fields["reason"] = why
		logger.ErrorCF("attention", why, fields)
	} else {
		logger.WarnCF("attention", "message not delivered; moving its decisions to the digest", fields)
	}
	return o.giveUp(ctx, m, composed)
}

// scheduleRetry queues the next try of m and returns the wait. It refuses (ok
// false, with a reason fit for the log) when the tries are used up, when the outbox
// has stopped (nobody would run the retry), when too many retries are pending, or
// when the try would fall past the lease of a decision that can go to the digest.
// A protective message is refused only when its retry window is over, the outbox
// has stopped, or protective retries alone fill their cap. since is the start of
// that window (zero on the first failure).
func (o *Outbox) scheduleRetry(m Message, composed bool, failed int, since time.Time) (delay time.Duration, why string, ok bool) {
	now := o.clock()
	r := &retryEntry{m: m, composed: composed, failed: failed, since: since}
	prot := r.protective()
	switch {
	case failed < 1:
		return 0, "message not delivered; moving its decisions to the digest", false
	case failed <= len(retryDelays):
		delay = retryDelays[failed-1]
	case prot:
		delay = protectiveRetryEvery
	default:
		return 0, "message not delivered; moving its decisions to the digest", false
	}
	r.due = now.Add(delay)
	if prot {
		if r.since.IsZero() {
			r.since = protectiveSince(m.Decisions, now)
		}
		if r.due.Sub(r.since) > protectiveRetryWindow {
			return 0, "protective alert could not be delivered for 1h", false
		}
	} else if pastLease(m.Decisions, r.due) {
		return 0, "message not delivered; moving its decisions to the digest", false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return 0, "outbox stopping; protective alert not delivered", false
	}
	n, limit := 0, maxPendingRetries
	if prot {
		limit = maxPendingProtectiveRetries
	}
	for _, e := range o.hand.retries {
		if e.protective() == prot {
			n++
		}
	}
	if n >= limit {
		return 0, "too many protective alerts waiting for a retry; not delivered", false
	}
	o.hand.retries = append(o.hand.retries, r)
	return delay, "", true
}

// giveUp ends a message that will not be delivered: each decision is moved to the
// digest, or released if it is protective. A decision whose bookkeeping fails is
// left to processDeferrals, which tries again on later ticks. The Observer is told
// the message was not delivered.
func (o *Outbox) giveUp(ctx context.Context, m Message, composed bool) error {
	o.hand.retriesExhausted.Add(1)
	var errs []error
	for _, d := range m.Decisions {
		b := buffered{dec: d}
		release := isProtectiveDecision(d)
		if err := o.settle(ctx, deferral{b: b, release: release}); err != nil {
			errs = append(errs, err)
			o.mu.Lock()
			o.deferLocked([]buffered{b}, release)
			o.mu.Unlock()
		}
	}
	o.observe(Outcome{Message: m, Delivered: false, Composed: composed, At: o.clock()})
	return errors.Join(errs...)
}

// attemptRetry makes one more try at r.m. It runs on a delivery lane (or, for
// Tick, on the caller's goroutine) and is the only place a message is sent a
// second time.
func (o *Outbox) attemptRetry(ctx context.Context, r *retryEntry) error {
	if !r.protective() && pastLease(r.m.Decisions, o.clock()) {
		logger.WarnCF("attention", "message not delivered; too old to try again, moving its decisions to the digest",
			map[string]any{"decisions": len(r.m.Decisions), "intervention_id": r.m.InterventionID, "attempt": r.failed})
		return o.giveUp(ctx, r.m, r.composed)
	}
	o.hand.retried.Add(1)
	if ok, derr := o.deliver(ctx, r.m); !ok {
		return o.deliveryFailed(ctx, r.m, r.composed, r.failed+1, r.since, derr)
	}
	return o.delivered(ctx, r.m, r.composed)
}

// takeDueRetries removes and returns the retries whose wait is over.
func (o *Outbox) takeDueRetries(now time.Time) []*retryEntry {
	o.mu.Lock()
	defer o.mu.Unlock()
	var due []*retryEntry
	keep := o.hand.retries[:0:0]
	for _, r := range o.hand.retries {
		if r.due.After(now) {
			keep = append(keep, r)
		} else {
			due = append(due, r)
		}
	}
	o.hand.retries = keep
	return due
}

// handOverDueRetries queues the retries whose wait is over on a delivery lane: a
// protective one on the protective lane, the rest on the ordinary lane. It only
// touches memory.
func (o *Outbox) handOverDueRetries(now time.Time) {
	h := o.hand
	o.mu.Lock()
	defer o.mu.Unlock()
	keep := h.retries[:0:0]
	for _, r := range h.retries {
		switch {
		case r.due.After(now):
			keep = append(keep, r)
		case r.protective():
			h.protRetry = append(h.protRetry, r)
			signal(h.protWake)
		default:
			h.ord = append(h.ord, laneJob{retry: r})
			signal(h.ordWake)
		}
	}
	h.retries = keep
}

// takeProtectiveRetry pops one retry from the protective lane.
func (o *Outbox) takeProtectiveRetry() *retryEntry {
	h := o.hand
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(h.protRetry) == 0 {
		return nil
	}
	r := h.protRetry[0]
	h.protRetry = h.protRetry[1:]
	if len(h.protRetry) == 0 {
		h.protRetry = nil
	}
	h.inflight++
	return r
}

// drainRetries is shutdown's last word on the retries still pending, whether they
// are waiting or were handed to a lane that has stopped: none is tried again, and
// their decisions are moved to the digest (or released) so that nothing a restart
// would lose is left only in memory.
func (o *Outbox) drainRetries(ctx context.Context) {
	h := o.hand
	o.mu.Lock()
	rs := append([]*retryEntry(nil), h.retries...)
	rs = append(rs, h.protRetry...)
	h.retries, h.protRetry = nil, nil
	ord := h.ord[:0:0]
	for _, j := range h.ord {
		if j.retry != nil {
			rs = append(rs, j.retry)
		} else {
			ord = append(ord, j)
		}
	}
	h.ord = ord
	o.mu.Unlock()

	for _, r := range rs {
		fields := map[string]any{"decisions": len(r.m.Decisions), "intervention_id": r.m.InterventionID, "attempt": r.failed}
		if r.protective() {
			logger.ErrorCF("attention", "outbox stopping; protective alert not delivered, releasing it", fields)
		} else {
			logger.WarnCF("attention", "outbox stopping; moving the decisions of an undelivered message to the digest", fields)
		}
		if err := o.giveUp(ctx, r.m, r.composed); err != nil {
			logger.WarnCF("attention", "final retry fallback failed", map[string]any{"error": err.Error()})
		}
	}
}
