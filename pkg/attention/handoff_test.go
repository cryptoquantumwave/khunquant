package attention

import (
	"context"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// countingGate counts the calls that reach the governor.
type countingGate struct {
	Gate
	mu sync.Mutex
	n  int
}

func (c *countingGate) bump() {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
}

func (c *countingGate) total() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

func (c *countingGate) Decide(ctx context.Context, cd proactive.Candidate) (Decision, error) {
	c.bump()
	return c.Gate.Decide(ctx, cd)
}

func (c *countingGate) MarkDelivered(ctx context.Context, d Decision, r Delivery) error {
	c.bump()
	return c.Gate.MarkDelivered(ctx, d, r)
}

func (c *countingGate) PendingDigest(ctx context.Context) (*Digest, error) {
	c.bump()
	return c.Gate.PendingDigest(ctx)
}

func (c *countingGate) CompleteDigest(ctx context.Context, dg *Digest, r Delivery) error {
	c.bump()
	return c.Gate.CompleteDigest(ctx, dg, r)
}

// decideOnly asks the governor, as the pipeline does, and returns the decision.
func (e *obEnv) decideOnly(c proactive.Candidate) Decision {
	e.t.Helper()
	d, err := e.ob.Decide(context.Background(), c)
	if err != nil {
		e.t.Fatalf("Decide(%s): %v", c.DedupKey, err)
	}
	return d
}

// accept decides c and hands it over.
func (e *obEnv) accept(c proactive.Candidate) (Decision, Handoff) {
	e.t.Helper()
	d := e.decideOnly(c)
	return d, e.ob.Accept(d, Item{Candidate: d.Candidate})
}

// startLanes runs the outbox with a tick interval so long that only the explicit
// tickAsync calls tick it, and stops it when the test ends.
func (e *obEnv) startLanes() (stop func()) {
	e.t.Helper()
	e.ob.SetTickInterval(time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.ob.Run(ctx)
	}()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(15 * time.Second):
				e.t.Error("Run did not return")
			}
		})
	}
	e.t.Cleanup(stop)
	return stop
}

// tickAsync ticks the hand-over form once.
func (e *obEnv) tickAsync() {
	e.t.Helper()
	if err := e.ob.tick(context.Background(), true); err != nil {
		e.t.Fatalf("tick: %v", err)
	}
}

func (e *obEnv) tickAsyncAt(t time.Time) {
	e.t.Helper()
	e.clock.Set(t)
	e.tickAsync()
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func (e *obEnv) waitIdle() {
	e.t.Helper()
	waitUntil(e.t, "the outbox to go idle", e.ob.idle)
}

func TestAccept_DoesNoIOOnTheCallersGoroutine(t *testing.T) {
	e := newObEnv(t)
	cg := &countingGate{Gate: e.g}
	release := make(chan struct{})
	var calls sync.WaitGroup
	calls.Add(1)
	var once sync.Once
	del := &fakeDeliverer{fn: func(ctx context.Context, m Message) (bool, error) {
		once.Do(calls.Done)
		<-release // never returns during the test: a channel that has hung
		return true, nil
	}}
	t.Cleanup(func() { close(release) })
	ob := NewOutbox(cg, del, time.Minute, e.clock.Now)
	ob.SetTickInterval(time.Hour)

	d := e.decide(cand("liq:btc", critical, protective)) // the pipeline's own Decide
	before := cg.total()
	got := make(chan Handoff, 1)
	go func() { got <- ob.Accept(d, Item{}) }()
	select {
	case h := <-got:
		if h != HandoffQueued {
			t.Fatalf("Accept = %s, want queued", h)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Accept blocked")
	}
	if cg.total() != before {
		t.Fatalf("Accept called the governor %d times", cg.total()-before)
	}
	if n := len(del.messages()); n != 0 {
		t.Fatalf("Accept delivered %d messages inline", n)
	}

	// With the lanes running and the protective worker stuck in Deliver, further
	// Accepts still return at once, ordinary and protective alike.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		ob.Run(ctx)
	}()
	calls.Wait() // the worker is now inside the blocked Deliver
	for i, c := range []proactive.Candidate{
		cand("n"+strconv.Itoa(1), notice),
		cand("liq:eth", critical, protective),
		cand("liq:sol", critical, protective),
	} {
		d := e.decide(c)
		start := time.Now()
		ob.Accept(d, Item{Candidate: d.Candidate})
		if el := time.Since(start); el > time.Second {
			t.Fatalf("Accept #%d took %s", i, el)
		}
	}
	cancel()
	// The stuck deliverer ignores its context, so Run gives up on it after a
	// bounded wait; the test releases it afterwards.
	select {
	case <-done:
	case <-time.After(shutdownFlushTimeout + laneStopGrace + 5*time.Second):
		t.Fatal("Run hung on a deliverer that never returns")
	}
}

func TestAccept_IgnoresWhatIsNotAMessage(t *testing.T) {
	e := newObEnv(t)
	e.tick() // prime the digest
	d, h := e.accept(cand("i", info))
	if d.Disposition != proactive.DispDigest || h != HandoffIgnored {
		t.Fatalf("digest decision: %s / %s", d.Disposition, h)
	}
	if st := e.ob.Stats(); st.OrdinaryHandOvers+st.ProtectiveHandOvers != 0 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestAccept_ProtectiveOvertakesASlowOrdinaryDelivery(t *testing.T) {
	e := newObEnv(t)
	slow := make(chan struct{})
	e.del.setFn(func(ctx context.Context, m Message) (bool, error) {
		if m.Severity < critical {
			<-slow // the channel is slow for an ordinary message
		}
		return true, nil
	})
	t.Cleanup(func() { close(slow) })
	e.startLanes()

	t0 := e.clock.Now()
	e.accept(cand("a", notice))
	e.tickAsyncAt(t0.Add(time.Minute)) // the window closed: the group is handed over
	waitUntil(t, "the ordinary delivery to start", func() bool { return len(e.del.messages()) == 1 })

	start := time.Now()
	_, h := e.accept(cand("liq:btc", critical, protective))
	if h != HandoffQueued {
		t.Fatalf("Accept = %s", h)
	}
	waitUntil(t, "the protective message", func() bool { return len(e.del.messages()) == 2 })
	if el := time.Since(start); el > time.Second {
		t.Fatalf("protective message took %s behind a slow ordinary one", el)
	}
	if m := e.del.messages()[1]; m.Severity != critical || len(m.Decisions) != 1 {
		t.Fatalf("second message = %+v", m)
	}
}

func TestAccept_EveryMemberIsMarked(t *testing.T) {
	for name, fn := range map[string]func(context.Context, Message) (bool, error){
		"delivered": nil,
		"failed":    failing,
	} {
		t.Run(name, func(t *testing.T) {
			e := newObEnv(t)
			e.del.setFn(fn)
			e.startLanes()
			t0 := e.clock.Now()
			var ds []Decision
			for _, k := range []string{"a", "b", "c"} {
				d, _ := e.accept(cand(k, notice))
				ds = append(ds, d)
			}
			e.tickAsyncAt(t0.Add(time.Minute))
			e.waitIdle()
			msgs := e.del.messages()
			if len(msgs) != 1 || len(msgs[0].Decisions) != 3 {
				t.Fatalf("messages = %+v", msgs)
			}
			for _, d := range ds {
				r := e.rowOf(d.ID)
				if name == "delivered" && (r.delivered != 1 || r.intervention != msgs[0].InterventionID) {
					t.Fatalf("row %s = %+v", d.ID, r)
				}
				if name == "failed" && (r.delivered != 0 || !r.deliveredAtSet) {
					t.Fatalf("row %s = %+v, want released", d.ID, r)
				}
			}
		})
	}
}

// The existing acceptance replays, driven through Decide + Accept and the lanes.
func TestAccept_AcceptanceReplay_DigestHoldsTheRest(t *testing.T) {
	cfg := digestCfg()
	cfg.GroupWindowSeconds = 20
	e := newObEnvCfg(t, cfg)
	e.startLanes()
	runReplayAccept(t, e, 15)

	msgs := e.del.messages()
	if len(msgs) != 3 {
		t.Fatalf("%d messages during the replay, want exactly the budget of 3", len(msgs))
	}
	sent := 0
	for _, m := range msgs {
		if m.Digest {
			t.Fatal("digest sent during the replay")
		}
		sent += len(m.Decisions)
	}
	queued := e.count(`SELECT COUNT(*) FROM attention_digest_queue WHERE flushed_ms IS NULL`)
	if sent+queued != 15 || queued == 0 {
		t.Fatalf("sent %d + queued %d != 15 (or nothing queued)", sent, queued)
	}
	if n := e.count(`SELECT COUNT(*) FROM attention_decisions WHERE reason = ?`, ReasonBudget); n != queued {
		t.Fatalf("budget decisions = %d, queued = %d", n, queued)
	}
	e.tickAsyncAt(bkkAt(1, 8, 0))
	e.waitIdle()
	msgs = e.del.messages()
	if len(msgs) != 4 {
		t.Fatalf("%d messages after the digest time, want 4", len(msgs))
	}
	if dg := msgs[3]; !dg.Digest || !dg.Silent || len(dg.Decisions) != queued {
		t.Fatalf("digest = %+v", dg)
	}
	if n := e.count(`SELECT COUNT(DISTINCT intervention_id) FROM attention_decisions WHERE disposition = 'notify' AND delivered = 1`); n != 3 {
		t.Fatalf("distinct interventions = %d, want 3", n)
	}
	if n := e.count(`SELECT COUNT(*) FROM attention_decisions WHERE delivered = 0 AND disposition IN ('notify','escalate')`); n != 0 {
		t.Fatalf("%d notify rows never reported", n)
	}
}

func TestAccept_AcceptanceReplay_DefaultWindowFitsInThreeMessages(t *testing.T) {
	e := newObEnv(t)
	e.startLanes()
	runReplayAccept(t, e, 15)
	msgs := e.del.messages()
	if len(msgs) != 3 {
		t.Fatalf("%d messages, want 3", len(msgs))
	}
	for _, m := range msgs {
		if len(m.Decisions) != 5 {
			t.Fatalf("message has %d decisions, want 5", len(m.Decisions))
		}
	}
	e.tickAsyncAt(bkkAt(1, 8, 0))
	e.waitIdle()
	if n := len(e.del.messages()); n != 3 {
		t.Fatalf("an empty digest sent a message: %d", n)
	}
}

// runReplayAccept is runReplay with the new entry point: the same candidates at
// the same simulated times, one simulated second per step.
func runReplayAccept(t *testing.T, e *obEnv, n int) {
	t.Helper()
	e.tickAsync() // prime the digest
	e.waitIdle()
	start := e.clock.Now()
	last := (n-1)*12 + 90
	for s := 0; s <= last; s++ {
		e.clock.Set(start.Add(time.Duration(s) * time.Second))
		if s%12 == 0 && s/12 < n {
			e.accept(cand("c"+strconv.Itoa(s/12), notice))
		}
		e.tickAsync()
		e.waitIdle()
	}
}

func TestAccept_FullLaneDefersToTheDigestAndProtectiveStillGoes(t *testing.T) {
	e := newObEnv(t)
	e.ob.ordSlots = 1
	t0 := e.clock.Now()

	a, _ := e.accept(cand("a", notice))
	e.tickAsyncAt(t0.Add(time.Minute)) // group 1 takes the only slot
	b, _ := e.accept(cand("b", notice))
	e.tickAsyncAt(t0.Add(2 * time.Minute)) // group 2 finds the lane full
	st := e.ob.Stats()
	if st.OrdinaryQueued != 1 || st.OrdinaryHandOvers != 1 || st.Deferred != 1 {
		t.Fatalf("stats = %+v, want one queued and one deferred", st)
	}
	if r, d := e.count(`SELECT COUNT(*) FROM attention_digest_queue WHERE decision_id = ? AND flushed_ms IS NULL`, b.ID),
		e.count(`SELECT COUNT(*) FROM attention_decisions WHERE id = ? AND disposition = 'digest' AND reason = ?`, b.ID, ReasonDeferred); r != 1 || d != 1 {
		t.Fatalf("deferred decision: queue rows %d, digest rows %d", r, d)
	}
	// the deferred one no longer reserves budget, the queued one still does.
	if n := e.count(`SELECT COUNT(*) FROM attention_decisions WHERE disposition IN ('notify','escalate') AND delivered = 0 AND delivered_at_ms IS NULL`); n != 1 {
		t.Fatalf("%d decisions still reserving budget, want 1", n)
	}

	// a protective Critical is never deferred, whatever the ordinary lane looks like.
	_, h := e.accept(cand("liq:btc", critical, protective))
	if h != HandoffQueued {
		t.Fatalf("protective Accept = %s", h)
	}
	e.startLanes()
	e.waitIdle()
	msgs := e.del.messages()
	if len(msgs) != 2 {
		t.Fatalf("messages = %+v", msgs)
	}
	var sawProtective, sawA bool
	for _, m := range msgs {
		for _, d := range m.Decisions {
			sawProtective = sawProtective || d.Candidate.DedupKey == "liq:btc"
			sawA = sawA || d.ID == a.ID
		}
	}
	if !sawProtective || !sawA {
		t.Fatalf("protective delivered %v, queued ordinary delivered %v", sawProtective, sawA)
	}

	// and the deferred decision reaches the owner in the digest.
	e.tickAsyncAt(bkkAt(1, 8, 0))
	e.waitIdle()
	msgs = e.del.messages()
	last := msgs[len(msgs)-1]
	if !last.Digest || len(last.Decisions) != 1 || last.Decisions[0].ID != b.ID {
		t.Fatalf("digest = %+v", last)
	}
}

func TestAccept_FullBufferGoesToTheLaneWithoutATick(t *testing.T) {
	e := newObEnv(t)
	for i := 0; i < maxBuffered+1; i++ {
		e.accept(cand("k"+strconv.Itoa(i), notice))
	}
	st := e.ob.Stats()
	if st.OrdinaryHandOvers != 1 || st.OrdinaryQueued != 1 {
		t.Fatalf("stats = %+v, want the full group queued", st)
	}
	e.ob.mu.Lock()
	held := len(e.ob.buf)
	e.ob.mu.Unlock()
	if held != 1 {
		t.Fatalf("%d decisions still buffered, want 1", held)
	}
}

func TestAccept_QueuedPastItsLeaseIsDeferredNotSent(t *testing.T) {
	e := newObEnv(t)
	t0 := e.clock.Now()
	d, _ := e.accept(cand("slow", notice))
	e.tickAsyncAt(t0.Add(time.Minute)) // queued; no worker yet, as if the lane were busy
	if e.ob.Stats().OrdinaryQueued != 1 {
		t.Fatal("not queued")
	}
	e.clock.Set(t0.Add(maxQueueAge + time.Minute))
	e.startLanes()
	e.waitIdle()
	if n := len(e.del.messages()); n != 0 {
		t.Fatalf("a message outside its lease was sent: %d", n)
	}
	st := e.ob.Stats()
	if st.Aged != 1 || st.Deferred != 1 {
		t.Fatalf("stats = %+v", st)
	}
	if n := e.count(`SELECT COUNT(*) FROM attention_decisions WHERE id = ? AND disposition = 'digest' AND reason = ?`, d.ID, ReasonDeferred); n != 1 {
		t.Fatal("decision not deferred to the digest")
	}
}

func TestAccept_ProtectiveRepeatsCoalesceByDedupKey(t *testing.T) {
	e := newObEnv(t)
	var ds []Decision
	for i := 0; i < 5; i++ {
		d, _ := e.accept(cand("liq:btc", critical, protective))
		ds = append(ds, d)
	}
	e.accept(cand("liq:eth", critical, protective))
	st := e.ob.Stats()
	if st.ProtectivePending != 2 || st.Superseded != 4 {
		t.Fatalf("stats = %+v, want 2 pending and 4 superseded", st)
	}
	e.tickAsync() // releases the superseded rows
	for _, d := range ds[:4] {
		if r := e.rowOf(d.ID); r.delivered != 0 || !r.deliveredAtSet {
			t.Fatalf("superseded row %s = %+v, want released", d.ID, r)
		}
	}
	e.startLanes()
	e.waitIdle()
	msgs := e.del.messages()
	if len(msgs) != 1 || len(msgs[0].Decisions) != 2 {
		t.Fatalf("messages = %+v, want one message of the two live decisions", msgs)
	}
	if r := e.rowOf(ds[4].ID); r.delivered != 1 {
		t.Fatalf("newest row = %+v", r)
	}
}

func TestAccept_ProtectiveLaneNeverConsultsTheComposer(t *testing.T) {
	e := newObEnv(t)
	comp := &stubComposer{out: Message{Title: "composed"}}
	e.ob.Composer = comp
	e.startLanes()
	t0 := e.clock.Now()

	e.accept(cand("llm", warning, composeLLM)) // buffered, an escalate
	e.accept(cand("liq:btc", critical, protective))
	e.waitIdle()
	if comp.calls != 0 {
		t.Fatalf("the composer was consulted %d times for a protective message", comp.calls)
	}
	msgs := e.del.messages()
	if len(msgs) != 1 || msgs[0].Title == "composed" || len(msgs[0].Decisions) != 2 {
		t.Fatalf("messages = %+v, want the template of both decisions", msgs)
	}

	// an escalate on its own does go through it, on the ordinary lane.
	e.accept(cand("llm2", warning, composeLLM))
	e.tickAsyncAt(t0.Add(2 * time.Minute))
	e.waitIdle()
	if comp.calls != 1 {
		t.Fatalf("composer calls = %d, want 1", comp.calls)
	}
}

// Whichever path flushes it, a message with a protective Critical in it is the
// template, at once: attention decides a protective Critical that asks for
// composition "escalate", and the composer must not be asked about it.
func TestOutbox_ProtectiveCriticalIsNeverComposed_OnAnyPath(t *testing.T) {
	e := newObEnv(t)
	comp := &stubComposer{out: Message{Title: "composed"}}
	e.ob.Composer = comp
	d := e.submit(cand("liq:btc", critical, protective, composeLLM)) // Submit flushes it inline
	if d.Disposition != proactive.DispEscalate || d.Reason != ReasonProtective {
		t.Fatalf("decision = %s/%s, want the protective escalate", d.Disposition, d.Reason)
	}
	msgs := e.del.messages()
	if comp.calls != 0 || len(msgs) != 1 || msgs[0].Title != "title liq:btc" {
		t.Fatalf("composer calls %d, messages %+v", comp.calls, msgs)
	}
	// and a buffered escalate that rides along is not composed either.
	e.submit(cand("llm", warning, composeLLM))
	e.submit(cand("liq:eth", critical, protective))
	if comp.calls != 0 || len(e.del.messages()) != 2 {
		t.Fatalf("composer calls %d, messages %d", comp.calls, len(e.del.messages()))
	}
}

func TestAccept_RunStopFlushesAndLeavesNoWorker(t *testing.T) {
	e := newObEnv(t)
	stop := e.startLanes()
	var ds []Decision
	for _, k := range []string{"a", "b"} {
		d, _ := e.accept(cand(k, notice)) // the window has not elapsed
		ds = append(ds, d)
	}
	stop()

	msgs := e.del.messages()
	if len(msgs) != 1 || len(msgs[0].Decisions) != 2 {
		t.Fatalf("shutdown flush = %+v", msgs)
	}
	for _, d := range ds {
		if r := e.rowOf(d.ID); r.delivered != 1 {
			t.Fatalf("row %s = %+v", d.ID, r)
		}
	}
	buf := make([]byte, 1<<20)
	if stacks := string(buf[:runtime.Stack(buf, true)]); strings.Contains(stacks, "(*Outbox).laneLoop") {
		t.Fatal("a lane worker outlived Run")
	}

	// after the stop an Accept does nothing: no inline delivery, no governor call.
	d := e.decide(cand("late", notice))
	if h := e.ob.Accept(d, Item{Candidate: d.Candidate}); h != HandoffClosed {
		t.Fatalf("Accept after stop = %s", h)
	}
	if n := len(e.del.messages()); n != 1 {
		t.Fatalf("a late Accept delivered: %d messages", n)
	}
	if e.ob.Stats().RejectedClosed != 1 {
		t.Fatalf("stats = %+v", e.ob.Stats())
	}
	// Submit's behaviour after the stop is unchanged: it delivers inline.
	e.submit(cand("late2", notice))
	if n := len(e.del.messages()); n != 2 {
		t.Fatalf("Submit after stop: %d messages", n)
	}
}

func TestAccept_PanickingDelivererAndComposerAreSurvived(t *testing.T) {
	e := newObEnv(t)
	e.del.setFn(func(context.Context, Message) (bool, error) { panic("boom") })
	e.startLanes()
	t0 := e.clock.Now()
	d, _ := e.accept(cand("a", notice))
	e.tickAsyncAt(t0.Add(time.Minute))
	e.waitIdle()
	if r := e.rowOf(d.ID); r.delivered != 0 || !r.deliveredAtSet {
		t.Fatalf("row after a panicking deliverer = %+v, want released", r)
	}
	if e.ob.Stats().Panics != 1 {
		t.Fatalf("stats = %+v", e.ob.Stats())
	}

	// a panicking composer falls back to the template.
	e.del.setFn(nil)
	e.ob.Composer = panicComposer{}
	e.accept(cand("llm", warning, composeLLM))
	e.tickAsyncAt(t0.Add(3 * time.Minute))
	e.waitIdle()
	msgs := e.del.messages()
	if last := msgs[len(msgs)-1]; last.Title != "title llm" {
		t.Fatalf("message after a panicking composer = %+v", last)
	}
}

type panicComposer struct{}

func (panicComposer) Compose(context.Context, Message, []Item) (Message, error) { panic("composer") }

func TestDeferToDigest_IsIdempotentAndNeverDefersProtective(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	d := e.decide(cand("a", notice))
	if d.Disposition != proactive.DispNotify {
		t.Fatalf("decision = %s", d.Disposition)
	}
	for i := 0; i < 2; i++ {
		if err := e.g.DeferToDigest(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	if n := e.count(`SELECT COUNT(*) FROM attention_digest_queue WHERE decision_id = ?`, d.ID); n != 1 {
		t.Fatalf("queue rows = %d", n)
	}

	p := e.decide(cand("liq:btc", critical, protective))
	if err := e.g.DeferToDigest(ctx, p); err == nil {
		t.Fatal("a protective decision was deferred")
	}
	if n := e.count(`SELECT COUNT(*) FROM attention_digest_queue WHERE decision_id = ?`, p.ID); n != 0 {
		t.Fatal("a protective decision was queued")
	}

	// a delivered decision is left alone.
	s := e.send(cand("sent", warning))
	if err := e.g.DeferToDigest(ctx, s); err != nil {
		t.Fatal(err)
	}
	if n := e.count(`SELECT COUNT(*) FROM attention_decisions WHERE id = ? AND disposition = 'notify' AND delivered = 1`, s.ID); n != 1 {
		t.Fatal("a delivered decision was changed")
	}
}
