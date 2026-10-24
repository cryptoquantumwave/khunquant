package approval

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeReflectionSink records what the owner approved and can be made to fail.
type fakeReflectionSink struct {
	mu   sync.Mutex
	got  []Proposal
	fail error
}

func (f *fakeReflectionSink) ApplyApprovedReflection(_ context.Context, p Proposal) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return f.fail
	}
	f.got = append(f.got, p)
	return nil
}

func (f *fakeReflectionSink) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.got)
}

type reflRig struct {
	*proc
	sink   *fakeReflectionSink
	digest *fakeDigest
}

func newReflRig(t *testing.T) *reflRig {
	t.Helper()
	w := newWorld(t)
	r := &reflRig{sink: &fakeReflectionSink{}, digest: &fakeDigest{}}
	r.proc = w.newProc(t0, func(o *Options) { o.Reflections, o.Digest = r.sink, r.digest })
	return r
}

func triggerChange(to float64) ReflectionRequest {
	return ReflectionRequest{
		Trigger:   &TriggerParamParams{SpecID: "pf-move_vs_vol", Kind: "move_vs_vol", Param: "floor_th_pct", From: 3, To: to},
		Rationale: "You dismissed 4 of 4 SET move alerts.",
	}
}

func (r *reflRig) mustPropose(req ReflectionRequest) Proposal {
	r.t.Helper()
	p, err := r.svc.ProposeReflection(context.Background(), req)
	if err != nil {
		r.t.Fatalf("ProposeReflection: %v", err)
	}
	return p
}

func TestProposeReflection_QueuesForTheDigestAndChangesNothing(t *testing.T) {
	r := newReflRig(t)
	p := r.mustPropose(triggerChange(5))

	if p.Kind != KindTriggerParam || p.Status != StatusPending || p.Origin != OriginReflection {
		t.Fatalf("proposal = %+v", p)
	}
	if got := p.ExpiresAt.Sub(p.CreatedAt); got != 48*time.Hour {
		t.Errorf("expires after %s, want 48h", got)
	}
	if r.sink.count() != 0 || len(r.w.notifier.all()) != 0 {
		t.Error("a proposal applied something or delivered a card before the owner decided")
	}
	cs := r.digest.all()
	if len(cs) != 1 || !strings.Contains(cs[0].Quote, "from 3 to 5") || cs[0].Evidence["why"] != "You dismissed 4 of 4 SET move alerts." {
		t.Fatalf("digest candidates = %+v", cs)
	}
	if strings.Contains(cs[0].Title, "floor") || strings.Contains(p.Summary, "floor") {
		t.Error("the change is in the title or summary, which can reach a session")
	}
	if v, err := VerifiedTriggerParam(r.get(p.ID)); err != nil || v.To != 5 || v.From != 3 {
		t.Errorf("params = %+v, %v", v, err)
	}
}

func TestApprovedReflectionApplied_RejectedNot(t *testing.T) {
	r := newReflRig(t)
	p := r.mustPropose(triggerChange(5))
	if got := r.approve(p); got.Status != StatusExecuted {
		t.Fatalf("status = %s", got.Status)
	}
	if r.sink.count() != 1 || r.sink.got[0].DecidedBy != ownerID || r.sink.got[0].Status != StatusExecuting {
		t.Fatalf("sink saw %+v", r.sink.got)
	}
	q := r.mustPropose(triggerChange(6))
	if _, err := r.svc.Decide(context.Background(), q.Code, q.Nonce, false, ownerID); err != nil {
		t.Fatal(err)
	}
	if r.sink.count() != 1 || r.get(q.ID).Status != StatusRejected {
		t.Error("a rejected change was applied")
	}
	// Only the owner decides.
	z := r.mustPropose(triggerChange(7))
	if _, err := r.svc.Decide(context.Background(), z.Code, z.Nonce, true, "telegram:999"); !errors.Is(err, ErrNotOwner) {
		t.Errorf("a stranger's approval = %v", err)
	}
}

func TestReflectionRefusedByTheSinkFailsAndNeverRetries(t *testing.T) {
	r := newReflRig(t)
	r.sink.fail = fmt.Errorf("%w: moved", ErrReflectionRefused)
	p := r.mustPropose(triggerChange(5))
	if got := r.approve(p); got.Status != StatusFailed || !strings.Contains(got.Outcome, "moved") {
		t.Fatalf("status = %s outcome %q", got.Status, got.Outcome)
	}
	// A transient error stays executing, to be tried again.
	r.sink.fail = errors.New("database is locked")
	q := r.mustPropose(triggerChange(6))
	if got := r.approve(q); got.Status != StatusExecuting {
		t.Fatalf("status = %s, want executing", got.Status)
	}
	r.sink.fail = nil
	if err := r.svc.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.get(q.ID).Status != StatusExecuted || r.sink.count() != 1 {
		t.Errorf("recover left %s, applied %d", r.get(q.ID).Status, r.sink.count())
	}
}

func TestReflectionBoundsAreTheirOwnAndIndependentOfOrders(t *testing.T) {
	r := newReflRig(t)
	for i := 0; i < MaxPendingReflection; i++ {
		r.mustPropose(triggerChange(float64(4 + i)))
	}
	_, err := r.svc.ProposeReflection(context.Background(), triggerChange(9))
	var pe *PendingError
	if !errors.As(err, &pe) {
		t.Fatalf("a sixth pending item = %v, want a PendingError", err)
	}
	// Reflection items never use the order slots: an order can still be proposed.
	if p := r.propose(sellBTC()); p.Kind != KindOrder {
		t.Fatalf("order proposal = %+v", p)
	}
}

func TestReflectionProposalsNeedAnOwnerASinkAndAValidChange(t *testing.T) {
	w := newWorld(t)
	noSink := w.newProc(t0, func(o *Options) { o.Digest = &fakeDigest{} })
	if _, err := noSink.svc.ProposeReflection(context.Background(), triggerChange(5)); !errors.Is(err, ErrReflectionUnavailable) {
		t.Errorf("without a sink = %v", err)
	}
	r := newReflRig(t)
	bad := []ReflectionRequest{
		{},
		{Trigger: &TriggerParamParams{SpecID: "a", Kind: "k", Param: "p", From: 1, To: 1}},
		{Mute: &MuteParams{Prefix: "move_vs_vol:", Days: 31}},
		{Mute: &MuteParams{Prefix: "move_vs_vol:", Days: 7}, Intent: &IntentCleanupParams{IntentID: "i", Action: CleanupPause}},
		{Intent: &IntentCleanupParams{IntentID: "i", Action: "activate"}},
	}
	for i, req := range bad {
		if _, err := r.svc.ProposeReflection(context.Background(), req); err == nil {
			t.Errorf("request %d was accepted: %+v", i, req)
		}
	}
	if n := len(r.digest.all()); n != 0 {
		t.Errorf("%d invalid requests reached the digest", n)
	}
}

func TestReflectionKindsRoundTripAndDescribe(t *testing.T) {
	r := newReflRig(t)
	m := r.mustPropose(ReflectionRequest{Mute: &MuteParams{Prefix: "move_vs_vol:", Days: 7}, Rationale: "Noisy."})
	i := r.mustPropose(ReflectionRequest{Intent: &IntentCleanupParams{IntentID: "01ABC", Action: CleanupPause, Title: "Watch SOL"}, Rationale: "Stale."})
	for _, c := range []struct {
		p    Proposal
		want string
	}{{m, `Mute alerts starting "move_vs_vol:" for 7 days`}, {i, `Pause intent 01ABC "Watch SOL"`}} {
		got, err := DescribeReflection(r.get(c.p.ID))
		if err != nil || got != c.want {
			t.Errorf("Describe = %q, %v; want %q", got, err, c.want)
		}
	}
	// A tampered row does not verify.
	tampered := r.get(m.ID)
	tampered.Params = []byte(`{"prefix":"price:","days":30}`)
	if _, err := VerifiedMute(tampered); err == nil {
		t.Error("a tampered mute verified")
	}
	if _, err := VerifiedTriggerParam(r.get(m.ID)); err == nil {
		t.Error("a mute verified as a trigger change")
	}
}

func TestReflectionActionsAreBuiltOnlyForWhatTheOwnerRead(t *testing.T) {
	r := newReflRig(t)
	p := r.mustPropose(triggerChange(5))
	read, _ := DescribeReflection(r.get(p.ID))
	acts := r.svc.ReflectionActions(context.Background(), []ShownReflection{{Code: p.Code, Change: read}}, 8)
	if len(acts) != 2 || acts[0].Payload != ButtonPayload(p.Code, p.Nonce) || acts[1].TextCommand != "/reject "+p.Code {
		t.Fatalf("actions = %+v", acts)
	}
	if got := r.svc.ReflectionActions(context.Background(), []ShownReflection{{Code: p.Code, Change: read + " (edited)"}}, 8); len(got) != 0 {
		t.Errorf("buttons for a change the owner did not read: %+v", got)
	}
	if got := r.svc.ReflectionActions(context.Background(), []ShownReflection{{Code: p.Code, Change: read}}, 1); len(got) != 0 {
		t.Errorf("a pair that does not fit was built: %+v", got)
	}
}
