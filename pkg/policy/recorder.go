package policy

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
)

// Authorizer is the engine as the Recorder uses it (a seam for tests).
type Authorizer interface {
	Authorize(ctx context.Context, r ActionRequest) Verdict
	Note(r ActionRequest)
}

// DefaultBuffer is how many calls the recorder queues before it starts dropping.
const DefaultBuffer = 256

type job struct {
	ctx     context.Context
	req     ActionRequest
	allowed bool
	// decided, when set, is a verdict the live hook already made and applied: the
	// job records it and neither asks the engine again nor counts the order again.
	decided *Verdict
}

// Recorder is the shadow wiring: it asks the engine about a call that the
// existing checks have already decided, records the verdict beside what they did,
// and publishes policy.decision. It changes no outcome and takes nothing from the
// call it observes.
//
// Observe only queues a record (the request, already built from what the call
// had, and a detached copy of its context). One goroutine does everything else:
// the engine's gates, including the mandate lookup and the USD valuation (each
// bounded by its own timeout), the database insert and the event. When the queue is
// full a record is dropped and counted; Observe never blocks.
type Recorder struct {
	engine Authorizer
	store  *Store
	events events.Bus
	now    func() time.Time

	q       chan job
	stop    chan struct{}
	done    chan struct{}
	closed  atomic.Bool
	deadln  atomic.Int64 // unix nanos: when draining after Close must stop
	pending atomic.Int64
	dropped atomic.Int64
}

// NewRecorder starts a recorder with a queue of buffer records (DefaultBuffer when
// not positive). store and bus may be nil. Call Close to stop it.
func NewRecorder(engine Authorizer, store *Store, bus events.Bus, buffer int) *Recorder {
	if buffer <= 0 {
		buffer = DefaultBuffer
	}
	rc := &Recorder{
		//simclock:wall the default when no clock is injected
		engine: engine, store: store, events: bus, now: time.Now,
		q: make(chan job, buffer), stop: make(chan struct{}), done: make(chan struct{}),
	}
	go rc.run()
	return rc
}

// SetClock replaces the clock that stamps each stored record. Call it before the
// first Observe; a nil now keeps the current one.
func (rc *Recorder) SetClock(now func() time.Time) {
	if now != nil {
		rc.now = now
	}
}

// Observe queues the record of a call whose outcome the existing checks have
// already decided. legacyAllowed is what they did; the engine never sees it. It
// never blocks: a full queue, or a closed recorder, drops the record and counts it.
func (rc *Recorder) Observe(ctx context.Context, r ActionRequest, legacyAllowed bool) {
	rc.enqueue(job{ctx: context.WithoutCancel(ctx), req: r, allowed: legacyAllowed})
}

func (rc *Recorder) enqueue(j job) {
	if rc == nil || rc.closed.Load() {
		return
	}
	rc.pending.Add(1)
	select {
	case rc.q <- j:
	default:
		rc.pending.Add(-1)
		if n := rc.dropped.Add(1); n == 1 || n%100 == 0 {
			logger.WarnCF("policy", "The policy shadow queue is full; dropping records", map[string]any{"dropped": n})
		}
	}
}

// Record queues the record of a verdict the live tool hook has already decided and
// applied to a call that the existing checks allowed. It asks the engine nothing and
// counts nothing (the hook's own decision did); like Observe it never blocks.
func (rc *Recorder) Record(ctx context.Context, r ActionRequest, v Verdict) {
	rc.enqueue(job{ctx: context.WithoutCancel(ctx), req: r, allowed: true, decided: &v})
}

// Idle is true when no record is queued, being evaluated or being stored. It changes
// nothing; only the sandbox lab calls it.
func (rc *Recorder) Idle() bool { return rc == nil || rc.pending.Load() == 0 }

// Dropped is how many records were dropped because the queue was full.
func (rc *Recorder) Dropped() int64 { return rc.dropped.Load() }

// Flush waits until every queued record has been processed, or the timeout
// passes; it reports whether the queue emptied. For tests and shutdown.
func (rc *Recorder) Flush(timeout time.Duration) bool {
	//simclock:wall the queue drain on shutdown: bounded in wall time
	end := time.Now().Add(timeout)
	for rc.pending.Load() > 0 {
		//simclock:wall the queue drain on shutdown: bounded in wall time
		if time.Now().After(end) {
			return false
		}
		//simclock:wall the queue drain on shutdown: bounded in wall time
		time.Sleep(time.Millisecond)
	}
	return true
}

// Close stops accepting records, processes what is queued for at most timeout, and
// stops the goroutine; it returns once that has happened or the timeout passed (a
// record still in flight then finishes on its own and is lost if the database has
// closed). Safe to call more than once.
func (rc *Recorder) Close(timeout time.Duration) {
	if rc == nil {
		return
	}
	if rc.closed.CompareAndSwap(false, true) {
		//simclock:wall the queue drain on shutdown: bounded in wall time
		rc.deadln.Store(time.Now().Add(timeout).UnixNano())
		close(rc.stop)
	}
	select {
	case <-rc.done:
	//simclock:wall a shutdown wait, bounded in wall time
	case <-time.After(timeout):
	}
}

func (rc *Recorder) run() {
	defer close(rc.done)
	for {
		select {
		case j := <-rc.q:
			rc.process(j)
		case <-rc.stop:
			//simclock:wall the queue drain on shutdown: bounded in wall time
			for time.Now().UnixNano() < rc.deadln.Load() {
				select {
				case j := <-rc.q:
					rc.process(j)
				default:
					return
				}
			}
			return
		}
	}
}

// process records one call. A panic anywhere in it is recovered: the shadow path
// must never take the process down.
func (rc *Recorder) process(j job) {
	defer rc.pending.Add(-1)
	defer func() {
		if p := recover(); p != nil {
			logger.ErrorCF("policy", "The policy shadow path panicked; the call was unaffected",
				map[string]any{"tool": j.req.Tool, "panic": p})
		}
	}()
	r := j.req
	var v Verdict
	if j.decided != nil {
		v = *j.decided
	} else {
		v = rc.engine.Authorize(j.ctx, r)
	}
	if j.decided == nil && !j.allowed && v.Mode == ModeAct {
		logger.ErrorCF("policy", "The policy engine is looser than the existing checks: it would let through a call they refused",
			map[string]any{"tool": r.Tool, "turn_kind": string(r.TurnKind), "tier": v.Tier.String(), "reasons": v.Reasons})
	}
	if j.allowed && j.decided == nil {
		rc.engine.Note(r)
	}
	d := Decision{
		At: rc.now(), Tool: r.Tool, TurnKind: string(r.TurnKind), Trust: r.Trust,
		Tier: v.Tier, Mode: v.Mode, LegacyAllowed: j.allowed, Enforced: j.decided != nil, Reasons: v.Reasons,
	}
	if rc.store != nil {
		if err := rc.store.Add(j.ctx, d); err != nil {
			logger.WarnCF("policy", "Could not record a policy decision", map[string]any{"error": err.Error()})
		}
	}
	if rc.events != nil {
		// Ids, names, mode and tier only: never the call's arguments or amounts.
		rc.events.PublishNonBlocking(events.Event{
			Kind:   events.KindPolicyDecision,
			Source: events.Source{Component: "policy", Name: r.Tool},
			Trust:  events.TrustPtr(r.Trust),
			Attrs: map[string]any{
				"tool": r.Tool, "class": string(r.Class), "turn_kind": string(r.TurnKind),
				"mode": string(v.Mode), "tier": v.Tier.String(), "legacy_allowed": j.allowed, "enforced": j.decided != nil,
			},
		})
	}
}
