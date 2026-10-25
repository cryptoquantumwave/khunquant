package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/attention"
	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/channels"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/notify"
	"github.com/cryptoquantumwave/khunquant/pkg/notify/notifytest"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/perceptiontest"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex/sqlitextest"
)

// candWM is a world model that reports the candidate an event carries as its
// diff, and candEval an evaluator that turns that diff into the candidate. They
// let a test script exactly which candidate each event produces, standing in for
// MR-207 and MR-208.
type candWM struct{}

func (candWM) Apply(_ context.Context, ev events.Event) (Diff, error) { return ev.Attrs["cand"], nil }
func (candWM) View() View                                             { return nil }

type candEval struct{}

func (candEval) Evaluate(_ context.Context, _ View, d Diff) ([]proactive.Candidate, error) {
	c, ok := d.(proactive.Candidate)
	if !ok {
		return nil, nil
	}
	return []proactive.Candidate{c}, nil
}

func candEvent(c proactive.Candidate) events.Event {
	return events.Event{
		Kind:   events.KindPerceptionMarketTick,
		Source: events.Source{Component: "perception", Name: "test"},
		Attrs:  map[string]any{"subject": c.DedupKey, "cand": c},
		Trust:  events.TrustPtr(proactive.TrustExchange),
	}
}

func protective(key string) proactive.Candidate {
	c := candidate(key)
	c.Severity, c.Protective = proactive.SeverityCritical, true
	return c
}

// pipeline is an engine feeding a real governor and outbox, with a recording
// deliverer, all on one fake clock.
type pipeline struct {
	g    *govEnv
	ob   *attention.Outbox
	del  *fakeDeliverer
	eng  *started
	disp *DefaultDispatcher
}

func newPipeline(t *testing.T, mode string, comp attention.Composer, deliver func(context.Context, attention.Message) (bool, error)) *pipeline {
	t.Helper()
	g := newGov(t, mode, liveCfg())
	del := &fakeDeliverer{fn: deliver}
	ob := attention.NewOutbox(g.gov, del, 0, g.clock.Now) // no grouping: out on the next tick
	ob.Composer = comp
	runOutbox(t, ob)
	disp := NewDefaultDispatcher(ob)
	eng := start(t, Options{
		WorldModel: candWM{}, Evaluator: candEval{}, Attention: ob, Dispatcher: disp, Clock: g.clock,
	})
	return &pipeline{g: g, ob: ob, del: del, eng: eng, disp: disp}
}

func TestEngine_EscalationFailureFallsBackToTemplate(t *testing.T) {
	failures := map[string]*fakeRunner{
		"error":              {err: errors.New("provider down")},
		"timeout":            {err: fmt.Errorf("proactive turn exceeded its deadline: %w", context.DeadlineExceeded)},
		"contract violation": {res: TurnResult{ContractViolated: true, Decisions: []proactive.Decision{{Kind: proactive.DecideSilent}}}},
		"silent":             {res: TurnResult{Decisions: []proactive.Decision{{Kind: proactive.DecideSilent}}}},
	}
	for name, run := range failures {
		t.Run(name, func(t *testing.T) {
			comp := NewComposer(run)
			p := newPipeline(t, config.ProactiveModeLive, comp, nil)
			c := candidate("esc")
			c.Severity, c.Compose = proactive.SeverityWarning, proactive.ComposeLLM
			p.eng.Emitter().Emit(candEvent(c))

			waitFor(t, "the message", func() bool { return len(p.del.messages()) == 1 })
			m := p.del.messages()[0]
			if m.Title != "title esc" || m.Body != "" || m.Digest {
				t.Fatalf("message = %+v, want the template as rendered", m)
			}
			if len(run.requests()) != 1 {
				t.Fatalf("the model was asked %d times, want once", len(run.requests()))
			}
			waitFor(t, "the row to be marked", func() bool {
				return p.g.count(t, `SELECT COUNT(*) FROM attention_decisions WHERE disposition = 'escalate' AND delivered = 1 AND intervention_id = ?`, m.InterventionID) == 1
			})
		})
	}
}

func TestEngine_EscalationAddsProseWhenTheTurnSucceeds(t *testing.T) {
	run := &fakeRunner{res: notifyText("Funding flipped negative.")}
	p := newPipeline(t, config.ProactiveModeLive, NewComposer(run), nil)
	c := candidate("esc")
	c.Severity, c.Compose = proactive.SeverityWarning, proactive.ComposeLLM
	p.eng.Emitter().Emit(candEvent(c))
	waitFor(t, "the message", func() bool { return len(p.del.messages()) == 1 })
	if m := p.del.messages()[0]; m.Title != "title esc" || m.Body != "Funding flipped negative." {
		t.Fatalf("message = %+v", m)
	}
}

// Nothing is delivered when attention is shadow: its decisions are "log".
func TestEngine_ShadowDeliversNothing(t *testing.T) {
	run := &fakeRunner{res: notifyText("never")}
	p := newPipeline(t, config.ProactiveModeShadow, NewComposer(run), nil)
	for i, c := range []proactive.Candidate{candidate("a"), protective("liq"), func() proactive.Candidate {
		c := candidate("llm")
		c.Compose = proactive.ComposeLLM
		return c
	}()} {
		_ = i
		p.eng.Emitter().Emit(candEvent(c))
	}
	waitFor(t, "three decisions", func() bool { return p.eng.Stats().Decisions == 3 })
	time.Sleep(100 * time.Millisecond) // many outbox ticks
	if n := len(p.del.messages()); n != 0 {
		t.Fatalf("shadow mode delivered %d messages", n)
	}
	if n := p.g.count(t, `SELECT COUNT(*) FROM attention_decisions WHERE disposition = 'log'`); n != 3 {
		t.Fatalf("%d log decisions, want 3", n)
	}
	if st := p.disp.Stats(); st.Ignored != 3 || st.Buffered+st.Queued != 0 {
		t.Fatalf("dispatch stats = %+v", st)
	}
	if len(run.requests()) != 0 {
		t.Fatal("a model turn ran in shadow mode")
	}
}

// blockingComposer never returns until its context ends.
type blockingComposer struct{ calls atomic.Int32 }

func (b *blockingComposer) Compose(ctx context.Context, m attention.Message, _ []attention.Item) (attention.Message, error) {
	b.calls.Add(1)
	<-ctx.Done()
	return m, ctx.Err()
}

// A protective Critical that asks for composition is decided "escalate" by
// attention; it must still go out as the template at once, with the composer
// never invoked.
func TestEngine_ProtectiveCriticalWithComposeLLMIsNotHeldForTheComposer(t *testing.T) {
	comp := &blockingComposer{}
	p := newPipeline(t, config.ProactiveModeLive, comp, nil)
	c := protective("liq:btc")
	c.Compose = proactive.ComposeLLM
	start := time.Now()
	p.eng.Emitter().Emit(candEvent(c))
	waitFor(t, "the protective message", func() bool { return len(p.del.messages()) == 1 })
	if el := time.Since(start); el > time.Second {
		t.Fatalf("delivered after %s, want within the protective-lane bound", el)
	}
	if comp.calls.Load() != 0 {
		t.Fatalf("the composer was invoked %d times for a protective Critical", comp.calls.Load())
	}
	m := p.del.messages()[0]
	if m.Title != "title liq:btc" || m.Severity != proactive.SeverityCritical {
		t.Fatalf("message = %+v", m)
	}
	if p.g.count(t, `SELECT COUNT(*) FROM attention_decisions WHERE disposition = 'escalate' AND reason = ?`, attention.ReasonProtective) != 1 {
		t.Fatal("the decision was not the protective escalate this test is about")
	}
	// the real composer is equally out of it.
	run := &fakeRunner{res: notifyText("x")}
	if _, err := NewComposer(run).Compose(context.Background(), m, []attention.Item{{Candidate: m.Decisions[0].Candidate}}); err != nil || len(run.requests()) != 0 {
		t.Fatalf("composer ran for a protective group: %v", err)
	}
}

// The pipeline goroutine never waits on delivery: a deliverer that hangs does not
// stop events from being processed.
func TestEngine_PipelineIsNotHeldByASlowChannel(t *testing.T) {
	hang := make(chan struct{})
	t.Cleanup(func() { close(hang) })
	p := newPipeline(t, config.ProactiveModeLive, nil, func(ctx context.Context, _ attention.Message) (bool, error) {
		select {
		case <-hang:
		case <-ctx.Done():
		}
		return false, nil
	})
	const n = 60
	for i := 0; i < n; i++ {
		c := candidate(fmt.Sprintf("n%d", i))
		if i%10 == 0 {
			c = protective(fmt.Sprintf("liq%d", i))
		}
		p.eng.Emitter().Emit(candEvent(c))
	}
	waitFor(t, "every event decided while delivery hangs", func() bool { return p.eng.Stats().Decisions == n })
	if st := p.eng.Stats(); st.Decisions != n {
		t.Fatalf("stats = %+v", st)
	}
}

// slowSender is a notify.Sender whose non-protective sends take "10 seconds" of
// fake time, i.e. wait on the fake clock until the test advances it.
type slowSender struct {
	clock   *perceptiontest.Clock
	inner   *notifytest.Sender
	entered atomic.Int32
}

func (s *slowSender) SendAndReport(ctx context.Context, msg bus.OutboundMessage) channels.DeliveryReceipt {
	if msg.Severity < proactive.SeverityCritical {
		s.entered.Add(1)
		select {
		case <-s.clock.After(10 * time.Second):
		case <-ctx.Done():
		}
	}
	return s.inner.SendAndReport(ctx, msg)
}

// notifyDeliverer is the gateway's adapter from the notify service to the
// outbox's Deliverer, in miniature.
type notifyDeliverer struct{ svc *notify.Service }

func (d notifyDeliverer) Deliver(ctx context.Context, m attention.Message) (bool, error) {
	res, err := d.svc.Deliver(ctx, notify.Notification{
		Severity: m.Severity, Title: m.Title, Body: m.Body, DedupKey: m.DedupKey,
		InterventionID: m.InterventionID, DisableNotification: m.Silent,
	})
	if err != nil {
		return false, err
	}
	if !res.Delivered {
		return false, fmt.Errorf("not delivered: %s", res.Reason)
	}
	return true, nil
}

// TestProtectiveDeliveredWhileOwnerSessionLocked: the owner's session lock is held
// by a blocked user turn (the appender never returns), a slow channel holds an
// ordinary message for 10 seconds of fake time, and a protective Critical still
// reaches the channel within a second.
func TestProtectiveDeliveredWhileOwnerSessionLocked(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Proactive.Enabled = true
	cfg.Proactive.Owner = config.OwnerConfig{Channel: "telegram", ChatID: "424242", SenderID: "telegram:424242"}

	g := newGov(t, config.ProactiveModeLive, liveCfg())
	sender := &slowSender{clock: g.clock, inner: &notifytest.Sender{}}
	lock := make(chan struct{}) // the session lock: held until the test ends
	appender := &notifytest.Appender{Block: lock}
	svc, err := notify.New(cfg, sender, appender, sqlitextest.Open(t), "main", g.clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		close(lock)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = svc.Close(ctx)
	})

	ob := attention.NewOutbox(g.gov, notifyDeliverer{svc: svc}, 0, g.clock.Now)
	runOutbox(t, ob)
	eng := start(t, Options{
		WorldModel: candWM{}, Evaluator: candEval{}, Attention: ob, Dispatcher: NewDefaultDispatcher(ob), Clock: g.clock,
	})
	// release the slow channel before the outbox stops, or its shutdown waits
	// for the delivery timeout.
	t.Cleanup(func() { g.clock.Advance(10 * time.Second) })

	eng.Emitter().Emit(candEvent(candidate("ordinary")))
	waitFor(t, "the slow channel to take the ordinary message", func() bool { return sender.entered.Load() == 1 })

	began := time.Now()
	eng.Emitter().Emit(candEvent(protective("liq:btc")))
	waitFor(t, "the protective message on the channel", func() bool { return len(sender.inner.Sent()) == 1 })
	if el := time.Since(began); el > time.Second {
		t.Fatalf("the protective Critical took %s while the session was locked and the channel slow", el)
	}
	sent := sender.inner.Sent()
	if sent[0].Severity != proactive.SeverityCritical || sent[0].ChatID != "424242" {
		t.Fatalf("sent %+v", sent[0])
	}
	if n := len(appender.Appends()); n != 0 {
		t.Fatalf("%d session writes landed although the session lock is held", n)
	}

	// 10 seconds of fake time later the ordinary message goes out too.
	g.clock.Advance(10 * time.Second)
	waitFor(t, "the ordinary message", func() bool { return len(sender.inner.Sent()) == 2 })
}

var _ sync.Locker = (*sync.Mutex)(nil)
