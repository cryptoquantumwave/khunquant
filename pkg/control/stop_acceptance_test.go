package control_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/agent"
	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/control"
	"github.com/cryptoquantumwave/khunquant/pkg/journal"
	"github.com/cryptoquantumwave/khunquant/pkg/providers"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex/sqlitextest"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

// watchedProvider blocks inside the model call until its context is cancelled and
// then closes cancelled: the 60-second turn of the acceptance test, and the proof
// that the turn really was cancelled.
type watchedProvider struct {
	started   chan struct{}
	cancelled chan struct{}
	once      sync.Once
}

func (p *watchedProvider) Chat(ctx context.Context, _ []providers.Message, _ []providers.ToolDefinition, _ string, _ map[string]any) (*providers.LLMResponse, error) {
	p.once.Do(func() { close(p.started) })
	select {
	case <-ctx.Done():
		close(p.cancelled)
		return nil, ctx.Err()
	case <-time.After(60 * time.Second):
		return &providers.LLMResponse{Content: "done"}, nil
	}
}

func (p *watchedProvider) GetDefaultModel() string { return "watched-model" }

// stampLog wraps a fake so each mutating step is timestamped for the report.
type stamps struct {
	mu    sync.Mutex
	begin time.Time
	at    map[string]time.Duration
}

func (s *stamps) mark(step string) {
	s.mu.Lock()
	if _, ok := s.at[step]; !ok {
		s.at[step] = time.Since(s.begin)
	}
	s.mu.Unlock()
}

type stampedMandates struct {
	*fakeMandates
	s *stamps
}

func (m stampedMandates) RevokeAll(ctx context.Context, r string) (int, error) {
	m.s.mark("4 mandates revoked")
	return m.fakeMandates.RevokeAll(ctx, r)
}

type stampedProposals struct {
	*fakeProposals
	s *stamps
}

func (p stampedProposals) CancelPending(ctx context.Context, r string) ([]approval.Cancelled, error) {
	p.s.mark("4 proposals cancelled")
	return p.fakeProposals.CancelPending(ctx, r)
}

type stampedTurns struct {
	l *agent.AgentLoop
	s *stamps
}

func (t stampedTurns) CancelAllTurns(reason string) int {
	t.s.mark("2 turns cancelled")
	return t.l.CancelAllTurns(reason)
}

type stampedDesk struct {
	*fakeDesk
	s *stamps
}

func (d stampedDesk) Cancel(ctx context.Context, ref broker.OrderRef, id string) error {
	d.s.mark("5 orders cancelled")
	return d.fakeDesk.Cancel(ctx, ref, id)
}

// The acceptance of MR-311: from a state with a running DCA workflow, one waiting,
// a protective one waiting, a pending proposal, an active model turn, an active
// mandate, an open order of the agent and an open order of the owner, /stop reaches
// its summary in under 2 s and leaves no non-protective activity.
//
// The journal engine, the agent loop and the kill flag are the real ones; the
// exchange, the mandates and the proposals are fakes (their real counterparts are
// tested in pkg/approval, pkg/dca and pkg/providers/broker).
func TestStop_Acceptance_NoNonProtectiveActivityAfterwards(t *testing.T) {
	ctx := context.Background()
	db := sqlitextest.Open(t)

	// --- the journal engine, with a DCA-shaped workflow and a protective one.
	var (
		placed   atomic.Int32
		holding  = make(chan struct{})
		holdOnce sync.Once
		release  = make(chan struct{})
	)
	type mode struct{ Mode string }
	defs := []journal.Definition{
		{Kind: "dca.execute", Run: func(c *journal.Ctx, raw json.RawMessage) error {
			var m mode
			_ = json.Unmarshal(raw, &m)
			switch m.Mode {
			case "running":
				if err := c.Step("hold", nil, func(sctx context.Context, _ int) (any, error) {
					holdOnce.Do(func() { close(holding) })
					select { // an in-flight step: not interrupted by a cancel; it ends after one
					case <-release:
					case <-sctx.Done():
					}
					return nil, nil
				}); err != nil {
					return err
				}
			case "waiting":
				if err := c.SleepUntil(c.Now().Add(24 * time.Hour)); err != nil {
					return err
				}
			}
			return c.Step("place", nil, func(context.Context, int) (any, error) {
				placed.Add(1)
				return map[string]any{"order_id": "ord-" + m.Mode}, nil
			})
		}},
		{Kind: "dn.unwind", Protective: true, Run: func(c *journal.Ctx, _ json.RawMessage) error {
			return c.SleepUntil(c.Now().Add(24 * time.Hour))
		}},
	}
	eng, err := journal.New(db, defs, journal.RealClock{}, journal.WithTick(10*time.Millisecond), journal.WithWorkers(4))
	if err != nil {
		t.Fatal(err)
	}
	runCtx, stopRun := context.WithCancel(ctx)
	runDone := make(chan struct{})
	go func() { defer close(runDone); _ = eng.Run(runCtx) }()
	t.Cleanup(func() { stopRun(); <-runDone })

	// An earlier DCA run that placed an order which is still open on the exchange.
	oldID, err := eng.Start(ctx, "dca.execute", mode{"done"})
	if err != nil {
		t.Fatal(err)
	}
	if w, err := eng.Wait(ctx, oldID); err != nil || w.Status != journal.StatusSucceeded {
		t.Fatalf("setup: %+v, %v", w, err)
	}
	placedBefore := placed.Load()
	runningID, _ := eng.Start(ctx, "dca.execute", mode{"running"})
	waitingID, _ := eng.Start(ctx, "dca.execute", mode{"waiting"})
	unwindID, _ := eng.Start(ctx, "dn.unwind", nil)
	go func() { // the in-flight step finishes 100 ms after the cancel was requested
		for runCtx.Err() == nil {
			if w, err := eng.Get(ctx, runningID); err == nil && (w.CancelRequested || w.Status.Terminal()) {
				time.Sleep(100 * time.Millisecond)
				close(release)
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	select {
	case <-holding:
	case <-time.After(5 * time.Second):
		t.Fatal("the running workflow never started")
	}
	waitFor(t, "the waiting workflows to suspend", func() bool {
		a, _ := eng.Get(ctx, waitingID)
		b, _ := eng.Get(ctx, unwindID)
		return a.Status == journal.StatusWaiting && b.Status == journal.StatusWaiting
	})

	// --- the agent loop, in the middle of a turn that would last a minute.
	mb := bus.NewMessageBus()
	prov := &watchedProvider{started: make(chan struct{}), cancelled: make(chan struct{})}
	al := agent.NewAgentLoop(&config.Config{
		Agents:  config.AgentsConfig{Defaults: config.AgentDefaults{Workspace: t.TempDir(), Model: "watched-model", MaxTokens: 4096, MaxToolIterations: 5}},
		Session: config.SessionConfig{DMScope: "per-channel-peer"},
	}, mb, prov)
	alCtx, alCancel := context.WithCancel(ctx)
	alDone := make(chan struct{})
	go func() { defer close(alDone); _ = al.Run(alCtx) }()
	t.Cleanup(func() {
		alCancel()
		select {
		case <-alDone:
		case <-time.After(5 * time.Second):
		}
		al.Stop()
		al.Close()
	})

	// --- the rest of the world.
	state, err := control.NewState(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	log := &callLog{}
	st := &stamps{at: map[string]time.Duration{}}
	desk := newDesk(log)
	desk.open["ord-done"] = true // the agent's, from the earlier run
	desk.open["manual-1"] = true // the owner's own
	mandates := &fakeMandates{log: log, active: 1}
	proposals := &fakeProposals{log: log, pending: []approval.Cancelled{{Code: "7F3K"}}}
	k := &control.Kill{
		Flag: state, Turns: stampedTurns{l: al, s: st}, Flows: eng, Mandates: stampedMandates{mandates, st},
		Proposals: stampedProposals{proposals, st}, Desk: stampedDesk{desk, st},
		Orders: func(_ context.Context, w journal.Workflow, steps []journal.StepRecord) (broker.FlowOrders, error) {
			var fo broker.FlowOrders
			for _, rec := range steps {
				if rec.Name != "place" || rec.FinishedAt.IsZero() {
					continue
				}
				var o struct {
					OrderID string `json:"order_id"`
				}
				_ = json.Unmarshal(rec.Output, &o)
				fo.Settle = true
				fo.Orders = append(fo.Orders, broker.OrderRef{WorkflowID: w.ID, Kind: w.Kind, Provider: "okx", Market: "spot", Symbol: "BTC/USDT", Side: "buy", OrderID: o.OrderID})
			}
			return fo, nil
		},
	}
	d := control.New(control.Deps{Cfg: cfgFor("telegram"), Kill: state, Stop: k})
	ch := newFakeChannel("telegram", mb, d)

	// A turn that will not finish by itself.
	ch.say(ownerUser, ownerChat, "think about the market for a minute", nil)
	select {
	case <-prov.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the turn never reached the model")
	}

	// --- /stop
	st.begin = time.Now()
	ch.say(ownerUser, ownerChat, "/stop", nil)
	took := time.Since(st.begin)
	reply := ch.onlyReply(t).Content
	t.Logf("summary after %v:\n%s", took, reply)
	for step, at := range st.at {
		t.Logf("  %-22s at %v", step, at.Round(time.Millisecond))
	}
	if took >= 2*time.Second {
		t.Errorf("/stop took %v, want under 2s", took)
	}
	mustContain(t, "summary", reply, "Stopped.", "Cancelled 1 turn, 2 workflows (dca.execute x2), 1 proposal; revoked 1 mandate; cancelled 1 open order",
		"Protective, still running: dn.unwind", "Use /resume")

	// The turn really was cancelled, within a second of the command.
	select {
	case <-prov.cancelled:
	case <-time.After(time.Second):
		t.Error("the model turn was not cancelled")
	}
	// The flag is set, in the database.
	if !state.Killed() {
		t.Error("the kill flag is not engaged")
	}
	// Workflows: the running one stopped at its boundary without placing, the waiting
	// one was cancelled, the protective one is untouched.
	for id, want := range map[string]journal.Status{runningID: journal.StatusCancelled, waitingID: journal.StatusCancelled, unwindID: journal.StatusWaiting, oldID: journal.StatusSucceeded} {
		if w, err := eng.Get(ctx, id); err != nil || w.Status != want {
			t.Errorf("workflow %s: %+v, %v; want %s", id, w, err, want)
		}
	}
	time.Sleep(150 * time.Millisecond) // a cancelled workflow must stay quiet
	if got := placed.Load(); got != placedBefore {
		t.Errorf("a workflow placed %d order(s) after /stop", got-placedBefore)
	}
	// A legacy scheduled "dca:" job (a plan with no mandate) that falls due after /stop
	// buys nothing: execute_dca_order reads the same flag and refuses before it touches
	// a plan or an exchange (so it needs neither here).
	tools.SetKillFlag(state.Killed)
	t.Cleanup(func() { tools.SetKillFlag(nil) })
	job := tools.WithAutomationScope(tools.WithToolOrigin(ctx, tools.OriginCron), tools.AutomationScope{JobName: "dca:9:weekly", DCAPlanID: 9})
	if res := tools.NewExecuteDCAOrderTool(nil, nil).Execute(job, map[string]any{"plan_id": float64(9)}); !res.IsError ||
		!strings.Contains(res.ForLLM, "kill switch is engaged") {
		t.Errorf("a scheduled DCA job after /stop = %+v, want the kill-switch refusal", res)
	}
	// Mandates, proposals, orders.
	if mandates.active != 0 || len(proposals.pending) != 0 {
		t.Errorf("mandates active %d, proposals pending %d", mandates.active, len(proposals.pending))
	}
	if desk.isOpen("ord-done") {
		t.Error("the agent's open order is still open")
	}
	if !desk.isOpen("manual-1") {
		t.Error("the owner's own order was cancelled")
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
