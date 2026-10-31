package attention

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// The outbox turns decisions into messages: it owns "decision -> message".
//
// A notify or escalate decision is buffered for the group window so that
// several things firing together reach the owner as one message; a protective
// Critical flushes at once with everything already buffered; the daily digest
// is sent from here too, once PendingDigest says it is due. The governor does
// not know how a message is delivered, so delivery goes through the Deliverer
// interface, which the gateway adapts to pkg/notify (attention must not import
// it).
//
// Nothing here holds a lock while it calls the governor or the deliverer: the
// deliverer does network I/O and the governor opens transactions.
//
// Crash behaviour. Buffered decisions live in memory only. If the process dies
// before a flush, their rows are in-flight reservations that expire after
// pendingLease (10 minutes) and then stop counting against the budget and dedup;
// nothing is delivered twice and nothing is stuck. On an orderly stop, Run
// flushes the buffer once.
//
// Failed deliveries. A message the deliverer did not take is not given up on: it
// is tried again after a short wait, a bounded number of times (see retry.go),
// while its decisions stay reserved. When the tries run out, or the outbox stops
// with a retry pending, each decision is moved to the digest (DeferToDigest) so
// the owner hears of it late instead of never. A protective Critical cannot be
// moved there: it is retried for up to an hour, then released. Retries are in
// memory, like the buffer: after a crash the reservation lapses as described
// above.

const (
	// tickEvery is how often Run calls Tick.
	tickEvery = time.Second

	// deliverTimeout bounds one Deliver call, comfortably under pendingLease: a
	// delivery still running when its reservation lapses could be double-counted.
	deliverTimeout = 90 * time.Second
	// bookkeepTimeout bounds the MarkDelivered calls that follow a Deliver. They
	// run on a context that outlives cancellation: the owner may already have the
	// message.
	bookkeepTimeout = 10 * time.Second
	// shutdownFlushTimeout bounds the final flush when Run's context ends.
	shutdownFlushTimeout = 5 * time.Second
	// tickWarnEvery limits how often Run logs a failing Tick.
	tickWarnEvery = time.Minute

	// maxBuffered caps the decisions held for one message. A Submit that would
	// exceed it flushes the buffered group first.
	maxBuffered = 64

	maxLineRunes  = 200
	maxProseRunes = 300
	// maxDetailLineRunes is longer than a title line: a detail line may quote an
	// alert's own message, which a trigger caps at 300 characters.
	maxDetailLineRunes = 320
)

// Item is a candidate to put in front of the owner, with optional model text.
type Item struct {
	Candidate proactive.Candidate
	// Prose is optional model text shown under the title. It is not trusted:
	// the deliverer cleans it.
	Prose string
}

// Message is what the deliverer sends. It carries no actions and no footer: the
// control path that would answer a "/snooze" does not exist yet, and a footer
// would reach the model as ordinary chat.
type Message struct {
	// InterventionID is shared by every decision in the message.
	InterventionID string
	// Severity is the highest in the group; Info for a digest.
	Severity proactive.Severity
	Title    string
	Body     string
	// DedupKey is the audit identity of the message.
	DedupKey string
	// Silent asks the channel not to buzz the owner (the digest).
	Silent bool
	// Digest marks the daily digest.
	Digest bool
	// Decisions are the decisions that make up the message, ranked as rendered.
	Decisions []Decision
	// SessionRestricted says that some member's Quote is not to be kept in the owner's
	// session (see SessionWithheld): the session copy then carries SessionBody in
	// place of Body. When it is false the session copy carries Body, as it always did.
	SessionRestricted bool
	SessionBody       string
}

// SessionWithheld stands in the owner's session for the quoted text of a message.
// The session is what the model reads on the owner's next turn, a turn with the
// owner's own tools, so text that did not originate in this process (see
// proactive.Candidate.Quote) is shown to the owner in the chat and not kept there.
const SessionWithheld = "(quoted text was shown to you in the message and is not kept in this conversation)"

// hasQuote reports whether a member carries quoted text.
func hasQuote(c proactive.Candidate) bool {
	return strings.TrimSpace(c.Quote) != ""
}

// Deliverer sends one message to the owner. delivered=false with a nil error
// means the message could not or must not be sent right now; it is then treated
// as for an error: tried again later, and in the end moved to the digest.
//
// A message is sent again only after a false or an error, so Deliver must not
// report false or an error for a message it did send. The same Message, with the
// same InterventionID, is passed on every try.
type Deliverer interface {
	Deliver(ctx context.Context, m Message) (delivered bool, err error)
}

// Gate is what the outbox needs from the governor. *Governor satisfies it, and
// so does attentiontest.Fake, which lets other packages test against an outbox
// without a database.
type Gate interface {
	Decider
	PendingDigest(ctx context.Context) (*Digest, error)
	CompleteDigest(ctx context.Context, dg *Digest, r Delivery) error
}

var _ Gate = (*Governor)(nil)

// Composer is the extension point where Phase 2 plugs in the LLM composer for
// groups that contain an escalate decision. Phase 1 has none: the field is nil
// and escalate decisions are delivered with the template rendering, which is
// also what happens when a Composer returns an error.
//
// Compose receives the template message m and the items in the order of
// m.Decisions, and returns the message to send in its place. The outbox keeps
// the intervention ID and the decisions of m whatever the result says.
//
// The Composer is never consulted for a message that has a protective Critical
// among its decisions, even though attention decides such a candidate "escalate"
// when it asks for composition: that message is delivered with the template
// rendering at once, and no explanation follows (one breach, one message). It also
// runs under composeTimeout, and a panic in it is an error.
type Composer interface {
	Compose(ctx context.Context, m Message, items []Item) (Message, error)
}

type buffered struct {
	dec  Decision
	item Item
}

// Outbox buffers, groups and delivers. It is safe for concurrent use.
type Outbox struct {
	// Composer is the optional LLM composer for escalate groups; see Composer.
	// Set it before Run and Submit are first used.
	Composer Composer

	g      Gate
	d      Deliverer
	window time.Duration
	clock  func() time.Time
	// timers, when set (SetTimerClock), paces Run instead of a wall ticker.
	timers interface {
		After(d time.Duration) <-chan time.Time
	}

	mu            sync.Mutex
	buf           []buffered
	windowStart   time.Time // when the first buffered decision arrived
	closed        bool
	lastDigestMin int64
	digestBusy    bool
	lastTickWarn  time.Time

	// The hand-over side (Accept and the delivery lanes; see handoff.go). All of
	// it is guarded by mu.
	tickEvery time.Duration
	ordSlots  int
	hand      *handoffState

	// obs reports each finished message to the Observer (see observer.go).
	obs *observerState
}

// NewOutbox builds an outbox over g and d. A groupWindow of zero or less means
// no grouping: buffered decisions go out on the next Tick. It should equal
// AttentionConfig.GroupWindowSeconds, which the governor's budget uses to count
// decisions in flight. A nil clock means time.Now.
func NewOutbox(g Gate, d Deliverer, groupWindow time.Duration, clock func() time.Time) *Outbox {
	if clock == nil {
		clock = time.Now
	}
	return &Outbox{
		g: g, d: d, window: groupWindow, clock: clock, lastDigestMin: -1,
		tickEvery: tickEvery, ordSlots: ordinaryLaneSlots, hand: newHandoffState(), obs: newObserverState(),
	}
}

// Submit asks the governor to decide it and, for a notify or escalate decision,
// buffers it for the next message. Other decisions are returned as they are (a
// digest decision is already queued). A protective Critical is flushed before
// Submit returns, together with everything already buffered. A failure to deliver
// is not an error of Submit: it is logged and the message is retried or its
// decisions moved to the digest (see the file comment); the returned error is the
// governor's.
func (o *Outbox) Submit(ctx context.Context, it Item) (Decision, error) {
	d, err := o.g.Decide(ctx, it.Candidate)
	if err != nil {
		return d, err
	}
	if d.Disposition != proactive.DispNotify && d.Disposition != proactive.DispEscalate {
		return d, nil
	}
	it.Candidate = d.Candidate // normalised: ID and time filled in

	now := o.clock()
	var expired, out []buffered
	o.mu.Lock()
	if len(o.buf) > 0 && o.window > 0 && !now.Before(o.windowStart.Add(o.window)) {
		// The window closed before a Tick got to it. The new decision opens the
		// next one rather than joining a group that is already due.
		expired = o.takeLocked()
	}
	if len(o.buf) >= maxBuffered && expired == nil {
		// Belt and braces: nothing may grow without bound if ticks stop. The
		// full group goes out now and the new decision opens the next one.
		expired = o.takeLocked()
	}
	if len(o.buf) == 0 {
		o.windowStart = now
	}
	o.buf = append(o.buf, buffered{dec: d, item: it})
	if isProtectiveCritical(d.Candidate) || o.closed {
		out = o.takeLocked()
	}
	o.mu.Unlock()

	for _, group := range [][]buffered{expired, out} {
		if group == nil {
			continue
		}
		if err := o.flush(ctx, group); err != nil {
			logger.WarnCF("attention", "immediate flush failed", map[string]any{"error": err.Error()})
		}
	}
	return d, nil
}

// takeLocked empties the buffer. The caller holds o.mu.
func (o *Outbox) takeLocked() []buffered {
	out := o.buf
	o.buf = nil
	return out
}

// Tick flushes the buffer when its window has elapsed and sends the daily digest
// when one is due. The window opens at the first buffered decision and later
// ones do not extend it. It is safe to call concurrently with Submit and itself.
//
// Tick delivers on the caller's goroutine. Run does not use it: it hands the
// same work to the delivery lanes (see handoff.go), so that its own goroutine
// never waits on a channel.
func (o *Outbox) Tick(ctx context.Context) error { return o.tick(ctx, false) }

// tick is Tick, or with handOver its non-blocking form: a due group and the
// digest are queued for the delivery lanes instead of being delivered here.
func (o *Outbox) tick(ctx context.Context, handOver bool) error {
	now := o.clock()
	var out []buffered
	o.mu.Lock()
	if len(o.buf) > 0 && (o.window <= 0 || !now.Before(o.windowStart.Add(o.window))) {
		out = o.takeLocked()
	}
	o.mu.Unlock()

	var errs []error
	if out != nil {
		if handOver {
			o.handOverOrdinary(out)
		} else {
			errs = append(errs, o.flush(ctx, out))
		}
	}
	if handOver {
		o.handOverDueRetries(now)
	} else {
		for _, r := range o.takeDueRetries(now) {
			errs = append(errs, o.attemptRetry(ctx, r))
		}
	}
	errs = append(errs, o.processDeferrals(ctx))
	if handOver {
		errs = append(errs, o.handOverDigest(ctx, now))
	} else {
		errs = append(errs, o.tickDigest(ctx, now))
	}
	return errors.Join(errs...)
}

// SetTimerClock makes Run wait on c.After(tick interval) instead of a wall-clock
// ticker, so a simulated clock paces the outbox. Call it before Run; never calling
// it keeps the ticker.
func (o *Outbox) SetTimerClock(c interface {
	After(d time.Duration) <-chan time.Time
}) {
	o.mu.Lock()
	o.timers = c
	o.mu.Unlock()
}

// Run drives the outbox until ctx ends: it starts the delivery lanes (one
// ordinary worker, one protective worker), calls tick every tick interval (one
// second unless SetTickInterval changed it) and, when ctx ends, delivers what is
// buffered and queued once, with a short timeout of its own. It returns after
// that, and only once both workers have exited (or, for a deliverer that ignores
// its context, after a bounded wait).
func (o *Outbox) Run(ctx context.Context) {
	o.mu.Lock()
	every, timers := o.tickEvery, o.timers
	o.mu.Unlock()
	lanes := o.startLanes()
	var next func() <-chan time.Time
	if timers != nil {
		next = func() <-chan time.Time { return timers.After(every) }
	} else {
		t := time.NewTicker(every)
		defer t.Stop()
		next = func() <-chan time.Time { return t.C }
	}
	for {
		select {
		case <-ctx.Done():
			o.shutdown(ctx, lanes)
			return
		case <-next():
			if err := o.tick(ctx, true); err != nil && ctx.Err() == nil {
				o.warnTick(err)
			}
		}
	}
}

// warnTick logs a failing Tick at most once per tickWarnEvery: a database that
// is down would otherwise log every second.
func (o *Outbox) warnTick(err error) {
	now := o.clock()
	o.mu.Lock()
	due := o.lastTickWarn.IsZero() || now.Sub(o.lastTickWarn) >= tickWarnEvery
	if due {
		o.lastTickWarn = now
	}
	o.mu.Unlock()
	if due {
		logger.WarnCF("attention", "outbox tick failed", map[string]any{"error": err.Error()})
	}
}

// deliver calls the deliverer under deliverTimeout and folds its two failure
// shapes into one.
func (o *Outbox) deliver(ctx context.Context, m Message) (ok bool, err error) {
	// A deliverer that panics has not delivered: the members are released like
	// for any other failure, and the process survives.
	defer func() {
		if r := recover(); r != nil {
			o.notePanic("deliverer", r)
			ok, err = false, fmt.Errorf("deliverer panicked: %v", r)
		}
	}()
	dctx, cancel := context.WithTimeout(ctx, deliverTimeout)
	defer cancel()
	ok, err = o.d.Deliver(dctx, m)
	if err != nil {
		return false, err
	}
	return ok, nil
}

// compose calls the Composer under composeTimeout. A composer that panics is an
// error like any other, so the template message goes out.
func (o *Outbox) compose(ctx context.Context, m Message, items []Item) (out Message, err error) {
	defer func() {
		if r := recover(); r != nil {
			o.notePanic("composer", r)
			err = fmt.Errorf("composer panicked: %v", r)
		}
	}()
	// Bounded here, whatever the Composer does: a queued message must still be
	// inside its lease when it is delivered (see maxQueueAge).
	cctx, cancel := context.WithTimeout(ctx, composeTimeout)
	defer cancel()
	return o.Composer.Compose(cctx, m, items)
}

// flush renders group as one message, delivers it and reports the outcome for
// every member, whatever it was.
func (o *Outbox) flush(ctx context.Context, group []buffered) error {
	return o.flushWith(ctx, group, true)
}

// flushWith is flush; compose says whether the Composer may be consulted. The
// protective lane passes false: a model turn must never stand between a
// protective Critical and the owner.
func (o *Outbox) flushWith(ctx context.Context, group []buffered, compose bool) error {
	sort.SliceStable(group, func(i, j int) bool { return ranksBefore(group[i].dec, group[j].dec) })

	items := make([]Item, len(group))
	decs := make([]Decision, len(group))
	escalate, composedOK := false, false
	for i, b := range group {
		items[i], decs[i] = b.item, b.dec
		escalate = escalate || b.dec.Disposition == proactive.DispEscalate
		// A message with a protective Critical in it is never held for the
		// composer, whichever path flushes it: attention's rule 4 turns a protective
		// Critical that asks for composition into an escalate, and a liquidation-class
		// alert must not wait on a model call. It goes out as the template, once.
		compose = compose && !isProtectiveCritical(b.dec.Candidate)
	}
	m := renderGroup(items)
	m.InterventionID = proactive.NewID()
	m.Decisions = decs

	if escalate && compose && o.Composer != nil {
		composed, err := o.compose(ctx, m, items)
		if err != nil {
			logger.WarnCF("attention", "composer failed; delivering the template message", map[string]any{"error": err.Error()})
		} else {
			composed.InterventionID, composed.Decisions = m.InterventionID, m.Decisions
			composed.Digest = false
			// The session copy of a restricted message keeps its withheld form; the
			// composer's prose is added to it only when it is, as it is meant to be,
			// text appended to the template body (a composer that rewrote anything
			// else leaves the session with the template alone).
			composed.SessionRestricted, composed.SessionBody = m.SessionRestricted, m.SessionBody
			if m.SessionRestricted && strings.HasPrefix(composed.Body, m.Body) {
				composed.SessionBody = m.SessionBody + composed.Body[len(m.Body):]
			}
			m = composed
			composedOK = true
		}
	}

	// Only a message the deliverer did not take is ever tried again: once it has
	// taken it, nothing below can send it a second time.
	if ok, derr := o.deliver(ctx, m); !ok {
		return o.deliveryFailed(ctx, m, composedOK, 1, time.Time{}, derr)
	}
	return o.delivered(ctx, m, composedOK)
}

// delivered records that the deliverer took m: every member is marked delivered
// in m's intervention, and the Observer is told.
func (o *Outbox) delivered(ctx context.Context, m Message, composed bool) error {
	bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), bookkeepTimeout)
	defer cancel()
	var errs []error
	for _, d := range m.Decisions {
		if err := o.g.MarkDelivered(bctx, d, Delivery{Delivered: true, InterventionID: m.InterventionID}); err != nil {
			errs = append(errs, fmt.Errorf("decision %s: %w", d.ID, err))
		}
	}
	o.observe(Outcome{Message: m, Delivered: true, Composed: composed, At: o.clock()})
	return errors.Join(errs...)
}

// tickDigest sends the daily digest when the governor says it is due. It asks at
// most once per clock minute (a digest time has minute granularity) and never
// from two goroutines at once.
func (o *Outbox) tickDigest(ctx context.Context, now time.Time) error {
	dg, err := o.pendingDigest(ctx, now)
	if err != nil || dg == nil {
		return err
	}
	defer o.digestDone()
	return o.sendDigest(ctx, dg)
}

// pendingDigest asks the governor whether a digest is due. A nil digest with a
// nil error means none is due (or one is already being sent). A non-nil digest
// leaves the outbox busy: the caller must call digestDone once it has sent it.
func (o *Outbox) pendingDigest(ctx context.Context, now time.Time) (*Digest, error) {
	minute := now.Unix() / 60
	o.mu.Lock()
	if o.digestBusy || minute == o.lastDigestMin {
		o.mu.Unlock()
		return nil, nil
	}
	o.lastDigestMin, o.digestBusy = minute, true
	o.mu.Unlock()

	dg, err := o.g.PendingDigest(ctx)
	if err != nil {
		o.mu.Lock()
		o.lastDigestMin = -1 // a failed check is retried on the next tick
		o.digestBusy = false
		o.mu.Unlock()
		return nil, err
	}
	if dg == nil {
		o.digestDone()
		return nil, nil
	}
	return dg, nil
}

func (o *Outbox) digestDone() {
	o.mu.Lock()
	o.digestBusy = false
	o.mu.Unlock()
}

// sendDigest delivers dg and reports the outcome to the governor.
func (o *Outbox) sendDigest(ctx context.Context, dg *Digest) error {
	title, body, sessionBody := renderDigest(dg.Items)
	m := Message{
		InterventionID: proactive.NewID(),
		Severity:       proactive.SeverityInfo,
		Title:          title,
		Body:           body,
		DedupKey:       "digest:" + dg.Day,
		Silent:         true,
		Digest:         true,
		Decisions:      dg.Items,
	}
	if sessionBody != "" {
		m.SessionRestricted, m.SessionBody = true, sessionBody
	}
	ok, derr := o.deliver(ctx, m)
	if derr != nil || !ok {
		fields := map[string]any{"items": len(dg.Items), "retry_after": digestRetry.String()}
		if derr != nil {
			fields["error"] = derr.Error()
		}
		logger.WarnCF("attention", "digest not delivered; it stays queued", fields)
	}
	bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), bookkeepTimeout)
	defer cancel()
	err := o.g.CompleteDigest(bctx, dg, Delivery{Delivered: ok, InterventionID: m.InterventionID})
	o.observe(Outcome{Message: m, Delivered: ok, At: o.clock()})
	return err
}

// renderGroup renders ranked items as one message. One item keeps its own
// title, Detail, Quote (as "> " lines) and prose; several get a headline with the
// count and one line per item (its first Detail line, or when it has none the first
// line of its Quote, and a prose, when present, follow its line, indented). Severity
// is the highest in the group. Every title is reduced to a single clean line here, so
// a newline in one cannot pass for another item.
//
// The Quote is text from outside, shown to the owner here and not kept in the
// session the model reads: when any member has one, the message also carries the
// session form of its body, in which the quoted lines are replaced by one
// SessionWithheld sentence. A message with no Quote is not restricted at all.
func renderGroup(items []Item) Message {
	m := Message{}
	for _, it := range items {
		m.Severity = max(m.Severity, it.Candidate.Severity)
		m.SessionRestricted = m.SessionRestricted || hasQuote(it.Candidate)
	}
	if len(items) == 0 {
		return m
	}
	m.DedupKey = items[0].Candidate.DedupKey

	if len(items) == 1 {
		c := items[0].Candidate
		prose := strings.TrimSpace(items[0].Prose)
		m.Title = itemTitle(c)
		m.Body = joinBlocks(detailBlock(c), quoteBlock(c), prose)
		if m.SessionRestricted {
			m.SessionBody = joinBlocks(detailBlock(c), SessionWithheld, prose)
		}
		return m
	}
	m.Title = strconv.Itoa(len(items)) + " updates"
	var b, sb strings.Builder
	for i, it := range items {
		if i > 0 {
			b.WriteString("\n")
			sb.WriteString("\n")
		}
		head := "• " + itemTitle(it.Candidate)
		b.WriteString(head)
		sb.WriteString(head)
		if l := detailLines(it.Candidate.Detail, 1); len(l) > 0 {
			b.WriteString("\n  " + l[0])
			sb.WriteString("\n  " + l[0])
		} else if l := quoteLines(it.Candidate, 1); len(l) > 0 {
			b.WriteString("\n  " + l[0])
		}
		if p := oneLine(it.Prose, maxProseRunes); p != "" {
			b.WriteString("\n  " + p)
			sb.WriteString("\n  " + p)
		}
	}
	m.Body = b.String()
	if m.SessionRestricted {
		m.SessionBody = sb.String() + "\n" + SessionWithheld
	}
	return m
}

// quoteLines cleans a candidate's Quote into at most max "> " lines, each cut at
// maxDetailLineRunes. The trigger has cleaned it already; it is text from outside, so
// each line goes through the same cleaning as a title all the same.
func quoteLines(c proactive.Candidate, max int) []string {
	var out []string
	for _, raw := range strings.Split(c.Quote, "\n") {
		if len(out) >= max {
			break
		}
		if l := oneLine(raw, maxDetailLineRunes); l != "" {
			out = append(out, "> "+l)
		}
	}
	return out
}

// quoteBlock is the lines of a single message's Quote, one per line.
func quoteBlock(c proactive.Candidate) string {
	return strings.Join(quoteLines(c, proactive.MaxQuoteLines), "\n")
}

// detailLines cleans a candidate's Detail into at most max single lines, each cut
// at maxDetailLineRunes. The Detail is built by code, but it may quote text from outside
// (an alert's own message), so each line goes through the same cleaning as a
// title: no control character survives and a newline cannot pass for another line.
func detailLines(detail string, max int) []string {
	var out []string
	for _, raw := range strings.Split(detail, "\n") {
		if len(out) >= max {
			break
		}
		if l := oneLine(raw, maxDetailLineRunes); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// detailBlock is the lines of a single message's Detail, one per line.
func detailBlock(c proactive.Candidate) string {
	return strings.Join(detailLines(c.Detail, proactive.MaxDetailLines), "\n")
}

// joinBlocks puts the non-empty blocks of a message body one blank line apart.
func joinBlocks(blocks ...string) string {
	var out []string
	for _, b := range blocks {
		if b != "" {
			out = append(out, b)
		}
	}
	return strings.Join(out, "\n\n")
}

func itemTitle(c proactive.Candidate) string {
	if t := oneLine(c.Title, maxLineRunes); t != "" {
		return t
	}
	if t := oneLine(c.DedupKey, maxLineRunes); t != "" {
		return t
	}
	return "(no title)"
}
