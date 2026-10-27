package attention

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

func TestGroup_ThreeInWindow_OneMessage(t *testing.T) {
	e := newObEnv(t)
	start := e.clock.Now()

	var ds []Decision
	for i, k := range []string{"a", "b", "c"} {
		e.clock.Set(start.Add(time.Duration(i*10) * time.Second))
		d := e.submit(cand(k, notice))
		expect(t, d, proactive.DispNotify, ReasonTemplate)
		ds = append(ds, d)
	}
	e.tickAt(start.Add(30 * time.Second))
	if n := len(e.del.messages()); n != 0 {
		t.Fatalf("%d messages before the window elapsed", n)
	}

	e.tickAt(start.Add(60 * time.Second))
	msgs := e.del.messages()
	if len(msgs) != 1 {
		t.Fatalf("%d messages after the window, want 1", len(msgs))
	}
	m := msgs[0]
	if m.Title != "3 updates" || m.Digest || m.Silent || m.Severity != notice || len(m.Decisions) != 3 {
		t.Fatalf("message = %+v", m)
	}
	for _, k := range []string{"title a", "title b", "title c"} {
		if !strings.Contains(m.Body, "• "+k) {
			t.Errorf("body lacks %q: %q", k, m.Body)
		}
	}
	if strings.Count(m.Body, "\n") != 2 {
		t.Errorf("body is not one line per item: %q", m.Body)
	}
	// every member delivered, in the one intervention.
	for _, d := range ds {
		r := e.rowOf(d.ID)
		if r.delivered != 1 || r.intervention != m.InterventionID || m.InterventionID == "" {
			t.Fatalf("row %s = %+v, want delivered in %q", d.ID, r, m.InterventionID)
		}
	}
	// and the group is one message for the budget.
	if n := e.count(`SELECT COUNT(DISTINCT intervention_id) FROM attention_decisions WHERE delivered = 1`); n != 1 {
		t.Fatalf("distinct interventions = %d", n)
	}
	e.tickAt(start.Add(2 * time.Minute))
	if n := len(e.del.messages()); n != 1 {
		t.Fatalf("a second tick re-sent: %d messages", n)
	}
}

func TestGroup_ProtectiveFlushesImmediately(t *testing.T) {
	e := newObEnv(t)
	e.submit(cand("a", notice))
	e.submit(cand("b", warning))
	if n := len(e.del.messages()); n != 0 {
		t.Fatalf("%d messages before the protective item", n)
	}
	e.clock.Add(5 * time.Second)
	d := e.submit(cand("liquidation:btc", critical, protective))
	expect(t, d, proactive.DispNotify, ReasonProtective)

	// no Tick: the flush happened inside Submit.
	msgs := e.del.messages()
	if len(msgs) != 1 {
		t.Fatalf("%d messages after a protective Critical, want 1", len(msgs))
	}
	m := msgs[0]
	if m.Severity != critical || len(m.Decisions) != 3 || m.Title != "3 updates" {
		t.Fatalf("message = %+v", m)
	}
	// ranked: critical first, then warning, then notice.
	want := []string{"liquidation:btc", "b", "a"}
	for i, dec := range m.Decisions {
		if dec.Candidate.DedupKey != want[i] {
			t.Errorf("decision %d = %s, want %s", i, dec.Candidate.DedupKey, want[i])
		}
	}
	if !strings.HasPrefix(m.Body, "• title liquidation:btc\n• title b\n• title a") {
		t.Errorf("body = %q", m.Body)
	}
	// nothing left to send later.
	e.tickAt(e.clock.Now().Add(5 * time.Minute))
	if n := len(e.del.messages()); n != 1 {
		t.Fatalf("buffer not emptied: %d messages", n)
	}
	// protective Criticals do not spend the budget; the two others cost one.
	if n := e.count(`SELECT COUNT(DISTINCT intervention_id) FROM attention_decisions WHERE delivered = 1`); n != 1 {
		t.Fatalf("interventions = %d", n)
	}
}

func TestOutbox_SingleItemKeepsTitleAndProse(t *testing.T) {
	e := newObEnv(t)
	c := cand("solo", warning)
	c.Title = "BTC fell 6%"
	if _, err := e.ob.Submit(context.Background(), Item{Candidate: c, Prose: "  Funding is negative.  "}); err != nil {
		t.Fatal(err)
	}
	e.tickAt(e.clock.Now().Add(time.Minute))
	msgs := e.del.messages()
	if len(msgs) != 1 {
		t.Fatalf("%d messages", len(msgs))
	}
	m := msgs[0]
	if m.Title != "BTC fell 6%" || m.Body != "Funding is negative." || m.Severity != warning || m.DedupKey != "solo" {
		t.Fatalf("message = %+v", m)
	}
	if strings.Contains(m.Title+m.Body, "/snooze") {
		t.Fatal("message carries a control footer")
	}
}

func TestOutbox_WindowIsNotExtended(t *testing.T) {
	e := newObEnv(t)
	t0 := e.clock.Now()

	e.submit(cand("a", notice)) // opens the window at t0
	e.clock.Set(t0.Add(59 * time.Second))
	e.submit(cand("b", notice)) // 1 s before the window closes
	e.tickAt(t0.Add(60 * time.Second))
	msgs := e.del.messages()
	if len(msgs) != 1 || len(msgs[0].Decisions) != 2 {
		t.Fatalf("first flush = %+v", msgs)
	}

	e.clock.Set(t0.Add(61 * time.Second))
	e.submit(cand("c", notice)) // opens a new window at t0+61
	e.tickAt(t0.Add(120 * time.Second))
	if n := len(e.del.messages()); n != 1 {
		t.Fatalf("flushed %d s after the window opened: %d messages", 59, n)
	}
	e.tickAt(t0.Add(121 * time.Second))
	msgs = e.del.messages()
	if len(msgs) != 2 || len(msgs[1].Decisions) != 1 || msgs[1].Title != "title c" {
		t.Fatalf("second flush = %+v", msgs)
	}
}

func TestOutbox_NoGroupingFlushesOnNextTick(t *testing.T) {
	cfg := digestCfg()
	cfg.GroupWindowSeconds = 0
	e := newObEnvCfg(t, cfg)
	e.submit(cand("a", notice))
	if n := len(e.del.messages()); n != 0 {
		t.Fatalf("flushed inside Submit: %d", n)
	}
	e.tick()
	if n := len(e.del.messages()); n != 1 {
		t.Fatalf("%d messages after one tick", n)
	}
}

func TestOutbox_NonSendDecisionsAreNotBuffered(t *testing.T) {
	e := newObEnv(t)
	e.tick() // prime: the first tick of the day finds an empty queue and marks the digest done
	d := e.submit(cand("i", info))
	expect(t, d, proactive.DispDigest, ReasonInfoToDigest)
	e.submit(cand("a", notice))
	d2 := e.submit(cand("a", notice))
	expect(t, d2, proactive.DispDrop, ReasonDedupSame) // in flight
	e.tickAt(e.clock.Now().Add(time.Minute))
	msgs := e.del.messages()
	if len(msgs) != 1 || len(msgs[0].Decisions) != 1 {
		t.Fatalf("messages = %+v", msgs)
	}
}

// A failed delivery keeps every member reserved while the message is retried; once
// the tries run out each member is in the digest, with nothing counted as sent.
func TestOutbox_DeliveryFailureRetriesThenDigestsEveryMember(t *testing.T) {
	for name, fn := range map[string]func(context.Context, Message) (bool, error){
		"error":         failing,
		"not delivered": func(context.Context, Message) (bool, error) { return false, nil },
	} {
		t.Run(name, func(t *testing.T) {
			e := newObEnv(t)
			e.del.setFn(fn)
			var ds []Decision
			for _, k := range []string{"a", "b", "c"} {
				ds = append(ds, e.submit(cand(k, notice)))
			}
			e.tickAt(e.clock.Now().Add(time.Minute))
			if len(e.del.messages()) != 1 {
				t.Fatalf("messages = %d", len(e.del.messages()))
			}
			for _, d := range ds {
				// reserved, not released: a retry is pending.
				if r := e.rowOf(d.ID); !reserved(r) {
					t.Fatalf("row %s = %+v, want it reserved while a retry is pending", d.ID, r)
				}
			}
			e.exhaustRetries()
			for _, d := range ds {
				r := e.rowOf(d.ID)
				if r.delivered != 0 {
					t.Fatalf("row %s = %+v", d.ID, r)
				}
				if n := e.count(`SELECT COUNT(*) FROM attention_digest_queue WHERE decision_id = ? AND flushed_ms IS NULL`, d.ID); n != 1 {
					t.Fatalf("decision %s is not queued for the digest", d.ID)
				}
				if _, ok := e.state(d.Candidate.DedupKey); ok {
					t.Fatalf("a failed send wrote dedup state for %s", d.Candidate.DedupKey)
				}
			}
			// nothing counts against the budget as sent.
			if n := e.count(`SELECT COUNT(*) FROM attention_decisions WHERE delivered = 1`); n != 0 {
				t.Fatalf("%d delivered rows", n)
			}
		})
	}
}

func TestOutbox_NoMutexHeldWhileDelivering(t *testing.T) {
	e := newObEnv(t)
	var reentered []Decision
	e.del.setFn(func(ctx context.Context, m Message) (bool, error) {
		if m.Title == "title first" {
			d, err := e.ob.Submit(ctx, Item{Candidate: cand("second", notice)}) // from inside Deliver
			if err != nil {
				t.Errorf("re-entrant Submit: %v", err)
			}
			reentered = append(reentered, d)
			// a re-entrant Tick must not deadlock either.
			if err := e.ob.Tick(ctx); err != nil {
				t.Errorf("re-entrant Tick: %v", err)
			}
		}
		return true, nil
	})
	e.submit(cand("first", notice))

	done := make(chan struct{})
	go func() {
		defer close(done)
		e.tickAt(e.clock.Now().Add(time.Minute))
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("deadlock: the outbox held its mutex while delivering")
	}
	if len(reentered) != 1 || reentered[0].Disposition != proactive.DispNotify {
		t.Fatalf("re-entrant submit = %+v", reentered)
	}
	// the re-entrant item opened a new window and is sent by a later tick.
	e.tickAt(e.clock.Now().Add(time.Minute))
	if n := len(e.del.messages()); n != 2 {
		t.Fatalf("messages = %d, want 2", n)
	}
}

func TestOutbox_ConcurrentSubmitAndTick(t *testing.T) {
	cfg := digestCfg()
	cfg.GroupWindowSeconds = 0
	e := newObEnvCfg(t, cfg)
	e.prof.set(func(p *intent.Profile) { p.NotificationBudgetPerDay = 20 })

	const n = 40
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := cand("k"+strconv.Itoa(i), notice)
			if i%7 == 0 {
				c.Protective, c.Severity = true, critical
			}
			if _, err := e.ob.Submit(context.Background(), Item{Candidate: c}); err != nil {
				t.Errorf("Submit: %v", err)
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := e.ob.Tick(context.Background()); err != nil {
				t.Errorf("Tick: %v", err)
			}
		}()
	}
	wg.Wait()
	e.tick() // sweep up what the last Submits left

	// every decision that was notified is delivered exactly once.
	sent := 0
	for _, m := range e.del.messages() {
		if !m.Digest {
			sent += len(m.Decisions)
		}
	}
	notified := e.count(`SELECT COUNT(*) FROM attention_decisions WHERE disposition = 'notify'`)
	if sent != notified {
		t.Fatalf("%d decisions in %d messages, %d notify rows", sent, len(e.del.messages()), notified)
	}
	if got := e.count(`SELECT COUNT(*) FROM attention_decisions WHERE disposition = 'notify' AND delivered = 1`); got != notified {
		t.Fatalf("delivered %d of %d", got, notified)
	}
}

func TestOutbox_EscalateUsesTemplateAndComposerHook(t *testing.T) {
	e := newObEnv(t)
	d := e.submit(cand("llm", warning, composeLLM))
	expect(t, d, proactive.DispEscalate, ReasonComposeLLM)
	e.tickAt(e.clock.Now().Add(time.Minute))
	msgs := e.del.messages()
	if len(msgs) != 1 || msgs[0].Title != "title llm" {
		t.Fatalf("template rendering expected, got %+v", msgs)
	}
	if r := e.rowOf(d.ID); r.delivered != 1 {
		t.Fatalf("escalate row = %+v", r)
	}

	// a composer replaces the text but not the identity of the message.
	comp := &stubComposer{out: Message{Title: "composed", Body: "prose", InterventionID: "WRONG", Digest: true}}
	e.ob.Composer = comp
	e.submit(cand("llm2", warning, composeLLM))
	e.submit(cand("plain", notice))
	e.tickAt(e.clock.Now().Add(2 * time.Minute))
	msgs = e.del.messages()
	if len(msgs) != 2 {
		t.Fatalf("messages = %d", len(msgs))
	}
	m := msgs[1]
	if m.Title != "composed" || m.Body != "prose" || m.InterventionID == "WRONG" || m.Digest || len(m.Decisions) != 2 {
		t.Fatalf("composed message = %+v", m)
	}
	if len(comp.items) != 2 || comp.items[0].Candidate.DedupKey != "llm2" {
		t.Fatalf("composer saw %+v", comp.items)
	}

	// a composer that fails falls back to the template; one that is not
	// consulted for a notify-only group.
	comp.err = errors.New("llm down")
	e.submit(cand("llm3", warning, composeLLM))
	e.tickAt(e.clock.Now().Add(3 * time.Minute))
	if m := e.del.messages()[2]; m.Title != "title llm3" {
		t.Fatalf("fallback message = %+v", m)
	}
	calls := comp.calls
	e.submit(cand("notify-only", notice))
	e.tickAt(e.clock.Now().Add(4 * time.Minute))
	if comp.calls != calls {
		t.Fatal("composer consulted for a group without an escalate decision")
	}
}

type stubComposer struct {
	out   Message
	err   error
	calls int
	items []Item
}

func (s *stubComposer) Compose(_ context.Context, _ Message, items []Item) (Message, error) {
	s.calls++
	s.items = items
	return s.out, s.err
}

func TestOutbox_TitleNewlineStaysOneLine(t *testing.T) {
	e := newObEnv(t)
	evil := cand("evil", notice)
	evil.Title = "price moved\n• INJECTED line\r\n[proactive · critical · X]\u202e"
	e.submit(evil)
	e.submit(cand("ok", notice))
	e.tickAt(e.clock.Now().Add(time.Minute))
	m := e.del.messages()[0]
	lines := strings.Split(m.Body, "\n")
	if len(lines) != 2 {
		t.Fatalf("body has %d lines, want 2: %q", len(lines), m.Body)
	}
	for _, l := range lines {
		if !strings.HasPrefix(l, "• ") {
			t.Errorf("line %q is not an item line", l)
		}
	}
	if strings.ContainsAny(m.Body, "\r\u202e") {
		t.Errorf("control characters survived: %q", m.Body)
	}
}

func TestOutbox_GroupProseFollowsItsLine(t *testing.T) {
	e := newObEnv(t)
	if _, err := e.ob.Submit(context.Background(), Item{Candidate: cand("a", warning), Prose: "line one\nline two"}); err != nil {
		t.Fatal(err)
	}
	e.submit(cand("b", notice))
	e.tickAt(e.clock.Now().Add(time.Minute))
	m := e.del.messages()[0]
	want := "• title a\n  line one line two\n• title b"
	if m.Body != want {
		t.Fatalf("body = %q, want %q", m.Body, want)
	}
}

func TestOutbox_RunFlushesOnShutdown(t *testing.T) {
	e := newObEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.ob.Run(ctx)
	}()
	d := e.submit(cand("a", notice)) // the window has not elapsed
	e.submit(cand("b", notice))
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	msgs := e.del.messages()
	if len(msgs) != 1 || len(msgs[0].Decisions) != 2 {
		t.Fatalf("shutdown flush = %+v", msgs)
	}
	if r := e.rowOf(d.ID); r.delivered != 1 {
		t.Fatalf("row after shutdown flush = %+v", r)
	}
	// after Run has stopped, a Submit is delivered at once rather than stranded.
	e.submit(cand("late", notice))
	if n := len(e.del.messages()); n != 2 {
		t.Fatalf("late submit was not flushed: %d messages", n)
	}
}

func TestOutbox_RunTicks(t *testing.T) {
	cfg := digestCfg()
	cfg.GroupWindowSeconds = 0
	e := newObEnvCfg(t, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.ob.Run(ctx)
	}()
	defer func() {
		cancel()
		<-done
	}()
	e.submit(cand("a", notice))
	deadline := time.Now().Add(5 * time.Second)
	for len(e.del.messages()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("Run never ticked")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestOutbox_SubmitErrorIsTheGovernors(t *testing.T) {
	e := newObEnv(t)
	bad := cand("", notice)
	if _, err := e.ob.Submit(context.Background(), Item{Candidate: bad}); err == nil {
		t.Fatal("candidate without a dedup key accepted")
	}
}

func TestOutbox_TickErrorIsReported(t *testing.T) {
	e := newObEnv(t)
	e.submit(cand("a", notice))
	e.clock.Add(time.Minute)
	if err := e.db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := e.ob.Tick(context.Background()); err == nil {
		t.Fatal("Tick on a closed database reported nothing")
	}
	e.ob.warnTick(errors.New("x")) // logged once, rate-limited
	e.ob.warnTick(errors.New("y"))
}

func TestOutbox_ShadowAndOffNeverDeliver(t *testing.T) {
	for _, mode := range []string{config.ProactiveModeShadow, config.ProactiveModeOff} {
		t.Run(mode, func(t *testing.T) {
			e := newEnvMode(t, mode, digestCfg())
			del := &fakeDeliverer{}
			ob := NewOutbox(e.g, del, time.Minute, e.clock.Now)
			for _, sev := range []proactive.Severity{info, notice, critical} {
				d, err := ob.Submit(context.Background(), Item{Candidate: cand("k"+sev.String(), sev, protective)})
				if err != nil {
					t.Fatal(err)
				}
				if d.Disposition == proactive.DispNotify || d.Disposition == proactive.DispEscalate {
					t.Fatalf("%s mode decided %s", mode, d.Disposition)
				}
			}
			e.clock.Add(48 * time.Hour)
			if err := ob.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			if n := len(del.messages()); n != 0 {
				t.Fatalf("%s mode delivered %d messages", mode, n)
			}
		})
	}
}

func TestOutbox_AcceptanceReplay_DigestHoldsTheRest(t *testing.T) {
	// 15 candidates, 5 per minute, active hours, budget 3. A 20 s window makes
	// each minute two groups, so grouping alone cannot absorb everything: the
	// third message exhausts the budget and the rest waits for the digest.
	cfg := digestCfg()
	cfg.GroupWindowSeconds = 20
	e := newObEnvCfg(t, cfg)
	runReplay(t, e, 15)

	msgs := e.del.messages()
	if len(msgs) != 3 {
		t.Fatalf("%d messages during the replay, want exactly the budget of 3", len(msgs))
	}
	for _, m := range msgs {
		if m.Digest {
			t.Fatal("digest sent during the replay")
		}
	}
	sent := 0
	for _, m := range msgs {
		sent += len(m.Decisions)
	}
	queued := e.count(`SELECT COUNT(*) FROM attention_digest_queue WHERE flushed_ms IS NULL`)
	if sent+queued != 15 || queued == 0 {
		t.Fatalf("sent %d + queued %d != 15 (or nothing queued)", sent, queued)
	}
	if n := e.count(`SELECT COUNT(*) FROM attention_decisions WHERE reason = ?`, ReasonBudget); n != queued {
		t.Fatalf("budget decisions = %d, queued = %d", n, queued)
	}

	// the next digest time: one digest, with exactly the rest, silently.
	e.tickAt(bkkAt(1, 7, 59))
	if n := len(e.del.messages()); n != 3 {
		t.Fatalf("digest before its time: %d messages", n)
	}
	e.tickAt(bkkAt(1, 8, 0))
	msgs = e.del.messages()
	if len(msgs) != 4 {
		t.Fatalf("%d messages after the digest time, want 4", len(msgs))
	}
	dg := msgs[3]
	if !dg.Digest || !dg.Silent || dg.Severity != info || len(dg.Decisions) != queued {
		t.Fatalf("digest = %+v", dg)
	}
	e.tickAt(bkkAt(1, 8, 1))
	e.tickAt(bkkAt(1, 20, 0))
	if n := len(e.del.messages()); n != 4 {
		t.Fatalf("a second digest was sent: %d messages", n)
	}

	// row states: every decision of the replay is accounted for.
	if n := e.count(`SELECT COUNT(*) FROM attention_decisions WHERE disposition = 'notify' AND delivered = 1`); n != sent {
		t.Fatalf("delivered notify rows = %d, want %d", n, sent)
	}
	if n := e.count(`SELECT COUNT(DISTINCT intervention_id) FROM attention_decisions WHERE disposition = 'notify' AND delivered = 1`); n != 3 {
		t.Fatalf("distinct interventions = %d, want 3", n)
	}
	if n := e.count(`SELECT COUNT(*) FROM attention_decisions WHERE disposition = 'digest' AND delivered = 1 AND intervention_id = ?`, dg.InterventionID); n != queued {
		t.Fatalf("digest rows delivered in the digest message = %d, want %d", n, queued)
	}
	if n := e.count(`SELECT COUNT(*) FROM attention_digest_queue WHERE flushed_ms IS NULL`); n != 0 {
		t.Fatalf("%d items left in the queue", n)
	}
	if n := e.count(`SELECT COUNT(*) FROM attention_decisions WHERE delivered = 0 AND disposition IN ('notify','escalate')`); n != 0 {
		t.Fatalf("%d notify rows never reported", n)
	}
}

func TestOutbox_AcceptanceReplay_DefaultWindowFitsInThreeMessages(t *testing.T) {
	// With the default 60 s window the same 15 candidates are three groups of
	// five: three messages, which is within the budget, and an empty digest.
	e := newObEnv(t)
	runReplay(t, e, 15)
	msgs := e.del.messages()
	if len(msgs) != 3 {
		t.Fatalf("%d messages, want 3", len(msgs))
	}
	for _, m := range msgs {
		if len(m.Decisions) != 5 {
			t.Fatalf("message has %d decisions, want 5", len(m.Decisions))
		}
	}
	e.tickAt(bkkAt(1, 8, 0))
	if n := len(e.del.messages()); n != 3 {
		t.Fatalf("an empty digest sent a message: %d", n)
	}
}

// runReplay feeds n distinct candidates, five per minute (every 12 s), starting
// at 12:00 Bangkok, ticking once a second of simulated time, and stops a minute
// after the last one. The digest is primed first so that it cannot fire at 12:00.
func runReplay(t *testing.T, e *obEnv, n int) {
	t.Helper()
	e.tick() // the first tick of the day finds an empty queue and marks it done
	start := e.clock.Now()
	last := (n-1)*12 + 90 // seconds: the last candidate, then long enough for any window
	for s := 0; s <= last; s++ {
		e.clock.Set(start.Add(time.Duration(s) * time.Second))
		if s%12 == 0 && s/12 < n {
			e.submit(cand("c"+strconv.Itoa(s/12), notice))
		}
		e.tick()
	}
}

// manualTimers is a timer clock whose After channel the test fires by hand.
type manualTimers struct{ fire chan time.Time }

func (m *manualTimers) After(time.Duration) <-chan time.Time { return m.fire }

func TestOutbox_TicksOnInjectedClock(t *testing.T) {
	e := newObEnv(t)
	tc := &manualTimers{fire: make(chan time.Time)}
	e.ob.SetTimerClock(tc)
	e.ob.SetTickInterval(5 * time.Millisecond) // a wall ticker would flush well inside the wait below
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.ob.Run(ctx)
	}()
	defer func() {
		cancel()
		<-done
	}()

	start := e.clock.Now()
	e.submit(cand("a", notice))
	e.clock.Set(start.Add(2 * time.Minute)) // the group window has closed
	time.Sleep(300 * time.Millisecond)
	if n := len(e.del.messages()); n != 0 {
		t.Fatalf("%d messages with no timer fired: wall time ticked the outbox", n)
	}
	tc.fire <- e.clock.Now()
	deadline := time.Now().Add(5 * time.Second)
	for len(e.del.messages()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("firing the injected timer did not flush the group")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
