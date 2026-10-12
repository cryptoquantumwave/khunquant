package attention

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// The hand-over side of the outbox.
//
// Submit decides a candidate and, for a protective Critical or a closed window,
// delivers on the caller's goroutine. That is right for the heartbeat, and wrong
// for the proactive pipeline, whose single goroutine must never wait on a
// channel, on the owner's session or on the network. Accept is the entry point
// for a caller that has already decided (Decide on the pipeline goroutine is a
// local transaction): it takes a Decision and an Item, touches nothing but
// memory under a short lock, and returns. Delivery happens on the two lane
// workers that Run starts:
//
//   - the ordinary lane carries a group whose window closed, the daily digest and
//     everything else that may wait. It holds up to ordinaryLaneSlots whole
//     messages, one worker delivers them in order, and it is the only lane that
//     calls the Composer;
//   - the protective lane carries a protective Critical together with whatever
//     was buffered. Its own worker never shares a goroutine with the ordinary
//     lane, so a slow channel holding an ordinary message cannot delay it, and it
//     never consults the Composer (a model turn must not stand between a
//     liquidation warning and the owner). It holds one pending group, coalesced
//     by dedup key, so a stuck channel and a repeating trigger cannot grow it
//     without bound while nothing is dropped.
//
// A queued message must still be inside its pendingLease when it is delivered,
// or its reservation lapses while it waits and the budget and dedup double-count
// it. The ordinary lane therefore defers a message that has waited longer than
// maxQueueAge to the digest instead of sending it late; a full lane defers the
// message it cannot take, the same way. Deferral is a database write, so it is
// done by Tick (and by the worker), never by Accept. A protective message is
// never deferred.
//
// Submit, Tick and everything they did before keep working as they were: Tick is
// still the inline, caller-goroutine form. Only Run hands over.

const (
	// ordinaryLaneSlots is how many messages the ordinary lane holds.
	ordinaryLaneSlots = 256

	// maxQueueAge is the longest an ordinary message may wait in the lane. It is
	// pendingLease less the longest delivery (composeTimeout + deliverTimeout)
	// with a margin: past it the message would be delivered outside its lease.
	maxQueueAge = 6 * time.Minute

	// composeTimeout bounds one Composer call.
	composeTimeout = 90 * time.Second

	// maxDeferBacklog caps decisions waiting for Tick to defer or release them.
	// Past it a decision is abandoned: its reservation lapses with pendingLease,
	// exactly as after a crash.
	maxDeferBacklog = 1024

	// deferTries is how often a failing deferral is retried on later ticks.
	deferTries = 3

	// laneStopGrace is how long Run waits for the workers after the final flush
	// window ended and their delivery context was cancelled.
	laneStopGrace = time.Second

	// panicWarnEvery limits how often a recovered panic is logged.
	panicWarnEvery = time.Minute
)

// Handoff is what Accept did with a decision.
type Handoff int

const (
	// HandoffIgnored: the decision is not a notify or escalate, so there is
	// nothing to deliver (a digest decision is already queued by the governor).
	HandoffIgnored Handoff = iota
	// HandoffBuffered: the decision waits in the group buffer for its window.
	HandoffBuffered
	// HandoffQueued: a protective Critical; it and the buffered group are queued
	// on the protective lane.
	HandoffQueued
	// HandoffClosed: the outbox has stopped. Nothing was done and nothing was
	// delivered; the decision's reservation lapses with pendingLease.
	HandoffClosed
)

// String returns the lower-case name of h.
func (h Handoff) String() string {
	switch h {
	case HandoffIgnored:
		return "ignored"
	case HandoffBuffered:
		return "buffered"
	case HandoffQueued:
		return "queued"
	case HandoffClosed:
		return "closed"
	}
	return fmt.Sprintf("handoff(%d)", int(h))
}

// DigestDeferrer is implemented by a gate that can move an in-flight notify or
// escalate decision to the digest queue. *Governor does. A gate without it gets
// the decision released instead (it can be decided again).
type DigestDeferrer interface {
	DeferToDigest(ctx context.Context, d Decision) error
}

var _ DigestDeferrer = (*Governor)(nil)

// OutboxStats are the outbox's hand-over counters. The counters count since the
// outbox was built; the three depths are gauges.
type OutboxStats struct {
	// OrdinaryHandOvers and ProtectiveHandOvers count messages queued on each lane.
	OrdinaryHandOvers   uint64
	ProtectiveHandOvers uint64
	// Deferred counts decisions moved to the digest because the ordinary lane was
	// full or they waited past maxQueueAge.
	Deferred uint64
	// Aged counts messages that waited past maxQueueAge (their decisions are in
	// Deferred).
	Aged uint64
	// DeferralFailed counts decisions whose deferral failed for good and whose
	// reservation therefore lapses.
	DeferralFailed uint64
	// Abandoned counts decisions dropped from a full deferral backlog.
	Abandoned uint64
	// Superseded counts protective decisions replaced by a newer one with the same
	// dedup key before they were delivered.
	Superseded uint64
	// RejectedClosed counts Accepts after the outbox stopped.
	RejectedClosed uint64
	// Panics counts recovered panics in a deliverer, a composer or a lane worker.
	Panics uint64

	OrdinaryQueued    int
	ProtectivePending int
	DeferBacklog      int
}

type laneJob struct {
	group  []buffered
	digest *Digest
}

type deferral struct {
	b       buffered
	release bool // release instead of defer: the decision was superseded
	tries   int
}

// handoffState is the part of the outbox's state the lanes use. queues are
// guarded by Outbox.mu; the counters are atomic.
type handoffState struct {
	ord      []laneJob
	prot     []buffered
	deferred []deferral
	inflight int

	ordWake  chan struct{}
	protWake chan struct{}

	ordinaryHandOvers, protectiveHandOvers atomic.Uint64
	deferredN, aged, deferralFailed        atomic.Uint64
	abandoned, superseded, rejectedClosed  atomic.Uint64
	panics                                 atomic.Uint64

	panicMu   sync.Mutex
	lastPanic time.Time
}

func newHandoffState() *handoffState {
	return &handoffState{ordWake: make(chan struct{}, 1), protWake: make(chan struct{}, 1)}
}

func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// Decide asks the governor to decide c without delivering anything. A caller that
// decides itself (the proactive pipeline) hands the result to Accept; one that
// wants the outbox to do both uses Submit.
func (o *Outbox) Decide(ctx context.Context, c proactive.Candidate) (Decision, error) {
	return o.g.Decide(ctx, c)
}

// SetTickInterval sets how often Run ticks (one second by default). Call it
// before Run; the group window and the digest are measured by the outbox's clock,
// so this only changes how soon after they are due they are noticed.
func (o *Outbox) SetTickInterval(d time.Duration) {
	if d <= 0 {
		return
	}
	o.mu.Lock()
	o.tickEvery = d
	o.mu.Unlock()
}

// Stats returns the hand-over counters.
func (o *Outbox) Stats() OutboxStats {
	h := o.hand
	o.mu.Lock()
	s := OutboxStats{
		OrdinaryQueued:    len(h.ord),
		ProtectivePending: len(h.prot),
		DeferBacklog:      len(h.deferred),
	}
	o.mu.Unlock()
	s.OrdinaryHandOvers = h.ordinaryHandOvers.Load()
	s.ProtectiveHandOvers = h.protectiveHandOvers.Load()
	s.Deferred = h.deferredN.Load()
	s.Aged = h.aged.Load()
	s.DeferralFailed = h.deferralFailed.Load()
	s.Abandoned = h.abandoned.Load()
	s.Superseded = h.superseded.Load()
	s.RejectedClosed = h.rejectedClosed.Load()
	s.Panics = h.panics.Load()
	return s
}

// Accept takes a decision the caller has already made (see Decide) and puts it
// on the way to the owner. It never decides again, never calls the governor, the
// deliverer or the composer, and never waits on anything but a mutex held for a
// few assignments: it is safe to call from a goroutine that must not do I/O.
//
// A notify or escalate decision joins the group buffer under the same window rules
// as Submit, so grouping and the daily budget behave exactly as they do there. A
// group whose window closed, or that outgrew the buffer, is queued on the
// ordinary lane (or deferred, if that is full); a protective Critical queues
// itself and the buffered group on the protective lane. Any other decision is
// ignored. After the outbox stopped, Accept does nothing: unlike Submit it does
// not deliver inline.
//
// Delivery needs Run: without it queued messages wait. Only Run's workers
// deliver them, and Run's final flush delivers what is still queued.
func (o *Outbox) Accept(d Decision, it Item) Handoff {
	if d.Disposition != proactive.DispNotify && d.Disposition != proactive.DispEscalate {
		return HandoffIgnored
	}
	it.Candidate = d.Candidate
	h := o.hand
	now := o.clock()

	var expired []buffered
	result := HandoffBuffered
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		h.rejectedClosed.Add(1)
		return HandoffClosed
	}
	if len(o.buf) > 0 && o.window > 0 && !now.Before(o.windowStart.Add(o.window)) {
		expired = o.takeLocked() // the window closed before a tick got to it
	}
	if len(o.buf) >= maxBuffered && expired == nil {
		expired = o.takeLocked() // nothing may grow without bound if ticks stop
	}
	if len(o.buf) == 0 {
		o.windowStart = now
	}
	o.buf = append(o.buf, buffered{dec: d, item: it})
	if isProtectiveCritical(d.Candidate) {
		for _, b := range o.takeLocked() {
			o.addProtectiveLocked(b)
		}
		result = HandoffQueued
	}
	if expired != nil {
		o.pushOrdinaryLocked(expired)
	}
	o.mu.Unlock()
	return result
}

// handOverOrdinary queues a due group on the ordinary lane.
func (o *Outbox) handOverOrdinary(group []buffered) {
	o.mu.Lock()
	o.pushOrdinaryLocked(group)
	o.mu.Unlock()
}

// pushOrdinaryLocked queues group on the ordinary lane. A protective member is
// routed to the protective lane instead; what the lane has no slot for is
// deferred. The caller holds o.mu.
func (o *Outbox) pushOrdinaryLocked(group []buffered) {
	h := o.hand
	rest := group[:0:0]
	for _, b := range group {
		if isProtectiveCritical(b.dec.Candidate) {
			o.addProtectiveLocked(b)
			continue
		}
		rest = append(rest, b)
	}
	if len(rest) == 0 {
		return
	}
	if len(h.ord) >= o.ordSlots {
		o.deferLocked(rest, false)
		return
	}
	h.ord = append(h.ord, laneJob{group: rest})
	h.ordinaryHandOvers.Add(1)
	signal(h.ordWake)
}

// addProtectiveLocked adds b to the pending protective group. A pending decision
// with the same dedup key is superseded by the newer one and released: the owner
// needs one line about it, not one per repeat. The caller holds o.mu.
func (o *Outbox) addProtectiveLocked(b buffered) {
	h := o.hand
	key := b.dec.Candidate.DedupKey
	for i, e := range h.prot {
		if e.dec.Candidate.DedupKey == key {
			h.prot[i] = b
			h.superseded.Add(1)
			o.deferLocked([]buffered{e}, true)
			signal(h.protWake)
			return
		}
	}
	h.prot = append(h.prot, b)
	h.protectiveHandOvers.Add(1)
	signal(h.protWake)
}

// deferLocked records decisions for Tick to defer to the digest (or, for
// release, to release). Past maxDeferBacklog a decision is abandoned. The caller
// holds o.mu.
func (o *Outbox) deferLocked(items []buffered, release bool) {
	h := o.hand
	for _, b := range items {
		if len(h.deferred) >= maxDeferBacklog {
			h.abandoned.Add(1)
			continue
		}
		h.deferred = append(h.deferred, deferral{b: b, release: release})
	}
}

// processDeferrals defers (or releases) what the hand-over could not deliver. It
// is database work only and runs on Tick's goroutine.
func (o *Outbox) processDeferrals(ctx context.Context) error {
	h := o.hand
	o.mu.Lock()
	todo := h.deferred
	h.deferred = nil
	o.mu.Unlock()
	if len(todo) == 0 {
		return nil
	}
	var errs []error
	var again []deferral
	for _, d := range todo {
		if err := o.settle(ctx, d); err != nil {
			d.tries++
			if d.tries < deferTries {
				again = append(again, d)
			} else {
				h.deferralFailed.Add(1)
			}
			errs = append(errs, err)
		}
	}
	if len(again) > 0 {
		o.mu.Lock()
		for _, d := range again {
			if len(h.deferred) < maxDeferBacklog {
				h.deferred = append(h.deferred, d)
			} else {
				h.abandoned.Add(1)
			}
		}
		o.mu.Unlock()
	}
	return errors.Join(errs...)
}

// settle moves one decision to the digest, or releases it.
func (o *Outbox) settle(ctx context.Context, d deferral) error {
	bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), bookkeepTimeout)
	defer cancel()
	if !d.release {
		if df, ok := o.g.(DigestDeferrer); ok {
			if err := df.DeferToDigest(bctx, d.b.dec); err != nil {
				return fmt.Errorf("defer decision %s: %w", d.b.dec.ID, err)
			}
			o.hand.deferredN.Add(1)
			return nil
		}
	}
	if err := o.g.MarkDelivered(bctx, d.b.dec, Delivery{Delivered: false}); err != nil {
		return fmt.Errorf("release decision %s: %w", d.b.dec.ID, err)
	}
	return nil
}

// handOverDigest is the hand-over form of tickDigest: it asks whether a digest is
// due and queues it on the ordinary lane. If the lane has no slot the digest is
// not lost: the governor hands it out again after digestRetry.
func (o *Outbox) handOverDigest(ctx context.Context, now time.Time) error {
	dg, err := o.pendingDigest(ctx, now)
	if err != nil || dg == nil {
		return err
	}
	h := o.hand
	o.mu.Lock()
	if len(h.ord) >= o.ordSlots {
		o.mu.Unlock()
		o.digestDone()
		logger.WarnCF("attention", "digest not handed over: the ordinary lane is full; it is retried later", nil)
		return nil
	}
	h.ord = append(h.ord, laneJob{digest: dg})
	signal(h.ordWake)
	o.mu.Unlock()
	return nil
}

// ---- lanes -----------------------------------------------------------------

type lanes struct {
	cancel context.CancelFunc
	ctx    context.Context
	stop   chan struct{}
	wg     sync.WaitGroup
}

// startLanes starts the two workers. Their delivery context is not Run's: it
// outlives the cancellation that stops Run, so that the final flush can deliver,
// and is cancelled by shutdown once the flush window has passed.
func (o *Outbox) startLanes() *lanes {
	ctx, cancel := context.WithCancel(context.Background())
	l := &lanes{cancel: cancel, ctx: ctx, stop: make(chan struct{})}
	h := o.hand
	l.wg.Add(2)
	go func() {
		defer l.wg.Done()
		o.laneLoop(l.stop, h.ordWake, func() bool { return o.ordinaryStep(l.ctx) })
	}()
	go func() {
		defer l.wg.Done()
		o.laneLoop(l.stop, h.protWake, func() bool { return o.protectiveStep(l.ctx) })
	}()
	return l
}

// laneLoop runs step until stop is closed and step has nothing left to do. step
// reports whether it did any work.
func (o *Outbox) laneLoop(stop <-chan struct{}, wake <-chan struct{}, step func() bool) {
	for {
		if step() {
			continue
		}
		select {
		case <-wake:
		case <-stop:
			for step() {
			}
			return
		}
	}
}

// shutdown stops accepting, delivers what is buffered and queued once and waits
// (bounded) for the workers.
func (o *Outbox) shutdown(ctx context.Context, l *lanes) {
	h := o.hand
	o.mu.Lock()
	o.closed = true
	if out := o.takeLocked(); out != nil {
		// Past the slot limit on purpose: this is the last chance to say it.
		rest := out[:0:0]
		for _, b := range out {
			if isProtectiveCritical(b.dec.Candidate) {
				o.addProtectiveLocked(b)
			} else {
				rest = append(rest, b)
			}
		}
		if len(rest) > 0 {
			h.ord = append(h.ord, laneJob{group: rest})
		}
	}
	o.mu.Unlock()

	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownFlushTimeout)
	defer cancel()
	// A delivery still running when the flush window ends is cancelled; whatever
	// it did not send is released by flush's bookkeeping.
	stopFlush := context.AfterFunc(fctx, l.cancel)
	defer stopFlush()

	close(l.stop)
	signal(h.ordWake)
	signal(h.protWake)
	done := make(chan struct{})
	go func() {
		l.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(shutdownFlushTimeout + laneStopGrace):
		logger.WarnCF("attention", "delivery lanes did not stop in time; a deliverer is ignoring its context", nil)
	}
	l.cancel()
	if err := o.processDeferrals(fctx); err != nil {
		logger.WarnCF("attention", "final deferral failed", map[string]any{"error": err.Error()})
	}
}

func (o *Outbox) popOrdinary() (laneJob, bool) {
	h := o.hand
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(h.ord) == 0 {
		return laneJob{}, false
	}
	j := h.ord[0]
	h.ord[0] = laneJob{}
	h.ord = h.ord[1:]
	if len(h.ord) == 0 {
		h.ord = nil
	}
	h.inflight++
	return j, true
}

func (o *Outbox) takeProtective() []buffered {
	h := o.hand
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(h.prot) == 0 {
		return nil
	}
	n := min(len(h.prot), maxBuffered)
	out := append([]buffered(nil), h.prot[:n]...)
	h.prot = append([]buffered(nil), h.prot[n:]...)
	if len(h.prot) == 0 {
		h.prot = nil
	}
	h.inflight++
	return out
}

func (o *Outbox) jobDone() {
	o.mu.Lock()
	o.hand.inflight--
	o.mu.Unlock()
}

// idle reports whether nothing is queued, being delivered or waiting to be
// deferred. It is for tests.
func (o *Outbox) idle() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	h := o.hand
	return len(h.ord) == 0 && len(h.prot) == 0 && len(h.deferred) == 0 && h.inflight == 0
}

// ordinaryStep delivers one queued ordinary job. It reports whether there was one.
func (o *Outbox) ordinaryStep(ctx context.Context) bool {
	j, ok := o.popOrdinary()
	if !ok {
		return false
	}
	defer o.jobDone()
	defer o.recoverLane("ordinary lane")
	if j.digest != nil {
		defer o.digestDone()
		if err := o.sendDigest(ctx, j.digest); err != nil {
			logger.WarnCF("attention", "digest bookkeeping failed", map[string]any{"error": err.Error()})
		}
		return true
	}
	if o.tooOld(j.group) {
		o.hand.aged.Add(1)
		for _, b := range j.group {
			if err := o.settle(ctx, deferral{b: b}); err != nil {
				o.hand.deferralFailed.Add(1)
				logger.WarnCF("attention", "could not defer a late message", map[string]any{"error": err.Error()})
			}
		}
		return true
	}
	if err := o.flush(ctx, j.group); err != nil {
		logger.WarnCF("attention", "ordinary delivery bookkeeping failed", map[string]any{"error": err.Error()})
	}
	return true
}

// protectiveStep delivers the pending protective group, without the composer.
func (o *Outbox) protectiveStep(ctx context.Context) bool {
	g := o.takeProtective()
	if g == nil {
		return false
	}
	defer o.jobDone()
	defer o.recoverLane("protective lane")
	if err := o.flushWith(ctx, g, false); err != nil {
		logger.WarnCF("attention", "protective delivery bookkeeping failed", map[string]any{"error": err.Error()})
	}
	return true
}

// tooOld reports whether the oldest decision of group has waited past
// maxQueueAge, which is to say close enough to its lease that sending it now
// could outlive the reservation. A decision without a time is never too old.
func (o *Outbox) tooOld(group []buffered) bool {
	now := o.clock()
	for _, b := range group {
		if !b.dec.At.IsZero() && now.Sub(b.dec.At) > maxQueueAge {
			return true
		}
	}
	return false
}

// recoverLane keeps a panic in a lane worker (a deliverer, a composer, anything
// below flush) from taking the process down. It is deferred by the worker steps.
func (o *Outbox) recoverLane(where string) {
	if r := recover(); r != nil {
		o.notePanic(where, r)
	}
}

// notePanic counts a recovered panic and logs it at most once per panicWarnEvery.
func (o *Outbox) notePanic(where string, r any) {
	h := o.hand
	h.panics.Add(1)
	now := o.clock()
	h.panicMu.Lock()
	due := h.lastPanic.IsZero() || now.Sub(h.lastPanic) >= panicWarnEvery
	if due {
		h.lastPanic = now
	}
	h.panicMu.Unlock()
	if due {
		logger.ErrorCF("attention", "panic recovered", map[string]any{
			"where": where, "panic": fmt.Sprint(r), "stack": string(debug.Stack()),
		})
	}
}
