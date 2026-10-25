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
}

// Deliverer sends one message to the owner. delivered=false with a nil error
// means the message could not or must not be sent right now; the decisions are
// then released, as for an error.
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

	mu            sync.Mutex
	buf           []buffered
	windowStart   time.Time // when the first buffered decision arrived
	closed        bool
	lastDigestMin int64
	digestBusy    bool
	lastTickWarn  time.Time
}

// NewOutbox builds an outbox over g and d. A groupWindow of zero or less means
// no grouping: buffered decisions go out on the next Tick. It should equal
// AttentionConfig.GroupWindowSeconds, which the governor's budget uses to count
// decisions in flight. A nil clock means time.Now.
func NewOutbox(g Gate, d Deliverer, groupWindow time.Duration, clock func() time.Time) *Outbox {
	if clock == nil {
		clock = time.Now
	}
	return &Outbox{g: g, d: d, window: groupWindow, clock: clock, lastDigestMin: -1}
}

// Submit asks the governor to decide it and, for a notify or escalate decision,
// buffers it for the next message. Other decisions are returned as they are (a
// digest decision is already queued). A protective Critical is flushed before
// Submit returns, together with everything already buffered. A failure to deliver
// is not an error of Submit: it is logged and recorded by releasing the
// decisions; the returned error is the governor's.
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
func (o *Outbox) Tick(ctx context.Context) error {
	now := o.clock()
	var out []buffered
	o.mu.Lock()
	if len(o.buf) > 0 && (o.window <= 0 || !now.Before(o.windowStart.Add(o.window))) {
		out = o.takeLocked()
	}
	o.mu.Unlock()

	var errs []error
	if out != nil {
		errs = append(errs, o.flush(ctx, out))
	}
	errs = append(errs, o.tickDigest(ctx, now))
	return errors.Join(errs...)
}

// Run calls Tick every second until ctx ends, then flushes what is buffered once,
// with a short timeout of its own. It returns after that flush.
func (o *Outbox) Run(ctx context.Context) {
	t := time.NewTicker(tickEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			o.shutdown(ctx)
			return
		case <-t.C:
			if err := o.Tick(ctx); err != nil && ctx.Err() == nil {
				o.warnTick(err)
			}
		}
	}
}

func (o *Outbox) shutdown(ctx context.Context) {
	o.mu.Lock()
	o.closed = true
	out := o.takeLocked()
	o.mu.Unlock()
	if out == nil {
		return
	}
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownFlushTimeout)
	defer cancel()
	if err := o.flush(fctx, out); err != nil {
		logger.WarnCF("attention", "final flush failed", map[string]any{"error": err.Error()})
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
func (o *Outbox) deliver(ctx context.Context, m Message) (bool, error) {
	dctx, cancel := context.WithTimeout(ctx, deliverTimeout)
	defer cancel()
	ok, err := o.d.Deliver(dctx, m)
	if err != nil {
		return false, err
	}
	return ok, nil
}

// flush renders group as one message, delivers it and reports the outcome for
// every member, whatever it was.
func (o *Outbox) flush(ctx context.Context, group []buffered) error {
	sort.SliceStable(group, func(i, j int) bool { return ranksBefore(group[i].dec, group[j].dec) })

	items := make([]Item, len(group))
	decs := make([]Decision, len(group))
	escalate := false
	for i, b := range group {
		items[i], decs[i] = b.item, b.dec
		escalate = escalate || b.dec.Disposition == proactive.DispEscalate
	}
	m := renderGroup(items)
	m.InterventionID = proactive.NewID()
	m.Decisions = decs

	if escalate && o.Composer != nil {
		composed, err := o.Composer.Compose(ctx, m, items)
		if err != nil {
			logger.WarnCF("attention", "composer failed; delivering the template message", map[string]any{"error": err.Error()})
		} else {
			composed.InterventionID, composed.Decisions = m.InterventionID, m.Decisions
			composed.Digest = false
			m = composed
		}
	}

	ok, derr := o.deliver(ctx, m)
	if derr != nil || !ok {
		fields := map[string]any{"decisions": len(decs), "intervention_id": m.InterventionID}
		if derr != nil {
			fields["error"] = derr.Error()
		}
		logger.WarnCF("attention", "message not delivered; decisions released", fields)
	}

	bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), bookkeepTimeout)
	defer cancel()
	var errs []error
	for _, d := range decs {
		if err := o.g.MarkDelivered(bctx, d, Delivery{Delivered: ok, InterventionID: m.InterventionID}); err != nil {
			errs = append(errs, fmt.Errorf("decision %s: %w", d.ID, err))
		}
	}
	return errors.Join(errs...)
}

// tickDigest sends the daily digest when the governor says it is due. It asks at
// most once per clock minute (a digest time has minute granularity) and never
// from two goroutines at once.
func (o *Outbox) tickDigest(ctx context.Context, now time.Time) error {
	minute := now.Unix() / 60
	o.mu.Lock()
	if o.digestBusy || minute == o.lastDigestMin {
		o.mu.Unlock()
		return nil
	}
	o.lastDigestMin, o.digestBusy = minute, true
	o.mu.Unlock()
	defer func() {
		o.mu.Lock()
		o.digestBusy = false
		o.mu.Unlock()
	}()

	dg, err := o.g.PendingDigest(ctx)
	if err != nil {
		o.mu.Lock()
		o.lastDigestMin = -1 // a failed check is retried on the next tick
		o.mu.Unlock()
		return err
	}
	if dg == nil {
		return nil
	}

	title, body := RenderDigest(dg.Items)
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
	return o.g.CompleteDigest(bctx, dg, Delivery{Delivered: ok, InterventionID: m.InterventionID})
}

// renderGroup renders ranked items as one message. One item keeps its own
// title and prose; several get a headline with the count and one line per item
// (a prose, when present, follows its line, indented). Severity is the highest in
// the group. Every title is reduced to a single clean line here, so a newline in
// one cannot pass for another item.
func renderGroup(items []Item) Message {
	m := Message{}
	for _, it := range items {
		m.Severity = max(m.Severity, it.Candidate.Severity)
	}
	if len(items) == 0 {
		return m
	}
	m.DedupKey = items[0].Candidate.DedupKey

	if len(items) == 1 {
		m.Title = itemTitle(items[0].Candidate)
		m.Body = strings.TrimSpace(items[0].Prose)
		return m
	}
	m.Title = strconv.Itoa(len(items)) + " updates"
	var b strings.Builder
	for i, it := range items {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString("• " + itemTitle(it.Candidate))
		if p := oneLine(it.Prose, maxProseRunes); p != "" {
			b.WriteString("\n  " + p)
		}
	}
	m.Body = b.String()
	return m
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
