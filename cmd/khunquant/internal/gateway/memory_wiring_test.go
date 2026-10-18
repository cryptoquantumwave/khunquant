package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/attention"
	"github.com/cryptoquantumwave/khunquant/pkg/attention/attentiontest"
	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

type stubButtons struct {
	shown   []approval.ShownMemory
	changes []approval.ShownReflection
	room    int
}

func (s *stubButtons) ReflectionActions(_ context.Context, shown []approval.ShownReflection, max int) []bus.Action {
	s.changes, s.room = shown, max
	return []bus.Action{{ID: "approve", Label: "Save " + shown[0].Code}, {ID: "reject", Label: "Discard " + shown[0].Code}}
}

// A digest with a memory suggestion and a reflection change carries both pairs of buttons,
// the change's built against what the owner read and sized to the room the memories leave.
func TestAttentionDeliverer_DigestCarriesReflectionButtons(t *testing.T) {
	b := &stubButtons{}
	setDigestButtons(b)
	t.Cleanup(func() { setDigestButtons(nil) })
	m := memoryDigestMessage()
	m.Decisions = append(m.Decisions, attention.Decision{Candidate: proactive.Candidate{
		TriggerID: proactive.ReflectionTriggerPrefix + "ZX9Q", DedupKey: "reflection:01Y", Title: "Suggestion ZX9Q (mute)",
		Quote: `Mute alerts starting "move_vs_vol:" for 7 days`,
	}})
	n := &capturingNotifier{}
	if ok, err := (attentionDeliverer{svc: n}).Deliver(context.Background(), m); err != nil || !ok {
		t.Fatalf("Deliver = %v, %v", ok, err)
	}
	if got := n.got[0].Actions; len(got) != 4 {
		t.Fatalf("actions = %+v, want the memory pair and the change pair", got)
	}
	if len(b.changes) != 1 || b.changes[0].Code != "ZX9Q" || b.changes[0].Change != `Mute alerts starting "move_vs_vol:" for 7 days` || b.room != bus.MaxActions-2 {
		t.Errorf("changes = %+v room %d", b.changes, b.room)
	}
}

func (s *stubButtons) MemoryActions(_ context.Context, shown []approval.ShownMemory) []bus.Action {
	s.shown = shown
	return []bus.Action{{ID: "approve", Label: "Save " + shown[0].Code}, {ID: "reject", Label: "Discard " + shown[0].Code}}
}

func memoryDigestMessage() attention.Message {
	c := proactive.Candidate{
		TriggerID: proactive.MemoryTriggerPrefix + "AB3K", DedupKey: "memory:01X", Title: "Memory suggestion AB3K (fact)",
		Quote: "I hold for the long term",
	}
	return attention.Message{
		Title: "Daily digest — 1 item", Body: "> I hold for the long term", Digest: true, Silent: true,
		Decisions: []attention.Decision{{Candidate: c}},
	}
}

// The digest's Save and Discard buttons are built by the approval service at send time,
// for the memory suggestions of a digest and for nothing else.
func TestAttentionDeliverer_DigestCarriesSaveAndDiscard(t *testing.T) {
	b := &stubButtons{}
	setDigestButtons(b)
	t.Cleanup(func() { setDigestButtons(nil) })

	n := &capturingNotifier{}
	d := attentionDeliverer{svc: n}
	if ok, err := d.Deliver(context.Background(), memoryDigestMessage()); err != nil || !ok {
		t.Fatalf("Deliver = %v, %v", ok, err)
	}
	if got := n.got[0].Actions; len(got) != 2 || got[0].Label != "Save AB3K" || got[1].Label != "Discard AB3K" {
		t.Errorf("actions = %+v", got)
	}
	if len(b.shown) != 1 || b.shown[0].Code != "AB3K" || b.shown[0].Fact != "I hold for the long term" {
		t.Errorf("shown = %+v: the buttons are checked against the text the owner read", b.shown)
	}

	// An ordinary digest, an ordinary message, and approvals not running: no buttons.
	plain := attention.Message{Title: "t", Body: "b", Digest: true, Decisions: []attention.Decision{{Candidate: proactive.Candidate{DedupKey: "a"}}}}
	notDigest := memoryDigestMessage()
	notDigest.Digest = false
	for name, m := range map[string]attention.Message{"ordinary digest": plain, "not a digest": notDigest} {
		n := &capturingNotifier{}
		if _, err := (attentionDeliverer{svc: n}).Deliver(context.Background(), m); err != nil || len(n.got[0].Actions) != 0 {
			t.Errorf("%s: actions %+v, %v", name, n.got[0].Actions, err)
		}
	}
	setDigestButtons(nil)
	n2 := &capturingNotifier{}
	if _, err := (attentionDeliverer{svc: n2}).Deliver(context.Background(), memoryDigestMessage()); err != nil || len(n2.got[0].Actions) != 0 {
		t.Errorf("approvals off: actions %+v, %v", n2.got[0].Actions, err)
	}
}

// A suggestion is queued only where the owner will see it: attention must decide it for the
// digest. Anything else is an error, and the proposal is cancelled by the service.
func TestMemoryDigest_QueueForDigest(t *testing.T) {
	c := proactive.Candidate{TriggerID: "memory:AB3K", DedupKey: "memory:01X", Severity: proactive.SeverityInfo, Quote: "x"}
	for disp, wantErr := range map[proactive.Disposition]bool{
		proactive.DispDigest: false, proactive.DispNotify: true, proactive.DispDrop: true, proactive.DispLog: true,
	} {
		f := &attentionTestGate{Fake: &attentiontest.Fake{Script: []attention.Decision{{Disposition: disp, Reason: "r"}}}}
		ob := attention.NewOutbox(f, &noDeliver{}, 0, nil)
		err := memoryDigest{ob: ob}.QueueForDigest(context.Background(), c)
		if (err != nil) != wantErr {
			t.Errorf("disposition %s: err = %v", disp, err)
		}
	}
}

type attentionTestGate struct{ *attentiontest.Fake }

type noDeliver struct{}

func (noDeliver) Deliver(context.Context, attention.Message) (bool, error) { return true, nil }

func TestMemoryDigestFor_NeedsALiveDigest(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Proactive.Enabled = true
	cfg.Proactive.Owner = config.OwnerConfig{Channel: "telegram", ChatID: "1001", SenderID: "telegram:1001"}
	cfg.Proactive.Attention.Mode = config.ProactiveModeLive
	cfg.Proactive.Attention.Digest = true
	ob := attention.NewOutbox(&attentiontest.Fake{}, &noDeliver{}, time.Second, nil)

	if memoryDigestFor(cfg, ob) == nil {
		t.Fatal("no digest queue with attention live and the digest on")
	}
	if memoryDigestFor(cfg, nil) != nil {
		t.Error("a queue without an outbox")
	}
	cfg.Proactive.Attention.Digest = false
	if memoryDigestFor(cfg, ob) != nil {
		t.Error("a queue with the digest switched off: the owner would never see it")
	}
	cfg.Proactive.Attention.Digest = true
	cfg.Proactive.Attention.Mode = config.ProactiveModeShadow
	if memoryDigestFor(cfg, ob) != nil {
		t.Error("a queue in shadow mode, where nothing is ever delivered")
	}
}
