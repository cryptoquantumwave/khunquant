package attention

import (
	"context"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

func repeatCfg(minutes int) config.AttentionConfig {
	c := liveCfg()
	c.CriticalRepeatMinutes = minutes
	return c
}

// A protective Critical that persists is delivered again every CriticalRepeatMinutes:
// not on every tick, and not once per (hours-long) dedup window.
func TestProtectiveCritical_RepeatsEveryRepeatIntervalNotEveryTick(t *testing.T) {
	e := newEnvMode(t, config.ProactiveModeLive, repeatCfg(30))
	crit := func(c *proactive.Candidate) { c.Protective, c.DedupWindow = true, 24*time.Hour }
	start := e.clock.Now()

	var sentAt []time.Duration
	for tick := 0; tick <= 24; tick++ { // two hours of 5-minute ticks
		e.clock.Set(start.Add(time.Duration(tick) * 5 * time.Minute))
		d := e.send(cand("dn_health:7:margin_critical", critical, crit))
		switch d.Disposition {
		case proactive.DispNotify:
			sentAt = append(sentAt, e.clock.Now().Sub(start))
		default:
			expect(t, d, proactive.DispDrop, ReasonDedupSame)
		}
	}
	want := []time.Duration{0, 30 * time.Minute, 60 * time.Minute, 90 * time.Minute, 120 * time.Minute}
	if len(sentAt) != len(want) {
		t.Fatalf("sent at %v, want %v", sentAt, want)
	}
	for i := range want {
		if sentAt[i] != want[i] {
			t.Fatalf("sent at %v, want %v", sentAt, want)
		}
	}
}

// Just inside the interval is held back; the interval itself is not.
func TestProtectiveCritical_RepeatWindowEdges(t *testing.T) {
	e := newEnvMode(t, config.ProactiveModeLive, repeatCfg(30))
	crit := func(c *proactive.Candidate) { c.Protective = true }
	e.send(cand("k", critical, crit))
	e.clock.Add(30*time.Minute - time.Second)
	expect(t, e.decide(cand("k", critical, crit)), proactive.DispDrop, ReasonDedupSame)
	e.clock.Add(time.Second)
	expect(t, e.decide(cand("k", critical, crit)), proactive.DispNotify, ReasonProtective)
}

// Everything else keeps its own window: an ordinary candidate, and a Critical that is
// not protective.
func TestRepeatInterval_DoesNotChangeOrdinaryDedup(t *testing.T) {
	e := newEnvMode(t, config.ProactiveModeLive, repeatCfg(30))
	hour := func(c *proactive.Candidate) { c.DedupWindow = time.Hour }
	e.send(cand("ordinary", warning, hour))
	e.send(cand("critical-not-protective", critical, hour))
	e.clock.Add(31 * time.Minute)
	expect(t, e.decide(cand("ordinary", warning, hour)), proactive.DispDrop, ReasonDedupSame)
	expect(t, e.decide(cand("critical-not-protective", critical, hour)), proactive.DispDrop, ReasonDedupSame)
	e.clock.Add(30 * time.Minute)
	if d := e.decide(cand("ordinary", warning, hour)); d.Disposition == proactive.DispDrop {
		t.Fatalf("an ordinary candidate stays deduped past its own window: %s/%s", d.Disposition, d.Reason)
	}
}

// With the setting at 0 a protective Critical keeps its own window, as before.
func TestProtectiveCritical_ZeroRepeatKeepsTheCandidatesWindow(t *testing.T) {
	e := newEnvMode(t, config.ProactiveModeLive, repeatCfg(0))
	crit := func(c *proactive.Candidate) { c.Protective, c.DedupWindow = true, 3*time.Hour }
	e.send(cand("k", critical, crit))
	e.clock.Add(2 * time.Hour)
	expect(t, e.decide(cand("k", critical, crit)), proactive.DispDrop, ReasonDedupSame)
	e.clock.Add(time.Hour)
	expect(t, e.decide(cand("k", critical, crit)), proactive.DispNotify, ReasonProtective)
}

// A different protective Critical is not held back by another's repeat window, and a
// delivery that failed does not start one.
func TestProtectiveCritical_RepeatIsPerKeyAndOnlyAfterDelivery(t *testing.T) {
	e := newEnvMode(t, config.ProactiveModeLive, repeatCfg(30))
	crit := func(c *proactive.Candidate) { c.Protective = true }
	d := e.decide(cand("a", critical, crit))
	e.mark(d, false) // the send failed
	expect(t, e.decide(cand("a", critical, crit)), proactive.DispNotify, ReasonProtective)
	e.send(cand("b", critical, crit))
	expect(t, e.decide(cand("c", critical, crit)), proactive.DispNotify, ReasonProtective)
}

// Property: over a simulated day of irregular ticks, a persisting protective Critical
// reaches the owner at least CriticalRepeatMinutes apart, and is sent whenever that
// long has passed since the last send (quiet hours do not matter to it).
func TestProtectiveCritical_RepeatProperty(t *testing.T) {
	const repeat = 30 * time.Minute
	e := newEnvMode(t, config.ProactiveModeLive, repeatCfg(30))
	rng := rand.New(rand.NewSource(210))
	crit := func(c *proactive.Candidate) { c.Protective, c.DedupWindow = true, 6*time.Hour }

	var last time.Time
	sent := 0
	for i := 0; i < 600; i++ {
		e.clock.Add(time.Duration(1+rng.Intn(9)) * time.Minute)
		now := e.clock.Now()
		d := e.send(cand("dn_health:1:margin_critical", critical, crit))
		sends := d.Disposition == proactive.DispNotify
		due := last.IsZero() || now.Sub(last) >= repeat
		if sends != due {
			t.Fatalf("tick %d at %s: sent=%v, due=%v (last send %s)", i, now.In(bkk).Format("15:04"), sends, due, last.In(bkk).Format("15:04"))
		}
		if sends {
			last = now
			sent++
		}
	}
	if sent < 20 {
		t.Fatalf("only %d sends: the run is too weak", sent)
	}
}

// ---- the observer ------------------------------------------------------------

type recObserver struct {
	mu   sync.Mutex
	outs []Outcome
	ch   chan struct{}
}

func newRecObserver() *recObserver { return &recObserver{ch: make(chan struct{}, 64)} }

func (r *recObserver) observe(o Outcome) {
	r.mu.Lock()
	r.outs = append(r.outs, o)
	r.mu.Unlock()
	r.ch <- struct{}{}
}

func (r *recObserver) waitFor(t *testing.T, n int) []Outcome {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-r.ch:
		case <-time.After(5 * time.Second):
			t.Fatalf("observer saw %d of %d outcomes", i, n)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Outcome(nil), r.outs...)
}

func TestObserver_ReportsEachMessageAfterTheBookkeeping(t *testing.T) {
	e := newObEnv(t)
	var rowDelivered atomic.Int32
	var id atomic.Value
	obs := newRecObserver()
	e.ob.SetObserver(func(o Outcome) {
		// By the time the observer is told, the decision is already recorded.
		if len(o.Message.Decisions) == 1 {
			rowDelivered.Store(int32(e.rowOf(o.Message.Decisions[0].ID).delivered))
			id.Store(o.Message.Decisions[0].ID)
		}
		obs.observe(o)
	})

	d := e.submit(cand("k", critical, protective))
	outs := obs.waitFor(t, 1)
	if !outs[0].Delivered || outs[0].Composed || outs[0].Message.Decisions[0].ID != d.ID || outs[0].At.IsZero() {
		t.Fatalf("outcome = %+v", outs[0])
	}
	if rowDelivered.Load() != 1 {
		t.Fatalf("the decision was not yet marked delivered when the observer ran")
	}

	// A failed delivery is reported too, as not delivered.
	e.del.setFn(failing)
	e.submit(cand("k2", critical, protective))
	outs = obs.waitFor(t, 1)
	if outs[1].Delivered {
		t.Fatalf("a failed delivery was reported as delivered: %+v", outs[1])
	}
}

func TestObserver_ComposedOnlyWhenTheComposerWrote_AndNeverForProtective(t *testing.T) {
	e := newObEnv(t)
	comp := &stubComposer{out: Message{Title: "composed", Body: "prose"}}
	e.ob.Composer = comp
	obs := newRecObserver()
	e.ob.SetObserver(obs.observe)

	e.submit(cand("llm", warning, composeLLM))
	e.tickAt(e.clock.Now().Add(2 * time.Minute))
	outs := obs.waitFor(t, 1)
	if !outs[0].Composed || comp.calls != 1 || outs[0].Message.Title != "composed" {
		t.Fatalf("composed outcome = %+v (calls %d)", outs[0], comp.calls)
	}

	// A protective Critical that asks for composition is sent as the template, once,
	// and the model is never woken.
	e.submit(cand("liq", critical, protective, composeLLM))
	outs = obs.waitFor(t, 1)
	if outs[1].Composed || comp.calls != 1 || outs[1].Message.Title != "title liq" {
		t.Fatalf("protective outcome = %+v (composer calls %d)", outs[1], comp.calls)
	}
}

func TestObserver_PanicDoesNotBreakDelivery(t *testing.T) {
	e := newObEnv(t)
	var calls atomic.Int32
	e.ob.SetObserver(func(Outcome) { calls.Add(1); panic("audit exploded") })

	for _, k := range []string{"a", "b", "c"} {
		d := e.submit(cand(k, critical, protective))
		if d.Disposition != proactive.DispNotify {
			t.Fatalf("%s: %s", k, d.Disposition)
		}
		if r := e.rowOf(d.ID); r.delivered != 1 {
			t.Fatalf("%s was not recorded as delivered", k)
		}
	}
	if n := len(e.del.messages()); n != 3 {
		t.Fatalf("%d messages delivered, want 3", n)
	}
	deadline := time.Now().Add(5 * time.Second)
	for e.ob.Stats().Panics < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := e.ob.Stats().Panics; got != 3 || calls.Load() != 3 {
		t.Fatalf("panics counted %d, observer calls %d, want 3 and 3", got, calls.Load())
	}
}

func TestObserver_ASlowObserverDoesNotBlockDelivery_AndTooManyAreDropped(t *testing.T) {
	e := newObEnv(t)
	release := make(chan struct{})
	var started atomic.Int32
	e.ob.SetObserver(func(Outcome) { started.Add(1); <-release })

	total := maxObserverInflight + 4
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < total; i++ {
			e.submit(cand("k"+string(rune('a'+i)), critical, protective))
		}
	}()
	select {
	case <-done: // every message delivered while the observer is stuck
	case <-time.After(5 * time.Second):
		t.Fatal("delivery waited for the observer")
	}
	if n := len(e.del.messages()); n != total {
		t.Fatalf("%d of %d messages delivered", n, total)
	}
	if got := e.ob.Stats().ObserverDropped; got != 4 {
		t.Fatalf("dropped %d outcomes, want 4", got)
	}
	close(release)
	e.ob.stopObservers() // returns once the stuck calls have finished
	if started.Load() != maxObserverInflight {
		t.Fatalf("%d observer calls ran, want %d", started.Load(), maxObserverInflight)
	}

	// After the stop nothing is reported, and nothing panics.
	e.ob.SetObserver(func(Outcome) { t.Error("observer called after stop") })
	e.submit(cand("late", critical, protective))
}

func TestObserver_NilObserverAndDigestOutcome(t *testing.T) {
	e := newObEnv(t)
	e.submit(cand("nobody-listens", critical, protective)) // no observer: nothing to do

	obs := newRecObserver()
	e.ob.SetObserver(obs.observe)
	// A digest message is reported with Digest set and no composition.
	e.ob.observe(Outcome{Message: Message{Digest: true}, Delivered: true})
	outs := obs.waitFor(t, 1)
	if !outs[0].Message.Digest || outs[0].Composed {
		t.Fatalf("outcome = %+v", outs[0])
	}
	_ = context.Background()
}
