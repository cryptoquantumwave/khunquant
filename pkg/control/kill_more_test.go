package control_test

import (
	"context"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/agent"
	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/control"
	"github.com/cryptoquantumwave/khunquant/pkg/journal"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// A /stop that finds another running still engages the flag. The earlier run can hold
// the lock for a long while and /resume does not touch it: /stop, /resume, /stop must
// leave the switch on.
func TestStop_BusyStillEngagesTheFlag(t *testing.T) {
	r := newKillRig(t)
	r.k.ReplyBudget = 30 * time.Millisecond
	wf := r.flows.add("approval.order", journal.StatusSucceeded, nil)
	r.desk.open["slow"] = true
	r.desk.gate = make(chan struct{})
	r.flows.orders[wf.ID] = brokerOrders(wf, "slow")

	first := r.stop()
	if !first.Pending || !first.Engaged {
		t.Fatalf("first = %+v", first)
	}
	if _, err := r.flag.Clear(context.Background(), "owner"); err != nil { // /resume
		t.Fatal(err)
	}
	if r.flag.Killed() {
		t.Fatal("setup: the flag was not cleared")
	}
	r.turns.n = 3
	second := r.stop()
	if !second.Busy || !second.Engaged || !r.flag.Killed() {
		t.Fatalf("a busy /stop must set the flag: %+v, killed=%v", second, r.flag.Killed())
	}
	if second.TurnsCancelled != 3 {
		t.Errorf("turns cancelled again = %d, want 3", second.TurnsCancelled)
	}
	if r.log.count("flag.engage") != 2 {
		t.Errorf("flag.engage called %d times, want 2", r.log.count("flag.engage"))
	}
	// And a flag that cannot be set is not reported as engaged.
	r.flag.mu.Lock()
	r.flag.on, r.flag.err = false, errBoom
	r.flag.mu.Unlock()
	if third := r.stop(); !third.Busy || third.Engaged {
		t.Errorf("third = %+v; want busy and not engaged", third)
	}
	close(r.desk.gate)
}

// An approved exit is Protect AND its order is an Exit: the workflow is not
// cancelled, its order is not looked up or withdrawn, and the summary names it.
func TestStop_ProtectedWorkflowsExitOrderIsNamed(t *testing.T) {
	r := newKillRig(t)
	wf := r.flows.add("approval.order", journal.StatusWaiting, nil)
	r.desk.open["exit-1"] = true
	r.flows.orders[wf.ID] = broker.FlowOrders{Protect: true, Orders: []broker.OrderRef{
		ref(wf, "exit-1", "ETH/USDT:USDT", func(o *broker.OrderRef) { o.Exit, o.Side, o.Market = true, "sell", "futures" }),
	}}
	sum := r.stop()
	if r.flows.status(wf.ID) != journal.StatusWaiting || r.log.count("flows.cancel") != 0 {
		t.Error("the protective workflow was cancelled")
	}
	if !r.desk.isOpen("exit-1") || r.desk.lookups != 0 {
		t.Errorf("the exit order was touched (lookups %d)", r.desk.lookups)
	}
	mustHave(t, "exit orders left", sum.ExitOrdersLeft, "sell ETH/USDT:USDT on okx")
	mustHave(t, "protective", sum.WorkflowsProtective, "approval.order")
}

// /stop revokes every mandate and cancels every pending proposal (an exit among
// them: the owner can approve a new one after /resume), and reports both.
func TestStop_RevokesMandatesCancelsProposals(t *testing.T) {
	r := newKillRig(t)
	r.mandates.active = 2
	r.proposals.pending = []approval.Cancelled{
		{Code: "AAAA", Kind: approval.KindOrder}, {Code: "BBBB", Kind: approval.KindOrder}, {Code: "CCCC", Kind: approval.KindMandate},
	}
	sum := r.stop()
	if sum.MandatesRevoked != 2 || r.mandates.active != 0 {
		t.Errorf("mandates: revoked %d, still active %d", sum.MandatesRevoked, r.mandates.active)
	}
	if sum.ProposalsCancelled != 3 || len(r.proposals.pending) != 0 {
		t.Errorf("proposals: cancelled %d, still pending %d", sum.ProposalsCancelled, len(r.proposals.pending))
	}
	mustContain(t, "text", sum.Text(), "3 proposals", "revoked 2 mandates")
}

// A turn that would last a minute is cancelled by /stop, and the model call returns
// within a second of the command.
func TestStop_DuringLLMTurn_CancelsWithin1s(t *testing.T) {
	ctx := context.Background()
	mb := bus.NewMessageBus()
	prov := &watchedProvider{started: make(chan struct{}), cancelled: make(chan struct{})}
	al := agent.NewAgentLoop(&config.Config{
		Agents:  config.AgentsConfig{Defaults: config.AgentDefaults{Workspace: t.TempDir(), Model: "watched-model", MaxTokens: 4096, MaxToolIterations: 5}},
		Session: config.SessionConfig{DMScope: "per-channel-peer"},
	}, mb, prov)
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); _ = al.Run(runCtx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
		al.Stop()
		al.Close()
	})

	log := &callLog{}
	flag := &fakeFlag{log: log}
	d := control.New(control.Deps{Cfg: cfgFor("telegram"), Kill: flag, Stop: &control.Kill{Flag: flag, Turns: al}})
	ch := newFakeChannel("telegram", mb, d)

	ch.say(ownerUser, ownerChat, "think about the market for a minute", nil)
	select {
	case <-prov.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the turn never reached the model")
	}
	begin := time.Now()
	ch.say(ownerUser, ownerChat, "/stop", nil)
	select {
	case <-prov.cancelled:
	case <-time.After(time.Second):
		t.Fatal("the model call was not cancelled within 1s of /stop")
	}
	t.Logf("turn cancelled %v after /stop", time.Since(begin))
	mustContain(t, "reply", ch.onlyReply(t).Content, "Stopped.", "Cancelled 1 turn")
	if !flag.Killed() {
		t.Error("the flag is not engaged")
	}
}
