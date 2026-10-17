package control_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/journal"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

func has(list []string, sub string) bool {
	for _, s := range list {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func mustHave(t *testing.T, what string, list []string, sub string) {
	t.Helper()
	if !has(list, sub) {
		t.Errorf("%s = %q, want an entry with %q", what, list, sub)
	}
}

// The sequence: the flag first, then turns, workflows, mandates and proposals, then
// the agent's orders; the summary says what each did.
func TestStop_Sequence_FlagFirstAndSummary(t *testing.T) {
	r := newKillRig(t)
	r.turns.n = 2
	r.mandates.active = 2
	r.proposals.pending = []approval.Cancelled{{Code: "AAAA"}, {Code: "BBBB"}, {Code: "CCCC"}}
	w1 := r.flows.add("dca.execute", journal.StatusWaiting, nil)
	r.flows.add("approval.order", journal.StatusPending, nil)
	r.desk.open["ord-1"] = true
	r.flows.orders[w1.ID] = broker.FlowOrders{Orders: []broker.OrderRef{ref(w1, "ord-1", "BTC/USDT", nil)}}

	sum := r.stop()
	if got := r.log.all(); len(got) == 0 || got[0] != "flag.engage" {
		t.Fatalf("the flag must be first; calls = %v", got)
	}
	order := []string{"flag.engage", "turns.cancel", "flows.cancel", "mandates.revoke", "proposals.cancel", "desk.cancel"}
	last := -1
	for _, c := range order {
		i := r.log.index(c)
		if i < 0 || i < last {
			t.Fatalf("step %q out of order or missing; calls = %v", c, r.log.all())
		}
		last = i
	}
	if !sum.Engaged || sum.AlreadyEngaged || sum.Pending || sum.TurnsCancelled != 2 || len(sum.WorkflowsCancelled) != 2 ||
		sum.ProposalsCancelled != 3 || sum.MandatesRevoked != 2 || len(sum.OrdersCancelled) != 1 {
		t.Fatalf("summary = %+v", sum)
	}
	text := sum.Text()
	for _, want := range []string{
		"Stopped.", "Cancelled 2 turns, 2 workflows (approval.order, dca.execute), 3 proposals; revoked 2 mandates; cancelled 1 open order.",
		"buy BTC/USDT on okx", "Monitoring and notifications stay on. Use /resume to allow approvals again; mandates must be approved again.",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}
	// Idempotent: a second /stop runs again, says so, finds nothing new.
	again := r.stop()
	if !again.AlreadyEngaged || len(again.WorkflowsCancelled) != 0 || len(again.OrdersCancelled) != 0 {
		t.Errorf("second run = %+v", again)
	}
	if !strings.Contains(again.Text(), "Stopped again") {
		t.Errorf("second text:\n%s", again.Text())
	}
}

// A protective workflow (an unwind, a recovery, an approved exit), a resize that
// will refuse, and a workflow that is settling a placed order all keep running; the
// rest are cancelled. The filter reads flags, not kind names.
func TestStop_ProtectiveWorkflowContinues(t *testing.T) {
	r := newKillRig(t)
	unwind := r.flows.add("dn.unwind", journal.StatusRunning, func(w *journal.Workflow) { w.Protective = true })
	exit := r.flows.add("approval.order", journal.StatusWaiting, nil)
	r.flows.orders[exit.ID] = broker.FlowOrders{Protect: true}
	resize := r.flows.add("dn.resize", journal.StatusRunning, nil)
	r.flows.orders[resize.ID] = broker.FlowOrders{Protect: true, Note: "increase: refuses at its next place step"}
	open := r.flows.add("dn.open", journal.StatusRunning, nil)
	r.flows.orders[open.ID] = broker.FlowOrders{Settle: true}
	dcaWf := r.flows.add("dca.execute", journal.StatusWaiting, nil)
	// A kind nobody has ever heard of is cancelled, not guessed to be safe.
	odd := r.flows.add("mystery.flow", journal.StatusPending, nil)

	sum := r.stop()
	for _, id := range []string{unwind.ID, exit.ID, resize.ID, open.ID} {
		if st := r.flows.status(id); st == journal.StatusCancelled || r.log.count("flows.cancel:"+id) != 0 {
			t.Errorf("workflow %s was cancelled (status %s)", id, st)
		}
	}
	for _, id := range []string{dcaWf.ID, odd.ID} {
		if st := r.flows.status(id); st != journal.StatusCancelled {
			t.Errorf("workflow %s status = %s, want cancelled", id, st)
		}
	}
	mustHave(t, "protective", sum.WorkflowsProtective, "dn.unwind")
	mustHave(t, "protective", sum.WorkflowsProtective, "approval.order")
	mustHave(t, "protective", sum.WorkflowsProtective, "dn.resize (increase: refuses at its next place step)")
	if len(sum.WorkflowsFinishing) != 1 || sum.WorkflowsFinishing[0] != "dn.open" {
		t.Errorf("finishing = %v", sum.WorkflowsFinishing)
	}
	if len(sum.WorkflowsCancelled) != 2 {
		t.Errorf("cancelled = %v", sum.WorkflowsCancelled)
	}
	text := sum.Text()
	if !strings.Contains(text, "Protective, still running:") || !strings.Contains(text, "Finishing without placing anything more: dn.open") {
		t.Errorf("text:\n%s", text)
	}
}

// Only orders the journal names are touched. The owner's own open order sits in the
// same book and is never looked at; the agent's reduce-only exit is left and named;
// one already filled is skipped.
func TestStop_CancelsOnlyAgentOrders(t *testing.T) {
	r := newKillRig(t)
	wf := r.flows.add("approval.order", journal.StatusSucceeded, nil)
	r.desk.open["manual-1"] = true // the owner's, by hand
	r.desk.open["agent-limit"] = true
	r.desk.open["agent-exit"] = true
	r.desk.open["agent-done"] = false
	r.flows.orders[wf.ID] = broker.FlowOrders{Settle: true, Orders: []broker.OrderRef{
		ref(wf, "agent-limit", "BTC/USDT", nil),
		ref(wf, "agent-exit", "ETH/USDT", func(o *broker.OrderRef) { o.Exit, o.Side = true, "sell" }),
		ref(wf, "agent-done", "SOL/USDT", func(o *broker.OrderRef) { o.Done = true }),
	}}

	sum := r.stop()
	if !r.desk.isOpen("manual-1") {
		t.Error("the owner's own order was cancelled")
	}
	if r.desk.isOpen("agent-limit") {
		t.Error("the agent's open order was not cancelled")
	}
	if !r.desk.isOpen("agent-exit") {
		t.Error("the agent's reduce-only exit was cancelled")
	}
	if got := r.log.count("desk.cancel"); got != 1 {
		t.Errorf("%d cancels, want 1: %v", got, r.log.all())
	}
	if r.desk.lookups != 1 {
		t.Errorf("%d exchange lookups, want 1 (exit and done are not looked up)", r.desk.lookups)
	}
	mustHave(t, "cancelled", sum.OrdersCancelled, "BTC/USDT")
	mustHave(t, "exits left", sum.ExitOrdersLeft, "sell ETH/USDT")
	if !strings.Contains(sum.Text(), "Exit orders left in place") {
		t.Errorf("text:\n%s", sum.Text())
	}
}

// A workflow cut inside its place step: the order is looked up read-only by its
// idempotency key. Found open: cancelled. Found filled: reported, nothing done. Not
// found, or the lookup failing: named as one that may exist.
func TestStop_CutPlaceStep(t *testing.T) {
	cut := func(r *killRig, key string) *journal.Workflow {
		wf := r.flows.add("dca.execute", journal.StatusWaiting, nil)
		r.flows.orders[wf.ID] = broker.FlowOrders{Orders: []broker.OrderRef{ref(wf, "", "BTC/THB", func(o *broker.OrderRef) {
			o.Provider, o.IdemKey = "bitkub", key
			o.Request = &broker.OrderRequest{Market: "spot", Symbol: "BTC/THB", Type: "market", Side: "buy", Amount: 0.001}
		})}}
		return wf
	}
	t.Run("found open", func(t *testing.T) {
		r := newKillRig(t)
		wf := cut(r, "wf:3:place")
		r.desk.byKey["wf:3:place"], r.desk.open["ex-9"] = "ex-9", true
		sum := r.stop()
		if r.flows.status(wf.ID) != journal.StatusCancelled || r.desk.isOpen("ex-9") || len(sum.OrdersCancelled) != 1 || len(sum.OrdersMayExist) != 0 {
			t.Errorf("summary = %+v", sum)
		}
	})
	t.Run("found filled", func(t *testing.T) {
		r := newKillRig(t)
		cut(r, "wf:3:place")
		r.desk.byKey["wf:3:place"], r.desk.open["ex-9"] = "ex-9", false
		sum := r.stop()
		if r.log.count("desk.cancel") != 0 || len(sum.OrdersClosed) != 1 || len(sum.OrdersCancelled) != 0 || len(sum.OrdersMayExist) != 0 {
			t.Errorf("summary = %+v, calls %v", sum, r.log.all())
		}
	})
	t.Run("not found", func(t *testing.T) {
		r := newKillRig(t)
		cut(r, "wf:3:place")
		sum := r.stop()
		if len(sum.OrdersMayExist) != 1 || len(sum.OrdersCancelled) != 0 {
			t.Errorf("summary = %+v", sum)
		}
		if !strings.Contains(sum.Text(), "May exist: check the exchange: buy BTC/THB on bitkub") {
			t.Errorf("text:\n%s", sum.Text())
		}
	})
	t.Run("lookup fails", func(t *testing.T) {
		r := newKillRig(t)
		cut(r, "wf:3:place")
		r.desk.lookupErr["|wf:3:place"] = errors.New("exchange down")
		sum := r.stop()
		if len(sum.OrdersMayExist) != 1 || r.log.count("desk.cancel") != 0 {
			t.Errorf("summary = %+v", sum)
		}
	})
	t.Run("cancel fails", func(t *testing.T) {
		r := newKillRig(t)
		cut(r, "wf:3:place")
		r.desk.byKey["wf:3:place"], r.desk.open["ex-9"] = "ex-9", true
		r.desk.cancelErr = errors.New("rejected")
		sum := r.stop()
		if len(sum.OrdersMayExist) != 1 || !strings.Contains(sum.OrdersMayExist[0], "cancelling failed") {
			t.Errorf("summary = %+v", sum)
		}
	})
}

// A step that fails is reported and the next steps still run.
func TestStop_StepFailuresAreReportedAndLaterStepsRun(t *testing.T) {
	boom := errors.New("boom")
	cases := []struct {
		name string
		fail func(r *killRig)
		want string
	}{
		{"flag", func(r *killRig) { r.flag.err = boom }, "kill flag"},
		{"workflows", func(r *killRig) { r.flows.listErr = boom }, "workflows"},
		{"mandates", func(r *killRig) { r.mandates.err = boom }, "mandates"},
		{"proposals", func(r *killRig) { r.proposals.err = boom }, "proposals"},
		{"orders", func(r *killRig) { r.flows.listErrAfter = 2 }, "orders"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newKillRig(t)
			r.flows.add("dca.execute", journal.StatusWaiting, nil)
			r.mandates.active = 1
			r.proposals.pending = []approval.Cancelled{{Code: "AAAA"}}
			tc.fail(r)
			sum := r.stop()
			mustHave(t, "errors", sum.Errors, tc.want)
			if tc.name != "flag" && !sum.Engaged {
				t.Error("the flag was not engaged")
			}
			if tc.name == "flag" && sum.Engaged {
				t.Error("a flag that did not write is reported engaged")
			}
			// Everything after the failed step still ran.
			after := map[string][]string{
				"flag":      {"turns.cancel", "flows.cancel", "mandates.revoke", "proposals.cancel"},
				"workflows": {"mandates.revoke", "proposals.cancel"},
				"mandates":  {"proposals.cancel"},
				"proposals": {},
				"orders":    {},
			}[tc.name]
			for _, c := range after {
				if r.log.index(c) < 0 {
					t.Errorf("%q did not run after %s failed; calls = %v", c, tc.name, r.log.all())
				}
			}
			if tc.name == "flag" && !strings.Contains(sum.Text(), "WARNING: the kill flag could not be set") {
				t.Errorf("text:\n%s", sum.Text())
			}
		})
	}
}

// A workflow that was mid-step when it was cancelled lands its order before it stops;
// the orders are looked for after it has, and it is counted as cancelled. One that
// never stops is named after the wait.
func TestStop_RunningWorkflowSettlesBeforeOrdersAreLooked(t *testing.T) {
	t.Run("settles", func(t *testing.T) {
		r := newKillRig(t)
		wf := r.flows.add("dca.execute", journal.StatusRunning, nil)
		r.desk.open["late-1"] = true
		r.k.SettleWait = 2 * time.Second
		go func() {
			for r.log.count("flows.cancel") == 0 {
				time.Sleep(time.Millisecond)
			}
			time.Sleep(60 * time.Millisecond)
			// The in-flight place step lands, then the workflow stops at the boundary.
			r.flows.mu.Lock()
			r.flows.orders[wf.ID] = broker.FlowOrders{Settle: true, Orders: []broker.OrderRef{ref(wf, "late-1", "BTC/USDT", nil)}}
			r.flows.mu.Unlock()
			r.flows.set(wf.ID, journal.StatusCancelled)
		}()
		sum := r.stop()
		if r.desk.isOpen("late-1") || len(sum.OrdersCancelled) != 1 || len(sum.WorkflowsCancelled) != 1 || len(sum.WorkflowsStopping) != 0 {
			t.Errorf("summary = %+v", sum)
		}
	})
	t.Run("never stops", func(t *testing.T) {
		r := newKillRig(t)
		r.flows.add("dca.execute", journal.StatusRunning, nil)
		sum := r.stop()
		if len(sum.WorkflowsStopping) != 1 || len(sum.WorkflowsCancelled) != 0 {
			t.Errorf("summary = %+v", sum)
		}
		mustHave(t, "errors", sum.Errors, "still mid-step")
	})
}

// The reply comes inside the budget; what is not finished goes on and its outcome
// arrives as a second message. A second /stop meanwhile is told one is running.
func TestStop_ReplyBudgetAndBackgroundContinuation(t *testing.T) {
	r := newKillRig(t)
	r.k.ReplyBudget = 50 * time.Millisecond
	wf := r.flows.add("approval.order", journal.StatusSucceeded, nil)
	r.desk.open["slow-1"] = true
	r.desk.gate = make(chan struct{})
	r.flows.orders[wf.ID] = broker.FlowOrders{Settle: true, Orders: []broker.OrderRef{ref(wf, "slow-1", "BTC/USDT", nil)}}
	r.mandates.active = 1

	begin := time.Now()
	sum := r.stop()
	if took := time.Since(begin); took > time.Second {
		t.Errorf("the reply took %v", took)
	}
	if !sum.Pending || sum.MandatesRevoked != 1 || len(sum.OrdersCancelled) != 0 {
		t.Fatalf("reply = %+v", sum)
	}
	if !strings.Contains(sum.Text(), "second message") {
		t.Errorf("text:\n%s", sum.Text())
	}
	if busy := r.stop(); !busy.Busy || !strings.Contains(busy.Text(), "already running") {
		t.Errorf("a second stop while one runs = %+v", busy)
	}
	if len(r.followups()) != 0 {
		t.Fatal("the second message came before the work ended")
	}
	close(r.desk.gate)
	deadline := time.Now().Add(5 * time.Second)
	for len(r.followups()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	f := r.followups()
	if len(f) != 1 || !strings.Contains(f[0], "cancelled 1 open order") || strings.Contains(f[0], "second message") {
		t.Fatalf("followups = %q", f)
	}
	if r.desk.isOpen("slow-1") {
		t.Error("the order was not cancelled in the background")
	}
	// The next stop is not busy any more.
	if next := r.stop(); next.Busy {
		t.Error("still busy after the run ended")
	}
}
