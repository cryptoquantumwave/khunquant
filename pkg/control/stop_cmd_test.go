package control_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/control"
)

// countingStopper wraps a Kill and counts the runs.
type countingStopper struct {
	runs atomic.Int32
	k    *control.Kill
}

func (c *countingStopper) Stop(ctx context.Context, by, reason string) control.StopSummary {
	c.runs.Add(1)
	return c.k.Stop(ctx, by, reason)
}

func stopRig(t *testing.T) (*killRig, *countingStopper, *control.Dispatcher, *fakeChannel) {
	t.Helper()
	r := newKillRig(t)
	cs := &countingStopper{k: r.k}
	d := control.New(control.Deps{Cfg: cfgFor("telegram"), Kill: r.flag, Stop: cs})
	return r, cs, d, newFakeChannel("telegram", nil, d)
}

// The owner's /stop runs the whole sequence and replies with its summary.
func TestStop_OwnerGetsTheSummary(t *testing.T) {
	r, cs, _, ch := stopRig(t)
	r.turns.n = 1
	r.mandates.active = 2
	ch.say(ownerUser, ownerChat, "/stop", nil)
	rep := ch.onlyReply(t).Content
	mustContain(t, "reply", rep, "Stopped.", "Cancelled 1 turn", "revoked 2 mandates", "Use /resume")
	if cs.runs.Load() != 1 || !r.flag.Killed() {
		t.Errorf("runs=%d killed=%v", cs.runs.Load(), r.flag.Killed())
	}
}

// Somebody who is not the owner is told so and nothing happens: no flag, no cancel.
func TestStop_NonOwnerDenied(t *testing.T) {
	r, cs, _, ch := stopRig(t)
	r.flows.add("dca.execute", "waiting", nil)
	ch.say(otherUser, otherUser, "/stop", nil)
	if got := ch.onlyReply(t).Content; got != "Not authorized." {
		t.Errorf("reply = %q", got)
	}
	if cs.runs.Load() != 0 || r.flag.Killed() || len(r.log.all()) != 0 {
		t.Errorf("a non-owner /stop did something: runs=%d calls=%v", cs.runs.Load(), r.log.all())
	}
}

// /stop is exempt from the rate limit for the owner, however often it is sent; the
// same number of another command is limited.
func TestStop_ExemptFromTheRateLimit(t *testing.T) {
	_, cs, _, ch := stopRig(t)
	for i := 0; i < control.DefaultRateLimit+5; i++ {
		ch.say(ownerUser, ownerChat, "/stop", nil)
	}
	for i, m := range ch.replies() {
		if strings.Contains(m.Content, "Too many commands") {
			t.Fatalf("/stop #%d was rate limited", i+1)
		}
	}
	if got := cs.runs.Load(); int(got) != control.DefaultRateLimit+5 {
		t.Errorf("%d runs", got)
	}
	for i := 0; i < control.DefaultRateLimit+2; i++ {
		ch.say(ownerUser, ownerChat, "/mute price_move", nil)
	}
	last := ch.replies()
	if !strings.Contains(last[len(last)-1].Content, "Too many commands") {
		t.Errorf("a different command was not limited: %q", last[len(last)-1].Content)
	}
}

// /resume clears the flag and restores nothing: mandates stay revoked and proposals
// stay cancelled, and nothing is called to bring them back.
func TestResume_DoesNotRestoreMandates(t *testing.T) {
	r, _, _, ch := stopRig(t)
	r.mandates.active = 3
	ch.say(ownerUser, ownerChat, "/stop", nil)
	if r.mandates.active != 0 {
		t.Fatal("setup: the mandates were not revoked")
	}
	before := r.log.all()
	ch.say(ownerUser, ownerChat, "/resume", nil)
	if r.flag.Killed() {
		t.Error("the flag is still engaged after /resume")
	}
	if r.mandates.active != 0 {
		t.Errorf("mandates came back: %d active", r.mandates.active)
	}
	after := r.log.all()
	if len(after) != len(before)+1 || after[len(after)-1] != "flag.clear" {
		t.Errorf("calls after /resume = %v (before %v); only the flag may change", after[len(before):], before)
	}
	reps := ch.replies()
	mustContain(t, "resume reply", reps[len(reps)-1].Content, "Resumed")
}

// Without a Stopper (proactive features partly off) /stop still engages the flag.
func TestStop_WithoutAStopperOnlyEngagesTheFlag(t *testing.T) {
	flag := &fakeKill{}
	d := control.New(control.Deps{Cfg: cfgFor("telegram"), Kill: flag})
	ch := newFakeChannel("telegram", nil, d)
	ch.say(ownerUser, ownerChat, "/stop", nil)
	mustContain(t, "reply", ch.onlyReply(t).Content, "Stopped acting")
	if !flag.Killed() {
		t.Error("the flag was not engaged")
	}
}

// The reply is inside the 2 s budget even with a stuck exchange, and the owner is
// told the rest follows.
func TestStop_StuckExchangeStillRepliesInBudget(t *testing.T) {
	r, _, _, ch := stopRig(t)
	wf := r.flows.add("approval.order", "succeeded", nil)
	r.desk.open["x"] = true
	r.desk.gate = make(chan struct{})
	r.flows.orders[wf.ID] = brokerOrders(wf, "x")
	begin := time.Now()
	ch.say(ownerUser, ownerChat, "/stop", nil)
	if took := time.Since(begin); took > control.DefaultReplyBudget+500*time.Millisecond {
		t.Errorf("the reply took %v", took)
	}
	mustContain(t, "reply", ch.onlyReply(t).Content, "second message")
	close(r.desk.gate)
	deadline := time.Now().Add(5 * time.Second)
	for len(r.followups()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if len(r.followups()) != 1 {
		t.Errorf("followups = %v", r.followups())
	}
}
