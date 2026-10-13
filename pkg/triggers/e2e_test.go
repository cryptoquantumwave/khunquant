package triggers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/attention"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/perceptiontest"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive/engine"
	"github.com/cryptoquantumwave/khunquant/pkg/worldmodel"
)

// The tests in this file run the real engine, the real governor and the real
// outbox on one test database, with the registry in the place the gateway puts it.

// stage is the registry as the engine's Evaluator, as the gateway's adapter makes
// it. dropCommits simulates a crash between attention's decision and the state save.
type stage struct {
	r           *Registry
	dropCommits bool
}

func (s stage) Evaluate(ctx context.Context, v engine.View, d engine.Diff) ([]proactive.Candidate, error) {
	wd, ok1 := d.(worldmodel.Diff)
	wv, ok2 := v.(worldmodel.View)
	if !ok1 || !ok2 {
		return nil, nil
	}
	return s.r.Evaluate(ctx, wv, wd)
}

func (s stage) Committed(ctx context.Context, ids []string) {
	if !s.dropCommits {
		s.r.Committed(ctx, ids)
	}
}

var (
	_ engine.Evaluator = stage{}
	_ engine.Committer = stage{}
)

// fakeModel boxes the diff an event carries, as the gateway's world model stage does.
type fakeModel struct{ now time.Time }

func (fakeModel) Apply(_ context.Context, ev events.Event) (engine.Diff, error) {
	return worldmodel.Diff{Event: ev, At: ev.Time}, nil
}
func (m fakeModel) View() engine.View { return fakeView{now: m.now} }

type staticProfile struct{}

func (staticProfile) Profile(context.Context) (intent.Profile, error) {
	return intent.DefaultProfile(), nil
}

type deliverer struct {
	mu   sync.Mutex
	msgs []attention.Message
}

func (d *deliverer) Deliver(_ context.Context, m attention.Message) (bool, error) {
	d.mu.Lock()
	d.msgs = append(d.msgs, m)
	d.mu.Unlock()
	return true, nil
}

func (d *deliverer) messages() []attention.Message {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]attention.Message(nil), d.msgs...)
}

// pipe is a governor and outbox over the env's database, and a way to run engines
// over them.
type pipe struct {
	e     *env
	gov   *attention.Governor
	ob    *attention.Outbox
	del   *deliverer
	clock *perceptiontest.Clock
}

func newPipe(t *testing.T, e *env, mode string) *pipe {
	t.Helper()
	clock := perceptiontest.NewClock(noon)
	cfg := config.AttentionConfig{Mode: mode, DailyBudget: 3, DedupWindowHours: 24}
	gov, err := attention.New(e.db, staticProfile{}, cfg, mode, clock.Now)
	if err != nil {
		t.Fatalf("attention.New: %v", err)
	}
	del := &deliverer{}
	ob := attention.NewOutbox(gov, del, 0, clock.Now)
	ob.SetTickInterval(2 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); ob.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return &pipe{e: e, gov: gov, ob: ob, del: del, clock: clock}
}

// run starts an engine over the pipe with the given registry stage and returns a
// function that emits n market ticks and waits for them to be processed.
func (p *pipe) run(t *testing.T, st stage) (emit func(n int), stop func()) {
	t.Helper()
	eng := engine.New(engine.Options{
		WorldModel: fakeModel{now: noon}, Evaluator: st, Attention: p.ob,
		Dispatcher: engine.NewDefaultDispatcher(p.ob), Clock: p.clock,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- eng.Run(ctx) }()
	var sent uint64
	stopped := false
	stop = func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("engine did not stop")
		}
	}
	t.Cleanup(stop)
	return func(n int) {
		for i := 0; i < n; i++ {
			sent++
			eng.Emitter().Emit(events.Event{
				ID: fmt.Sprintf("tick-%d", sent), Kind: kindTick, Time: noon,
				Source: events.Source{Component: "perception", Name: "test"},
				Trust:  events.TrustPtr(proactive.TrustExchange),
			})
		}
		waitFor(t, "the events to be processed", func() bool { return eng.Stats().Processed == sent })
	}, stop
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// edge fires once, the first time it runs from an empty state, and remembers that it
// did.
func edge(kind string) evalFn {
	return func(_ int, st *State, _ worldmodel.Diff) ([]proactive.Candidate, error) {
		if len(st.Raw) > 0 {
			return nil, nil
		}
		st.Raw, st.Dirty = json.RawMessage(`{"side":"above"}`), true
		return []proactive.Candidate{cand(kind, "above")}, nil
	}
}

func TestShadowCandidatesLoggedNotDelivered(t *testing.T) {
	for _, global := range []string{ModeLive, ModeShadow} {
		t.Run("global "+global, func(t *testing.T) {
			e := newEnv(t, nil)
			e.setGlobal(global)
			// Distinct keys, or the second would be absorbed as a repeat of the first.
			fk := map[string]Factory{
				testKind:  factoryOf(testKind, []events.Kind{kindTick}, fires(testKind), nil),
				otherKind: factoryOf(otherKind, []events.Kind{kindTick}, fires(otherKind), nil),
			}
			e.r = e.registry(fk)
			e.add("live", testKind, ModeLive, `{}`)
			e.add("shadow", otherKind, ModeShadow, `{}`)

			p := newPipe(t, e, ModeLive)
			emit, _ := p.run(t, stage{r: e.r})
			emit(1)

			wantDelivered := 0
			if global == ModeLive {
				wantDelivered = 1
				waitFor(t, "the live spec's message", func() bool { return len(p.del.messages()) == 1 })
			}
			// Everything the pipeline did is recorded: two decisions, and the shadow
			// ones are logged, never notified.
			if n := e.count(`SELECT COUNT(*) FROM attention_decisions`); n != 2 {
				t.Fatalf("%d decisions recorded, want 2", n)
			}
			wantShadowRows := 2 - wantDelivered
			if n := e.count(`SELECT COUNT(*) FROM attention_decisions WHERE disposition = 'log' AND reason = 'shadow'`); n != wantShadowRows {
				t.Fatalf("%d shadow rows, want %d", n, wantShadowRows)
			}
			time.Sleep(20 * time.Millisecond) // a delivery that should not happen would have by now
			msgs := p.del.messages()
			if len(msgs) != wantDelivered {
				t.Fatalf("%d messages delivered, want %d: %+v", len(msgs), wantDelivered, msgs)
			}
			for _, m := range msgs {
				if strings.Contains(m.DedupKey, otherKind) {
					t.Fatalf("a shadow trigger's candidate was delivered: %+v", m)
				}
			}
		})
	}
}

// The acceptance criterion: a trigger that always errors is disabled after ten
// events, and the owner gets exactly one notice.
func TestAcceptance_AlwaysErroringTriggerIsDisabledAfterTenEventsWithOneNotice(t *testing.T) {
	e := newEnv(t, tickFactory(failsWith(errors.New(secretError))))
	e.add("bad", testKind, ModeLive, `{}`)
	p := newPipe(t, e, ModeLive)
	emit, _ := p.run(t, stage{r: e.r})

	emit(10)
	waitFor(t, "the notice", func() bool { return len(p.del.messages()) == 1 })
	if mode, _, _ := e.row("bad"); mode != ModeOff {
		t.Fatalf("mode after ten events = %s, want off", mode)
	}
	emit(10)
	time.Sleep(20 * time.Millisecond)
	msgs := p.del.messages()
	if len(msgs) != 1 {
		t.Fatalf("%d messages, want exactly one: %+v", len(msgs), msgs)
	}
	m := msgs[0]
	if !strings.Contains(m.Title, testKind) || !strings.Contains(m.Title, "bad") || strings.Contains(m.Title+m.Body, secretError) {
		t.Fatalf("message = %+v", m)
	}
	if n := e.r.snap.Load().byID["bad"].trig.(*scripted).calls.Load(); n != 10 {
		t.Fatalf("the trigger ran %d times, want 10", n)
	}
	// And it is not repeated after a restart of the registry and the engine.
	e.r = e.registry(tickFactory(failsWith(errors.New(secretError))))
	emit2, _ := p.run(t, stage{r: e.r})
	emit2(5)
	time.Sleep(20 * time.Millisecond)
	if n := len(p.del.messages()); n != 1 {
		t.Fatalf("%d messages after a restart, want 1", n)
	}
}

// A crash between attention's decision and the state save is safe: the trigger,
// restarted from its old state, fires again, and attention's dedup absorbs the
// repeat. No second message.
func TestCrashBetweenDecisionAndStateSaveSendsNoDuplicate(t *testing.T) {
	e := newEnv(t, tickFactory(edge(testKind)))
	e.add("s1", testKind, ModeLive, `{}`)
	p := newPipe(t, e, ModeLive)

	// Run 1: the decision is recorded and the message goes out, and the process
	// "dies" before the state is saved.
	emit, stop := p.run(t, stage{r: e.r, dropCommits: true})
	emit(1)
	waitFor(t, "the first message", func() bool { return len(p.del.messages()) == 1 })
	waitFor(t, "the delivery to be recorded", func() bool {
		return e.count(`SELECT COUNT(*) FROM attention_decisions WHERE disposition = 'notify' AND delivered = 1`) == 1
	})
	stop()
	if _, st, _ := e.row("s1"); st != "" {
		t.Fatalf("the state was saved without a commit: %q", st)
	}

	// Run 2: a new process. The trigger starts from the old (empty) state and fires
	// again; the governor drops it as a repeat.
	e.r = e.registry(tickFactory(edge(testKind)))
	emit2, stop2 := p.run(t, stage{r: e.r})
	emit2(1)
	if st := e.r.Stats(); st.Candidates != 1 {
		t.Fatalf("the trigger did not re-evaluate from its old state: %+v", st)
	}
	time.Sleep(20 * time.Millisecond)
	if n := len(p.del.messages()); n != 1 {
		t.Fatalf("%d messages, want 1 (no duplicate)", n)
	}
	if n := e.count(`SELECT COUNT(*) FROM attention_decisions WHERE disposition = 'drop'`); n != 1 {
		t.Fatalf("%d dropped decisions, want 1 (the repeat, absorbed)", n)
	}
	// This time the commit was made: the state is saved, and it stays quiet.
	if _, st, _ := e.row("s1"); st != `{"side":"above"}` {
		t.Fatalf("state after a committed run = %q", st)
	}
	stop2()

	// Run 3: restarted again, the trigger knows its side and says nothing.
	e.r = e.registry(tickFactory(edge(testKind)))
	emit3, _ := p.run(t, stage{r: e.r})
	emit3(3)
	if st := e.r.Stats(); st.Candidates != 0 {
		t.Fatalf("a saved state did not stop the repeat: %+v", st)
	}
	if n := len(p.del.messages()); n != 1 {
		t.Fatalf("%d messages, want 1", n)
	}
}
