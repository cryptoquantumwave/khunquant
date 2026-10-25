package control_test

import (
	"context"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/channels"
)

func mandateDecide(code string, approve bool) (approval.Proposal, error) {
	st := approval.StatusExecuted
	if !approve {
		st = approval.StatusRejected
	}
	return approval.Proposal{Code: code, Kind: approval.KindMandate, Status: st,
		Summary: "Allow KhunQuant to BUY BTC/THB on bitkub: up to 2000 THB per run"}, nil
}

// The owner approves a mandate card through /approve and through its button: the reply
// says a mandate is active and that nothing was bought, and does not talk of an order.
func TestControl_ApprovingAMandateCard(t *testing.T) {
	r := newRig(t, "telegram", nil)
	r.appr.decideFn = mandateDecide
	r.ch.say(ownerUser, ownerChat, "/approve 7F3K", nil)
	reply := r.ch.onlyReply(t).Content
	mustContain(t, "reply", reply, "Approved mandate 7F3K", "The mandate is active", "Nothing was bought")
	for _, bad := range []string{"receipt", "Placing the order", "executed"} {
		if strings.Contains(reply, bad) {
			t.Errorf("a mandate reply talks of an order (%q): %q", bad, reply)
		}
	}

	in := channels.ControlInput{
		Channel: "telegram", ChatID: ownerChat, MessageID: "m2", SenderID: "telegram:" + ownerUser, PeerKind: "direct",
		Callback: &channels.ControlCallback{Action: "approve", Payload: "7F3K.abcdef0123456789", CallbackID: "cb1"},
	}
	rep, ok := r.d.TryHandle(context.Background(), in)
	if !ok || !strings.Contains(rep.Text, "Approved mandate 7F3K") || !strings.Contains(rep.Text, "The mandate is active") {
		t.Fatalf("approve button: %+v, %v", rep, ok)
	}
	in.Callback = &channels.ControlCallback{Action: "reject", Payload: "7F3K.abcdef0123456789", CallbackID: "cb2"}
	rep, ok = r.d.TryHandle(context.Background(), in)
	if !ok || !strings.Contains(rep.Text, "Rejected mandate 7F3K") || !strings.Contains(rep.Text, "no mandate was created") {
		t.Fatalf("reject button: %+v, %v", rep, ok)
	}
}
