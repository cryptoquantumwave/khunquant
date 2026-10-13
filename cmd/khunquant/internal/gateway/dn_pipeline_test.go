package gateway

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/attention"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral/planhealth"
	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/perceptiontest"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/sources"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive/engine"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex/sqlitextest"
	"github.com/cryptoquantumwave/khunquant/pkg/triggers"
	"github.com/cryptoquantumwave/khunquant/pkg/worldmodel"
)

// recordingDeliverer is the owner's phone: it keeps every message.
type recordingDeliverer struct {
	mu   sync.Mutex
	msgs []attention.Message
}

func (r *recordingDeliverer) Deliver(_ context.Context, m attention.Message) (bool, error) {
	r.mu.Lock()
	r.msgs = append(r.msgs, m)
	r.mu.Unlock()
	return true, nil
}

func (r *recordingDeliverer) messages() []attention.Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]attention.Message(nil), r.msgs...)
}

// countingComposer stands for the language model: it counts how often it is woken.
type countingComposer struct{ calls atomic.Int64 }

func (c *countingComposer) Compose(_ context.Context, m attention.Message, _ []attention.Item) (attention.Message, error) {
	c.calls.Add(1)
	m.Body += "\n(explained)"
	return m, nil
}

// dnPipeline is the real chain, source event to fake phone: world model, the dn_health
// trigger in a real registry, the real governor over a database, the real outbox with
// its delivery lanes and the real engine, all on one fake clock.
type dnPipeline struct {
	t        *testing.T
	clk      *perceptiontest.Clock
	eng      *engine.Engine
	ob       *attention.Outbox
	phone    *recordingDeliverer
	composer *countingComposer
	outcomes atomic.Int64
	sent     uint64
	start    time.Time
}

func newDNPipeline(t *testing.T, params triggers.DNHealthParams) *dnPipeline {
	t.Helper()
	p := &dnPipeline{t: t, phone: &recordingDeliverer{}, composer: &countingComposer{}}
	// 12:00 in Bangkok, the owner's active hours.
	p.start = time.Date(2026, 3, 10, 12, 0, 0, 0, time.FixedZone("ICT", 7*3600))
	p.clk = perceptiontest.NewClock(p.start)

	db := sqlitextest.Open(t)
	ctx := context.Background()
	store, err := worldmodel.NewStore(ctx, nil, p.clk)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := triggers.NewRegistry(ctx, db, func() string { return triggers.ModeLive },
		map[string]triggers.Factory{triggers.KindDNHealth: triggers.DNHealthFactory(triggers.DNHealthConfig{})},
		triggers.WithClock(p.clk.Now))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Add(ctx, triggers.Spec{
		ID: "dn1", Kind: triggers.KindDNHealth, Mode: triggers.ModeLive, Origin: "test", Params: params.JSON(),
	}); err != nil {
		t.Fatal(err)
	}

	profiles, err := intent.NewStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	// The shipped defaults, so that the 30-minute repeat is the configured one.
	acfg := config.DefaultConfig().Proactive.Attention
	acfg.Mode = config.ProactiveModeLive
	gov, err := attention.New(db, profiles, acfg, config.ProactiveModeLive, p.clk.Now)
	if err != nil {
		t.Fatal(err)
	}
	p.ob = attention.NewOutbox(gov, p.phone, time.Duration(acfg.GroupWindowSeconds)*time.Second, p.clk.Now)
	p.ob.Composer = p.composer
	p.ob.SetObserver(func(attention.Outcome) { p.outcomes.Add(1) })

	runCtx, stop := context.WithCancel(ctx)
	runDone := make(chan struct{})
	go func() { defer close(runDone); p.ob.Run(runCtx) }()

	p.eng = engine.New(engine.Options{
		WorldModel: worldModelStage{s: store}, Evaluator: triggerStage{r: reg},
		Attention: p.ob, Dispatcher: engine.NewDefaultDispatcher(p.ob), Clock: p.clk,
	})
	engCtx, engStop := context.WithCancel(ctx)
	engDone := make(chan struct{})
	go func() { defer close(engDone); _ = p.eng.Run(engCtx) }()
	t.Cleanup(func() {
		engStop()
		<-engDone
		stop()
		<-runDone
		_ = store.Close(ctx)
	})
	return p
}

// report sends the plan.health event of one check, as the plan-health source emits it,
// at the clock's time.
func (p *dnPipeline) report(h planhealth.Health) {
	p.t.Helper()
	now := p.clk.Now()
	p.sent++
	p.eng.Emitter().Emit(events.Event{
		Kind: events.KindPerceptionPlanHealth, Time: now, Source: events.Source{Component: "perception", Name: sources.DNPlansName},
		Payload: sources.PlanHealthPayload{Kind: planhealth.Kind, ID: 7, Status: "active", Health: h.Encode(), At: now},
		Attrs:   map[string]any{"subject": "dn/7"},
		Trust:   events.TrustPtr(proactive.TrustExchange),
	})
	waitUntil(p.t, "the report", func() bool { return p.eng.Stats().Processed == p.sent })
}

// settle waits until what was sent has been delivered and recorded.
func (p *dnPipeline) settle(want int) {
	p.t.Helper()
	waitUntil(p.t, "the deliveries", func() bool {
		return len(p.phone.messages()) >= want && p.outcomes.Load() >= int64(want)
	})
}

func marginCritical() planhealth.Health {
	return planhealth.Health{
		Score: 31, BreachCodes: []string{planhealth.CodeMarginCritical, planhealth.CodeLiquidationDistanceLow},
		LiquidationDistancePct: 4.2, MarginRatioPct: 93, DeltaDriftPct: 0.4, FundingAPYPct: 11,
		IntervalSeconds: 300, CooldownSeconds: 3600, DataOK: true,
	}
}

// A margin-critical plan checked every five minutes for two hours: the owner is
// messaged at most five times (it was 48 with the cron job, which woke the model each
// time), the model is never woken, and each message is a complete template.
func TestDNTrigger_SingleMessagePerBreach(t *testing.T) {
	p := newDNPipeline(t, triggers.DNHealthParams{PlanID: 7, Symbol: "CHZ/USDT:USDT", Name: "hedge"})
	delivered := 0
	for minute := 0; minute <= 120; minute += 5 {
		p.report(marginCritical())
		if minute%30 == 0 {
			delivered++
			p.settle(delivered)
		}
		if n := len(p.phone.messages()); n > delivered {
			t.Fatalf("minute %d: %d messages, %d expected so far", minute, n, delivered)
		}
		if minute < 120 {
			p.clk.Advance(5 * time.Minute)
		}
	}
	time.Sleep(50 * time.Millisecond) // nothing late may arrive

	msgs := p.phone.messages()
	if len(msgs) > 5 {
		t.Fatalf("%d messages for one breach over two hours, want at most 5", len(msgs))
	}
	if len(msgs) != 5 {
		t.Fatalf("%d messages, want 5 (every 30 minutes)", len(msgs))
	}
	if n := p.composer.calls.Load(); n != 0 {
		t.Fatalf("the model was woken %d times for a protective Critical", n)
	}
	for i, m := range msgs {
		// One breach, one message: both codes of the breach are in the one message.
		if len(m.Decisions) != 2 || m.Severity != proactive.SeverityCritical {
			t.Fatalf("message %d: %d decisions, severity %s", i, len(m.Decisions), m.Severity)
		}
		for _, d := range m.Decisions {
			if !d.Candidate.Protective || d.Candidate.Compose != proactive.ComposeTemplate {
				t.Fatalf("message %d: %+v", i, d.Candidate)
			}
		}
		if !strings.Contains(m.Body, "Recommended:") || !strings.Contains(m.Body, "liquidation distance low") || strings.Contains(m.Body, "(explained)") {
			t.Fatalf("message %d is not a complete template:\n%s\n%s", i, m.Title, m.Body)
		}
	}
}

// The repeat of a persisting protective Critical is the configured interval (30
// minutes), not every tick and not once per (hours-long) cooldown.
func TestDNCritical_RepeatEvery30MinNotEveryTick(t *testing.T) {
	p := newDNPipeline(t, triggers.DNHealthParams{PlanID: 7, Symbol: "CHZ/USDT:USDT"})
	var times []time.Duration
	delivered := 0
	for minute := 0; minute <= 120; minute += 5 {
		p.report(marginCritical())
		if minute%30 == 0 {
			delivered++
			p.settle(delivered)
			times = append(times, p.clk.Now().Sub(p.start))
		}
		if minute < 120 {
			p.clk.Advance(5 * time.Minute)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if n := len(p.phone.messages()); n != 5 {
		t.Fatalf("%d messages, want 5", n)
	}
	for i, d := range times {
		if d != time.Duration(i)*30*time.Minute {
			t.Fatalf("message times %v", times)
		}
	}
	// A breach that clears and comes back inside the interval is still one message per
	// interval: the situation is the same.
}

// A Warning is worth an explanation: the model is woken for it (with the delta-neutral
// skill), once per cooldown, and a message with a protective Critical in it is a
// template, never held for the model.
func TestDNTrigger_WarningWakesTheModelOncePerCooldown_CriticalNever(t *testing.T) {
	p := newDNPipeline(t, triggers.DNHealthParams{PlanID: 7, Symbol: "CHZ/USDT:USDT"})
	warn := planhealth.Health{
		Score: 55, BreachCodes: []string{planhealth.CodeDeltaDriftHigh}, DeltaDriftPct: 6, LiquidationDistancePct: 60,
		MarginRatioPct: 20, IntervalSeconds: 300, CooldownSeconds: 3600, DataOK: true,
	}
	// An ordinary message waits out the one-minute grouping window on the outbox's clock.
	// Sent at minute 0 and recorded a minute later, it dedups for an hour from then:
	// the next one is at minute 65.
	delivered := 0
	for minute := 0; minute <= 120; minute += 5 {
		p.report(warn)
		step := 5 * time.Minute
		if minute == 0 || minute == 65 {
			p.clk.Advance(61 * time.Second)
			delivered++
			p.settle(delivered)
			step -= 61 * time.Second
		}
		if minute < 120 {
			p.clk.Advance(step)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if n := len(p.phone.messages()); n != 2 {
		t.Fatalf("%d messages for a Warning over two hours with a one-hour cooldown, want 2", n)
	}
	if n := p.composer.calls.Load(); n != 2 {
		t.Fatalf("the model was woken %d times, want once per message", n)
	}
	var sawSkill bool
	for _, m := range p.phone.messages() {
		for _, d := range m.Decisions {
			sawSkill = sawSkill || (len(d.Candidate.ForcedSkills) == 1 && d.Candidate.ForcedSkills[0] == "delta-neutral")
		}
	}
	if !sawSkill {
		t.Fatal("the Warning did not carry the delta-neutral skill")
	}

	before := p.composer.calls.Load()
	crit := marginCritical()
	crit.BreachCodes = append([]string{planhealth.CodeDeltaDriftHigh}, crit.BreachCodes...)
	p.clk.Advance(2 * time.Hour) // the Warning's cooldown is over too
	p.report(crit)
	p.settle(3)
	last := p.phone.messages()[2]
	if p.composer.calls.Load() != before || last.Severity != proactive.SeverityCritical {
		t.Fatalf("a message with a protective Critical in it woke the model (calls %d -> %d)", before, p.composer.calls.Load())
	}
}

// The payload the plan-health source emits is what the world model decodes: a renamed
// field on either side would blind the trigger without a compile error.
// The owner's silence holds through the real chain: while the report says the codes are
// silenced the phone hears nothing; once the report no longer carries the silence (it
// ended, or clear_silences) the very next report is delivered.
func TestDNTrigger_OwnerSilenceHoldsUntilItEnds(t *testing.T) {
	p := newDNPipeline(t, triggers.DNHealthParams{PlanID: 7, Symbol: "CHZ/USDT:USDT", Name: "hedge"})
	silenced := marginCritical()
	silenced.SilencedCodes = append([]string(nil), silenced.BreachCodes...)
	for i := 0; i < 3; i++ {
		p.report(silenced)
		p.clk.Advance(5 * time.Minute)
	}
	time.Sleep(100 * time.Millisecond)
	if n := len(p.phone.messages()); n != 0 {
		t.Fatalf("%d messages while every code was silenced", n)
	}

	// One of the two codes is released: only that one is reported.
	partly := marginCritical()
	partly.SilencedCodes = []string{planhealth.CodeLiquidationDistanceLow}
	p.report(partly)
	p.settle(1)
	msgs := p.phone.messages()
	if len(msgs) != 1 || len(msgs[0].Decisions) != 1 || msgs[0].Decisions[0].Candidate.DedupKey != "dn_health:7:"+planhealth.CodeMarginCritical {
		t.Fatalf("messages = %+v", msgs)
	}
}

func TestPlanHealthPayload_IsWhatTheWorldModelDecodes(t *testing.T) {
	store, err := worldmodel.NewStore(context.Background(), nil, perception.RealClock{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close(context.Background())
	now := time.Now().UTC()
	h := marginCritical()
	h.SilencedCodes = []string{h.BreachCodes[0]}
	d, err := store.Apply(context.Background(), events.Event{
		Kind: events.KindPerceptionPlanHealth, Time: now, Source: events.Source{Component: "perception", Name: sources.DNPlansName},
		Payload: sources.PlanHealthPayload{Kind: planhealth.Kind, ID: 7, Status: "active", Health: h.Encode(), At: now},
		Trust:   events.TrustPtr(proactive.TrustExchange),
	})
	if err != nil {
		t.Fatal(err)
	}
	pv, ok := store.View().Plan(planhealth.Kind, 7)
	if !ok || pv.Status != "active" || !pv.At.Equal(d.At) {
		t.Fatalf("plan view = %+v (diff at %v)", pv, d.At)
	}
	got, err := planhealth.Decode(pv.Health)
	if err != nil || got.Score != 31 || len(got.BreachCodes) != 2 {
		t.Fatalf("decoded %+v, %v", got, err)
	}
	// The silenced codes survive the world model's bounds on a payload too.
	if len(got.SilencedCodes) != 1 || got.SilencedCodes[0] != planhealth.CodeMarginCritical {
		t.Fatalf("silenced = %v, want %v", got.SilencedCodes, h.SilencedCodes)
	}
}
