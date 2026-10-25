package gateway

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/agent"
	"github.com/cryptoquantumwave/khunquant/pkg/attention"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/perceptiontest"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/sources"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive/engine"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive/sweep"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex/sqlitextest"
	"github.com/cryptoquantumwave/khunquant/pkg/triggers"
	"github.com/cryptoquantumwave/khunquant/pkg/worldmodel"
)

// This file is the volatile-day acceptance test of MR-213: the real world model, the
// real trigger registry with the real portfolio kinds, the real attention governor
// and outbox and the real engine, joined as the gateway joins them, with the sweep on
// top. THE DATA IS SYNTHETIC: a deterministic generator with one constructed crash,
// not a recorded day (recorded replays belong to a later phase).

var sweepBKK = func() *time.Location {
	l, err := time.LoadLocation("Asia/Bangkok")
	if err != nil {
		panic(err)
	}
	return l
}()

// reconcileSource is a portfolio source whose authoritative state is whatever its
// price is now: Reconcile emits a snapshot of it, as the real source's collection
// does.
type reconcileSource struct {
	mu    sync.Mutex
	clock *perceptiontest.Clock
	btc   float64
	n     int
}

func (s *reconcileSource) Name() string                                    { return "portfolio" }
func (s *reconcileSource) Start(context.Context, perception.Emitter) error { return nil }
func (s *reconcileSource) Health() perception.SourceHealth                 { return perception.SourceHealth{} }
func (s *reconcileSource) Reconcile(_ context.Context, out perception.Emitter) error {
	s.mu.Lock()
	btc := s.btc
	s.n++
	n := s.n
	s.mu.Unlock()
	out.Emit(btcSnapshot(s.clock.Now(), btc, 1000+n))
	return nil
}

func (s *reconcileSource) setPrice(p float64) { s.mu.Lock(); s.btc = p; s.mu.Unlock() }

// btcSnapshot is a portfolio of 1 BTC at btc, 10 ETH at 2000 and 20,000 cash.
func btcSnapshot(at time.Time, btc float64, id int) events.Event {
	const eth, cash = 20000.0, 20000.0
	return events.Event{
		ID: fmt.Sprintf("snap-%d", id), Kind: events.KindPerceptionPortfolioSnapshot, Time: at,
		Source: events.Source{Component: "perception", Name: "portfolio"},
		Payload: sources.PortfolioSnapshotPayload{
			SnapshotID: int64(id), TakenAt: at, Quote: "USDT", TotalValueUSD: btc + eth + cash,
			ByClass: map[string]float64{"crypto": btc + eth, "cash": cash},
			Positions: []sources.PortfolioPosition{
				{Asset: "BTC", Source: "binance", Class: "crypto", Value: btc, Qty: 1},
				{Asset: "ETH", Source: "binance", Class: "crypto", Value: eth, Qty: 10},
				{Asset: "USDT", Source: "binance", Class: "cash", Value: cash, Qty: cash},
			},
		},
		Trust: events.TrustPtr(proactive.TrustExchange),
	}
}

type volatileDay struct {
	t      *testing.T
	clk    *perceptiontest.Clock
	store  *worldmodel.Store
	eng    *engine.Engine
	mgr    *perception.Manager
	src    *reconcileSource
	ob     *attention.Outbox
	reg    *triggers.Registry
	start  time.Time
	runner *sweep.Runner
	turn   func(prompt string) []proactive.Decision
	prompt []string
}

// newVolatileDay builds the pipeline and runs a quiet morning: BTC steady at 60,000
// from 06:00 to 12:00, a snapshot every 15 minutes, nothing fires. guard=false makes
// the sweep blind to what the triggers decided, which is the control of the test:
// without the guard the owner is told twice.
func newVolatileDay(t *testing.T, guard bool) *volatileDay {
	t.Helper()
	ctx := context.Background()
	v := &volatileDay{t: t, start: time.Date(2026, 3, 10, 6, 0, 0, 0, sweepBKK)}
	v.clk = perceptiontest.NewClock(v.start)
	db := sqlitextest.Open(t)

	var err error
	v.store, err = worldmodel.NewStore(ctx, db, v.clk, worldmodel.WithZone(func() *time.Location { return sweepBKK }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = v.store.Close(ctx) })

	factories := triggers.PortfolioFactories(triggers.PortfolioConfig{Stats: &triggers.PortfolioStats{}})
	v.reg, err = triggers.NewRegistry(ctx, db, func() string { return triggers.ModeLive }, factories)
	if err != nil {
		t.Fatal(err)
	}
	ensurePortfolioSpecs(ctx, v.reg, config.DefaultConfig(), factories)

	profiles, err := intent.NewStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	acfg := config.AttentionConfig{Mode: config.ProactiveModeLive, DailyBudget: 50, DedupWindowHours: 24, GroupWindowSeconds: 0, Digest: true}
	gov, err := attention.New(db, profiles, acfg, config.ProactiveModeLive, v.clk.Now)
	if err != nil {
		t.Fatal(err)
	}
	v.ob = attention.NewOutbox(gov, discardDeliverer{}, 0, v.clk.Now)

	v.eng = engine.New(engine.Options{
		WorldModel: worldModelStage{s: v.store}, Evaluator: triggerStage{r: v.reg}, Attention: v.ob, Clock: v.clk,
	})
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); _ = v.eng.Run(runCtx) }()
	t.Cleanup(func() { cancel(); <-done })
	waitUntil(t, "the engine", func() bool { return v.eng.Settle(ctx) == nil })

	v.src = &reconcileSource{clock: v.clk, btc: 60000}
	v.mgr = perception.NewManager(v.eng.Emitter(), perception.WithClock(v.clk))
	v.mgr.Add(v.src)

	if err := v.store.EnsureMark(ctx, sweep.Mark); err != nil {
		t.Fatal(err)
	}
	id := 0
	for ; v.clk.Now().Before(v.start.Add(6 * time.Hour)); v.clk.Advance(15 * time.Minute) {
		id++
		v.eng.Emitter().Emit(btcSnapshot(v.clk.Now(), 60000, id))
	}
	if err := v.eng.Settle(ctx); err != nil {
		t.Fatal(err)
	}
	if rows := v.decisions(); len(rows) != 0 {
		t.Fatalf("the quiet morning produced decisions: %+v", rows)
	}

	topics := sweep.TopicSource(sweepTopicSource{log: v.ob, now: v.clk.Now})
	if !guard {
		topics = noTopics{}
	}
	v.prompt = nil
	runner := sweep.New(sweep.Config{
		Reconciler: v.mgr, Settler: v.eng, World: v.store, Topics: topics, Now: v.clk.Now, State: &sweep.State{},
		Turn: func(_ context.Context, prompt string) (sweep.Outcome, error) {
			v.prompt = append(v.prompt, prompt)
			return sweep.Outcome{Decisions: v.turn(prompt)}, nil
		},
		Deliver: func(ctx context.Context, kept []proactive.Decision, _ sweep.Outcome) string {
			return submitHeartbeatDecisions(ctx, v.ob, agent.ProactiveTurnResult{Decisions: kept}, proactive.ModelTierPrimary)
		},
	})
	v.runner = runner
	return v
}

type noTopics struct{}

func (noTopics) TriggerTopics(context.Context, time.Time) ([]sweep.Topic, error) { return nil, nil }

type discardDeliverer struct{}

func (discardDeliverer) Deliver(context.Context, attention.Message) (bool, error) { return true, nil }

func (v *volatileDay) decisions() []attention.RecordedDecision {
	v.t.Helper()
	rows, err := v.ob.DecisionsSince(context.Background(), v.start.Add(-time.Hour), "", 0)
	if err != nil {
		v.t.Fatal(err)
	}
	return rows
}

// volatileDayModel is what a model that follows HEARTBEAT.md says about the crash:
// it raises the BTC move, the drawdown, and one thing the triggers do not cover.
func volatileDayModel(string) []proactive.Decision {
	return []proactive.Decision{
		{
			Kind: proactive.DecideNotify, Severity: proactive.SeverityWarning, DedupKey: "btc:drop",
			Text: "BTC dropped about 10% today\nIt is your largest holding.",
		},
		{
			Kind: proactive.DecideNotify, Severity: proactive.SeverityWarning, DedupKey: "portfolio:drawdown",
			Text: "Your portfolio is down about 6% on the day\nCheck the drawdown.",
		},
		{
			Kind: proactive.DecideNotify, Severity: proactive.SeverityNotice, DedupKey: "eth:funding",
			Text: "ETH perp funding turned sharply negative\nCarrying cost changed.",
		},
	}
}

// split returns the decision rows of the day by who made them.
func (v *volatileDay) split() (trig, hb []attention.RecordedDecision) {
	for _, r := range v.decisions() {
		if r.TriggerID == heartbeatTriggerID {
			hb = append(hb, r)
		} else {
			trig = append(trig, r)
		}
	}
	return trig, hb
}

func told(rows []attention.RecordedDecision) []string {
	var out []string
	for _, r := range rows {
		if r.Disposition == proactive.DispNotify || r.Disposition == proactive.DispEscalate {
			out = append(out, r.DedupKey)
		}
	}
	return out
}

// TestSweep_TriggerCandidatesNotDuplicated is the volatile-day case of the spec: a
// crash makes move_vs_vol and drawdown fire (live, through attention); the sweep then
// runs, sees the same crash in its diff, asks the model, and the model raises the
// same two topics. Neither is raised again; what the triggers do not cover is.
func TestSweep_TriggerCandidatesNotDuplicated(t *testing.T) {
	v := newVolatileDay(t, true)
	v.turn = volatileDayModel
	v.src.setPrice(54000) // BTC -10% in the last quarter of an hour; the next reconcile reads it
	v.clk.Advance(15 * time.Minute)

	res := v.runner.Run(context.Background(), "# Heartbeat Check\n\nthe owner's tasks")
	if res.Outcome != sweep.ResultCompleted || res.Err != nil {
		t.Fatalf("sweep = %+v", res)
	}

	// The reconcile reached the pipeline and the triggers fired on what it read,
	// before the sweep looked: both kinds decided, and the owner is told.
	trig, hb := v.split()
	var kinds []string
	for _, r := range trig {
		kinds = append(kinds, sweep.KindOf(r.DedupKey))
		t.Logf("trigger decision: %s %s %s (%s)", r.DedupKey, r.Severity, r.Disposition, r.Reason)
	}
	for _, r := range hb {
		t.Logf("sweep decision: %s %s %s (%s)", r.DedupKey, r.Severity, r.Disposition, r.Reason)
	}
	for _, want := range []string{triggers.KindMoveVsVol, triggers.KindDrawdown} {
		found := false
		for _, k := range kinds {
			found = found || k == want
		}
		if !found {
			t.Fatalf("%s did not fire on the crash (the volatile day is not volatile): triggers decided %v", want, kinds)
		}
	}
	if len(told(trig)) == 0 {
		t.Fatalf("no trigger told the owner: %+v", trig)
	}

	// The prompt told the model what was handled.
	if len(v.prompt) != 1 || !strings.Contains(v.prompt[0], "Already handled") ||
		!strings.Contains(v.prompt[0], "move_vs_vol:BTC:1h:tier1 (BTC)") || !strings.Contains(v.prompt[0], "drawdown:portfolio:day:tier1") {
		t.Fatalf("the model was not told what the triggers handled:\n%v", v.prompt)
	}

	// The model said it all anyway: the guarantee is in code. Of its three
	// decisions, only the one the triggers do not cover reached attention.
	if len(hb) != 1 || hb[0].DedupKey != "heartbeat:eth:funding" {
		t.Fatalf("the sweep submitted %+v, want only the ETH funding decision", hb)
	}

	// Every material event was delivered once: the crash by the triggers, never again
	// by the sweep; the funding change by the sweep.
	for _, r := range hb {
		k := strings.ToLower(r.DedupKey)
		if strings.Contains(k, "btc") || strings.Contains(k, "portfolio") {
			t.Fatalf("the sweep raised a topic a trigger had decided: %+v", r)
		}
	}

	// The mark moved: the next sweep, with nothing new, makes no model call.
	v.clk.Advance(30 * time.Minute)
	if res := v.runner.Run(context.Background(), "p"); res.Outcome == sweep.ResultCompleted && len(v.prompt) != 1 {
		t.Fatalf("the second sweep asked the model again: %+v", res)
	}
	if len(v.prompt) != 1 {
		t.Fatalf("%d model calls over the two sweeps, want 1", len(v.prompt))
	}
}

// The control: with the sweep blind to the triggers' decisions, the same day tells
// the owner about the crash twice. This is what the guard is for, and what makes the
// test above able to fail.
func TestSweep_WithoutTheGuardTheOwnerIsToldTwice(t *testing.T) {
	v := newVolatileDay(t, false)
	v.turn = volatileDayModel
	v.src.setPrice(54000)
	v.clk.Advance(15 * time.Minute)
	if res := v.runner.Run(context.Background(), "p"); res.Outcome != sweep.ResultCompleted {
		t.Fatalf("sweep = %+v", res)
	}
	_, hb := v.split()
	keys := strings.Join(told(hb), " ")
	if !strings.Contains(keys, "btc:drop") && !strings.Contains(keys, "portfolio:drawdown") {
		t.Fatalf("the control did not duplicate anything (told %q, rows %+v): the test above proves nothing", keys, hb)
	}
}

// A trigger that fires while the model is still working is honoured: the topics are
// read again right before anything is submitted.
func TestSweep_TriggerFiringDuringTheTurn(t *testing.T) {
	v := newVolatileDay(t, true)
	// The reconcile reads nothing special; the crash arrives while the turn runs.
	v.turn = func(string) []proactive.Decision {
		v.clk.Advance(5 * time.Minute)
		v.eng.Emitter().Emit(btcSnapshot(v.clk.Now(), 54000, 9001))
		if err := v.eng.Settle(context.Background()); err != nil {
			t.Error(err)
		}
		return volatileDayModel("")[:1] // the BTC move
	}
	// Something material for the sweep to ask about: a 5% rise in BTC.
	v.src.setPrice(63000)
	v.clk.Advance(15 * time.Minute)
	if res := v.runner.Run(context.Background(), "p"); res.Outcome != sweep.ResultCompleted {
		t.Fatalf("sweep = %+v", res)
	}
	trig, hb := v.split()
	if len(trig) == 0 {
		t.Fatal("the crash did not fire a trigger")
	}
	if len(hb) != 0 {
		t.Fatalf("the sweep raised a BTC move that a trigger decided during its turn: %+v", hb)
	}
}
