package attention

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// nextRetryDue is when the earliest pending retry is due (zero when none is).
func (e *obEnv) nextRetryDue() time.Time {
	e.ob.mu.Lock()
	defer e.ob.mu.Unlock()
	var due time.Time
	for _, r := range e.ob.hand.retries {
		if due.IsZero() || r.due.Before(due) {
			due = r.due
		}
	}
	return due
}

// tickNextRetry moves the clock to the earliest pending retry and ticks.
func (e *obEnv) tickNextRetry() {
	e.t.Helper()
	due := e.nextRetryDue()
	if due.IsZero() {
		e.t.Fatal("no retry is pending")
	}
	e.tickAt(due)
}

// exhaustRetries ticks from retry to retry until none is pending.
func (e *obEnv) exhaustRetries() {
	e.t.Helper()
	for i := 0; i < 40 && e.ob.Stats().RetryPending > 0; i++ {
		e.tickNextRetry()
	}
	if n := e.ob.Stats().RetryPending; n != 0 {
		e.t.Fatalf("%d retries still pending after 40 ticks", n)
	}
}

// sent is the messages the deliverer was handed, the digest excluded.
func (e *obEnv) sent() []Message {
	var out []Message
	for _, m := range e.del.messages() {
		if !m.Digest {
			out = append(out, m)
		}
	}
	return out
}

// failTimes fails the first n deliveries and takes the rest.
func failTimes(n int32) func(context.Context, Message) (bool, error) {
	var calls atomic.Int32
	return func(ctx context.Context, m Message) (bool, error) {
		if calls.Add(1) <= n {
			return failing(ctx, m)
		}
		return true, nil
	}
}

func reserved(r row) bool { return r.delivered == 0 && !r.deliveredAtSet }

// A message the channel refuses once is sent again, once, and the decision is
// marked delivered in the same intervention as the first try.
func TestRetry_SecondTrySucceeds(t *testing.T) {
	e := newObEnv(t)
	e.del.setFn(failTimes(1))
	d := e.submit(cand("a", notice))
	e.tickAt(e.clock.Now().Add(time.Minute))

	if n := len(e.sent()); n != 1 {
		t.Fatalf("%d messages after the first try", n)
	}
	if r := e.rowOf(d.ID); !reserved(r) {
		t.Fatalf("row after a failed first try = %+v, want it still reserved", r)
	}
	if n := e.ob.Stats().RetryPending; n != 1 {
		t.Fatalf("RetryPending = %d", n)
	}

	e.tickAt(e.clock.Now().Add(retryDelays[0] - time.Second)) // not yet
	if n := len(e.sent()); n != 1 {
		t.Fatalf("retried %d messages early", n)
	}
	e.tickNextRetry()
	msgs := e.sent()
	if len(msgs) != 2 {
		t.Fatalf("%d messages after the retry, want 2", len(msgs))
	}
	if msgs[1].InterventionID != msgs[0].InterventionID || msgs[1].Decisions[0].ID != d.ID {
		t.Fatalf("the retry is not the same message: %+v then %+v", msgs[0], msgs[1])
	}
	if r := e.rowOf(d.ID); r.delivered != 1 || r.intervention != msgs[0].InterventionID {
		t.Fatalf("row after the retry = %+v", r)
	}
	if _, ok := e.state("a"); !ok {
		t.Fatal("a delivered retry wrote no dedup state")
	}
	if n := e.count(`SELECT COUNT(*) FROM attention_digest_queue`); n != 0 {
		t.Fatalf("%d digest items for a message that was delivered", n)
	}
	if st := e.ob.Stats(); st.Retried != 1 || st.RetriesExhausted != 0 || st.RetryPending != 0 {
		t.Fatalf("stats = %+v", st)
	}

	// and it stays sent.
	for i := 0; i < 3; i++ {
		e.tickAt(e.clock.Now().Add(5 * time.Minute))
	}
	if n := len(e.sent()); n != 2 {
		t.Fatalf("%d messages after more ticks, want 2", n)
	}
}

// A message that the deliverer took is never sent again, and a failure to record
// it afterwards does not make it a candidate for a retry.
func TestRetry_DeliveredMessageIsNotResent(t *testing.T) {
	t.Run("delivered the first time", func(t *testing.T) {
		e := newObEnv(t)
		e.submit(cand("a", notice))
		e.tickAt(e.clock.Now().Add(time.Minute))
		for i := 0; i < 4; i++ {
			e.tickAt(e.clock.Now().Add(5 * time.Minute))
		}
		if n := len(e.sent()); n != 1 {
			t.Fatalf("%d messages, want 1", n)
		}
		if st := e.ob.Stats(); st.Retried != 0 || st.RetryPending != 0 {
			t.Fatalf("stats = %+v", st)
		}
	})
	t.Run("bookkeeping failed after delivery", func(t *testing.T) {
		e := newObEnv(t)
		e.ob.g = markFailGate{Gate: e.g}
		e.submit(cand("a", notice))
		e.clock.Add(time.Minute)
		if err := e.ob.Tick(context.Background()); err == nil {
			t.Fatal("a bookkeeping failure was swallowed")
		}
		for i := 0; i < 4; i++ {
			e.clock.Add(5 * time.Minute)
			_ = e.ob.Tick(context.Background())
		}
		if n := len(e.sent()); n != 1 {
			t.Fatalf("%d messages, want 1: a delivered message was sent again", n)
		}
		if st := e.ob.Stats(); st.Retried != 0 || st.RetryPending != 0 {
			t.Fatalf("stats = %+v", st)
		}
	})
}

// markFailGate fails the report of a successful delivery.
type markFailGate struct{ Gate }

func (g markFailGate) MarkDelivered(ctx context.Context, d Decision, r Delivery) error {
	if r.Delivered {
		return errors.New("database is down")
	}
	return g.Gate.MarkDelivered(ctx, d, r)
}

// When every try fails the decisions go to the digest, marked as late, and are
// delivered there once; nothing else sends them.
func TestRetry_ExhaustedFallsBackToTheDigest(t *testing.T) {
	e := newObEnv(t)
	e.del.setFn(failing)
	ds := []Decision{e.submit(cand("a", notice)), e.submit(cand("b", warning))}
	e.tickAt(e.clock.Now().Add(time.Minute))
	e.exhaustRetries()

	if n := len(e.sent()); n != 1+len(retryDelays) {
		t.Fatalf("%d tries, want %d", n, 1+len(retryDelays))
	}
	msgs := e.sent()
	for _, m := range msgs {
		if m.InterventionID != msgs[0].InterventionID || len(m.Decisions) != 2 {
			t.Fatalf("a try differs from the first: %+v", m)
		}
	}
	for _, d := range ds {
		r := e.rowOf(d.ID)
		if r.delivered != 0 {
			t.Fatalf("row %s = %+v", d.ID, r)
		}
		if n := e.count(`SELECT COUNT(*) FROM attention_decisions WHERE id = ? AND disposition = 'digest' AND reason = ?`, d.ID, ReasonDeferred); n != 1 {
			t.Fatalf("decision %s was not moved to the digest", d.ID)
		}
		if _, ok := e.state(d.Candidate.DedupKey); ok {
			t.Fatalf("a failed send wrote dedup state for %s", d.Candidate.DedupKey)
		}
	}
	if n := e.count(`SELECT COUNT(*) FROM attention_digest_queue WHERE flushed_ms IS NULL`); n != 2 {
		t.Fatalf("digest queue = %d, want 2", n)
	}
	if st := e.ob.Stats(); st.Retried != uint64(len(retryDelays)) || st.RetriesExhausted != 1 || st.Deferred != 2 {
		t.Fatalf("stats = %+v", st)
	}

	// the channel is back by the next digest.
	e.del.setFn(nil)
	e.tickAt(bkkAt(1, 8, 0))
	dgs := e.digests()
	if len(dgs) != 1 || len(dgs[0].Decisions) != 2 {
		t.Fatalf("digests = %+v", dgs)
	}
	for _, line := range []string{"title a (delayed: raised at 12:00,", "title b (delayed: raised at 12:00,"} {
		if !strings.Contains(dgs[0].Body, line) {
			t.Errorf("digest lacks %q: %q", line, dgs[0].Body)
		}
	}
	for _, d := range ds {
		if r := e.rowOf(d.ID); r.delivered != 1 || r.intervention != dgs[0].InterventionID {
			t.Fatalf("row %s after the digest = %+v", d.ID, r)
		}
	}
	if n := e.count(`SELECT COUNT(*) FROM attention_digest_queue WHERE flushed_ms IS NULL`); n != 0 {
		t.Fatalf("%d items left in the digest queue", n)
	}
	e.tickAt(bkkAt(1, 9, 0))
	e.tickAt(bkkAt(2, 8, 0))
	if n := len(e.digests()); n != 1 {
		t.Fatalf("%d digests, want 1", n)
	}
	if n := len(e.sent()); n != 1+len(retryDelays) {
		t.Fatalf("%d non-digest messages, want %d: a decision was sent outside the digest", n, 1+len(retryDelays))
	}
}

// A retry never runs past the decision's lease: the message goes to the digest
// instead of being sent late.
func TestRetry_PastTheLeaseGoesToTheDigest(t *testing.T) {
	e := newObEnv(t)
	e.del.setFn(failing)
	d := e.submit(cand("a", notice))
	e.tickAt(e.clock.Now().Add(time.Minute))
	if n := e.ob.Stats().RetryPending; n != 1 {
		t.Fatalf("RetryPending = %d", n)
	}

	e.del.setFn(nil) // the channel is back, but too late for this lease
	e.tickAt(e.clock.Now().Add(maxQueueAge))
	if n := len(e.sent()); n != 1 {
		t.Fatalf("%d messages, want the first try only", n)
	}
	if n := e.count(`SELECT COUNT(*) FROM attention_decisions WHERE id = ? AND disposition = 'digest' AND reason = ?`, d.ID, ReasonDeferred); n != 1 {
		t.Fatal("the decision was not moved to the digest")
	}
	if n := e.ob.Stats().RetryPending; n != 0 {
		t.Fatalf("RetryPending = %d", n)
	}
}

// The retries are driven by Run's lanes: an ordinary and a protective message
// each go out again on their own lane.
func TestRetry_OnTheLanes(t *testing.T) {
	for name, opts := range map[string][]func(*proactive.Candidate){
		"ordinary":   nil,
		"protective": {protective},
	} {
		t.Run(name, func(t *testing.T) {
			sev := notice
			if name == "protective" {
				sev = critical
			}
			e := newObEnv(t)
			e.del.setFn(failTimes(1))
			e.startLanes()
			t0 := e.clock.Now()
			d, _ := e.accept(cand("a", sev, opts...))
			// The protective message is on its lane already, and a tick must not race
			// its first try; the ordinary one waits for its window to close.
			if name == "ordinary" {
				e.tickAsyncAt(t0.Add(time.Minute))
			}
			waitUntil(t, "the first try", func() bool { return len(e.sent()) == 1 })
			waitUntil(t, "the retry to be scheduled", func() bool { return e.ob.Stats().RetryPending == 1 })
			e.waitIdle()
			if r := e.rowOf(d.ID); !reserved(r) {
				t.Fatalf("row = %+v, want it still reserved", r)
			}

			e.tickAsyncAt(e.nextRetryDue())
			waitUntil(t, "the retry", func() bool { return len(e.sent()) == 2 })
			e.waitIdle()
			msgs := e.sent()
			if msgs[1].InterventionID != msgs[0].InterventionID {
				t.Fatalf("the retry is a different message: %+v / %+v", msgs[0], msgs[1])
			}
			if r := e.rowOf(d.ID); r.delivered != 1 || r.intervention != msgs[0].InterventionID {
				t.Fatalf("row = %+v", r)
			}
			if st := e.ob.Stats(); st.RetryPending != 0 || st.Retried != 1 {
				t.Fatalf("stats = %+v", st)
			}
		})
	}
}

// logBuf is a log sink safe for concurrent use.
type logBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// A protective Critical is not given up after the ordinary delays: it is retried
// every protectiveRetryEvery, and is marked delivered, once, when the channel
// comes back.
func TestRetry_ProtectiveKeepsRetryingPastTheOrdinarySchedule(t *testing.T) {
	e := newObEnv(t)
	t0 := e.clock.Now()
	back := t0.Add(19 * time.Minute) // the channel is back from then on
	e.del.setFn(func(ctx context.Context, m Message) (bool, error) {
		if e.clock.Now().Before(back) {
			return failing(ctx, m)
		}
		return true, nil
	})
	d := e.submit(cand("liq:btc", critical, protective))
	var ordinary time.Duration
	for _, dl := range retryDelays {
		ordinary += dl
	}
	for i := 0; i < 40 && e.ob.Stats().RetryPending > 0; i++ {
		e.tickNextRetry()
	}
	if n := e.ob.Stats().RetryPending; n != 0 {
		t.Fatalf("RetryPending = %d", n)
	}
	msgs := e.sent()
	// 4 tries by the ordinary schedule, then one every 5 minutes: 9.5, 14.5, 19.5.
	if len(msgs) != 1+len(retryDelays)+3 {
		t.Fatalf("%d tries, want %d", len(msgs), 1+len(retryDelays)+3)
	}
	for _, m := range msgs {
		if m.InterventionID != msgs[0].InterventionID {
			t.Fatalf("a retry is a different message: %+v", m)
		}
	}
	at := e.clock.Now().Sub(t0)
	if at <= ordinary || at < 19*time.Minute || at > 21*time.Minute {
		t.Fatalf("delivered %s after the decision", at)
	}
	if r := e.rowOf(d.ID); r.delivered != 1 || r.intervention != msgs[0].InterventionID {
		t.Fatalf("row = %+v", r)
	}
	if n := e.count(`SELECT COUNT(*) FROM attention_digest_queue`); n != 0 {
		t.Fatal("a protective alert was queued for the digest")
	}
	// delivered once: nothing more is sent.
	for i := 0; i < 4; i++ {
		e.tickAt(e.clock.Now().Add(10 * time.Minute))
	}
	if n := len(e.sent()); n != len(msgs) {
		t.Fatalf("%d messages after more ticks, want %d", n, len(msgs))
	}
	if st := e.ob.Stats(); st.RetriesExhausted != 0 {
		t.Fatalf("stats = %+v", st)
	}
}

// A protective Critical that cannot be delivered for an hour is released, never
// sent to the digest, and the log says so at ERROR.
func TestRetry_ProtectiveIsReleasedAfterAnHour(t *testing.T) {
	var logs logBuf
	logger.SetAdditionalWriter(&logs)
	t.Cleanup(func() { logger.SetAdditionalWriter(nil) })

	e := newObEnv(t)
	t0 := e.clock.Now()
	e.del.setFn(failing)
	d := e.submit(cand("liq:btc", critical, protective)) // flushed inside Submit
	if n := e.ob.Stats().RetryPending; n != 1 || len(e.sent()) != 1 {
		t.Fatalf("pending %d, sent %d", n, len(e.sent()))
	}
	e.exhaustRetries()

	// 4 tries by the ordinary schedule, then one every 5 minutes up to 59.5.
	if n := len(e.sent()); n != 1+len(retryDelays)+11 {
		t.Fatalf("%d tries, want %d", n, 1+len(retryDelays)+11)
	}
	if el := e.clock.Now().Sub(t0); el > protectiveRetryWindow {
		t.Fatalf("tried until %s after the decision", el)
	}
	if r := e.rowOf(d.ID); r.delivered != 0 || !r.deliveredAtSet {
		t.Fatalf("row = %+v, want released", r)
	}
	if n := e.count(`SELECT COUNT(*) FROM attention_digest_queue`); n != 0 {
		t.Fatalf("a protective alert was queued for the digest")
	}
	out := logs.String()
	i := strings.Index(out, "protective alert could not be delivered for 1h")
	if i < 0 {
		t.Fatalf("no log line for the give-up:\n%s", out)
	}
	if !strings.Contains(out[max(0, i-200):i], "ERR") {
		t.Errorf("the give-up is not logged at ERROR:\n%s", out[max(0, i-200):i+60])
	}
}

// A full ordinary retry queue never refuses a protective retry, and protective
// retries have a cap of their own.
func TestRetry_ProtectiveRetryIsNotBlockedByAFullOrdinaryQueue(t *testing.T) {
	e := newObEnv(t)
	filler := func(prot bool) *retryEntry {
		d := Decision{ID: "x"}
		if prot {
			d.Candidate = cand("p", critical, protective)
		}
		return &retryEntry{m: Message{Decisions: []Decision{d}}, due: e.clock.Now().Add(time.Hour)}
	}
	e.ob.mu.Lock()
	for i := 0; i < maxPendingRetries; i++ {
		e.ob.hand.retries = append(e.ob.hand.retries, filler(false))
	}
	e.ob.mu.Unlock()

	e.del.setFn(failing)
	// an ordinary message is refused (and goes to the digest) ...
	o := e.submit(cand("a", notice))
	e.tickAt(e.clock.Now().Add(time.Minute))
	if n := e.count(`SELECT COUNT(*) FROM attention_decisions WHERE id = ? AND disposition = 'digest'`, o.ID); n != 1 {
		t.Fatal("the ordinary message was not moved to the digest")
	}
	// ... a protective one is queued for its retry.
	p := e.submit(cand("liq:btc", critical, protective))
	e.ob.mu.Lock()
	var prot int
	for _, r := range e.ob.hand.retries {
		if r.protective() && r.m.Decisions[0].ID == p.ID {
			prot++
		}
	}
	e.ob.mu.Unlock()
	if prot != 1 {
		t.Fatalf("the protective retry was refused (%d queued)", prot)
	}
	if r := e.rowOf(p.ID); !reserved(r) {
		t.Fatalf("row = %+v, want it reserved for the retry", r)
	}

	// the protective cap is its own.
	e.ob.mu.Lock()
	for i := 0; i < maxPendingProtectiveRetries-1; i++ {
		e.ob.hand.retries = append(e.ob.hand.retries, filler(true))
	}
	e.ob.mu.Unlock()
	q := e.submit(cand("liq:eth", critical, protective))
	if r := e.rowOf(q.ID); r.delivered != 0 || !r.deliveredAtSet {
		t.Fatalf("row past the protective cap = %+v, want released", r)
	}
}

// Stopping with a protective retry pending releases it (it cannot go to the
// digest) and logs it at ERROR.
func TestRetry_ShutdownReleasesAPendingProtectiveRetry(t *testing.T) {
	var logs logBuf
	logger.SetAdditionalWriter(&logs)
	t.Cleanup(func() { logger.SetAdditionalWriter(nil) })

	e := newObEnv(t)
	e.del.setFn(failing)
	stop := e.startLanes()
	d, _ := e.accept(cand("liq:btc", critical, protective))
	waitUntil(t, "the retry to be scheduled", func() bool { return e.ob.Stats().RetryPending == 1 })
	e.waitIdle()

	stop()
	if n := e.ob.Stats().RetryPending; n != 0 {
		t.Fatalf("RetryPending = %d after the stop", n)
	}
	if r := e.rowOf(d.ID); r.delivered != 0 || !r.deliveredAtSet {
		t.Fatalf("row = %+v, want released", r)
	}
	if n := e.count(`SELECT COUNT(*) FROM attention_digest_queue`); n != 0 {
		t.Fatal("a protective alert was queued for the digest")
	}
	if out := logs.String(); !strings.Contains(out, "protective alert not delivered, releasing it") {
		t.Fatalf("the release was not logged:\n%s", out)
	}
}

// Stopping with a retry pending moves its decisions to the digest instead of
// dropping them, and a failure after the stop does not queue another retry that
// nobody would run.
func TestRetry_ShutdownMovesPendingRetriesToTheDigest(t *testing.T) {
	e := newObEnv(t)
	e.del.setFn(failing)
	stop := e.startLanes()
	t0 := e.clock.Now()
	d, _ := e.accept(cand("a", notice))
	e.tickAsyncAt(t0.Add(time.Minute))
	waitUntil(t, "the retry to be scheduled", func() bool { return e.ob.Stats().RetryPending == 1 })
	e.waitIdle()

	stop()
	if n := e.ob.Stats().RetryPending; n != 0 {
		t.Fatalf("RetryPending = %d after the stop", n)
	}
	if n := e.count(`SELECT COUNT(*) FROM attention_decisions WHERE id = ? AND disposition = 'digest' AND reason = ?`, d.ID, ReasonDeferred); n != 1 {
		t.Fatalf("row %s was not moved to the digest", d.ID)
	}
	if n := e.count(`SELECT COUNT(*) FROM attention_digest_queue WHERE flushed_ms IS NULL`); n != 1 {
		t.Fatalf("digest queue = %d, want 1", n)
	}

	// Submit still delivers inline after the stop; its failure goes straight to
	// the digest.
	late := e.submit(cand("late", notice))
	if n := e.ob.Stats().RetryPending; n != 0 {
		t.Fatalf("a retry was queued after the stop")
	}

	e.del.setFn(nil)
	e.tickAt(bkkAt(1, 8, 0))
	dgs := e.digests()
	if len(dgs) != 1 || len(dgs[0].Decisions) != 2 {
		t.Fatalf("digests = %+v", dgs)
	}
	for _, id := range []string{d.ID, late.ID} {
		if r := e.rowOf(id); r.delivered != 1 || r.intervention != dgs[0].InterventionID {
			t.Fatalf("row %s = %+v after the digest", id, r)
		}
	}
}

func TestRenderDigest_MarksDeferredItemsAsDelayed(t *testing.T) {
	at := bkkAt(0, 14, 5)
	_, body := RenderDigest([]Decision{
		{ID: "1", Reason: ReasonDeferred, At: at, Candidate: cand("late", notice)},
		{ID: "2", Reason: ReasonInfoToDigest, At: at, Candidate: cand("plain", info)},
	})
	if !strings.Contains(body, "• title late (delayed: raised at 14:05, could not be delivered on time)") {
		t.Errorf("deferred item is not marked: %q", body)
	}
	if strings.Contains(body, "title plain (") {
		t.Errorf("a plain digest item is marked: %q", body)
	}
}
