package control_test

import (
	"context"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/control"
)

// explodingDeps is every dependency of the control path, and panics if any is used.
type explodingDeps struct{}

func (explodingDeps) Decide(context.Context, string, string, bool, string) (approval.Proposal, error) {
	panic("Decide called")
}

func (explodingDeps) PendingByCard(context.Context, string, string, string) (approval.Proposal, error) {
	panic("PendingByCard called")
}

func (explodingDeps) LookupByPlatformMessageID(context.Context, string, string) (string, bool, error) {
	panic("lookup called")
}
func (explodingDeps) Mute(context.Context, string, time.Time, string) error { panic("Mute called") }
func (explodingDeps) MuteExact(context.Context, string, time.Time, string) error {
	panic("MuteExact called")
}
func (explodingDeps) Unmute(context.Context, string) error { panic("Unmute called") }
func (explodingDeps) InterventionKeys(context.Context, string) ([]string, error) {
	panic("InterventionKeys called")
}

// What the agent path pays for the control path: for a message that is not a
// command, one call that looks at the text, touches no dependency and allocates
// nothing.
func TestControl_NonCommandCostsNothing(t *testing.T) {
	x := explodingDeps{}
	d := control.New(control.Deps{Cfg: cfgFor("telegram"), Approvals: x, Sent: x, Attention: x, Kill: &fakeKill{}})
	ctx := context.Background()
	for _, text := range []string{"hello there", "what is BTC doing?", "", "  ", "yes", "ok",
		"I think /approve is a command", "สวัสดีครับ", "/approvee", "/approve ABCD1234"} {
		in := control.Input{Channel: "telegram", ChatID: ownerChat, SenderID: "telegram:" + ownerUser, Text: text}
		if _, ok := d.TryHandle(ctx, in); ok {
			t.Errorf("%q was handled", text)
		}
		if d.Recognises(in) {
			t.Errorf("%q is recognised", text)
		}
	}
	in := control.Input{Channel: "telegram", ChatID: ownerChat, SenderID: "telegram:" + ownerUser, Text: "good morning, how are the markets"}
	if n := testing.AllocsPerRun(200, func() { _, _ = d.TryHandle(ctx, in) }); n != 0 {
		t.Errorf("TryHandle allocates %v times for a message that is not a command", n)
	}
	reply := in
	reply.ReplyToMessageID = "42"
	reply.Text = "thanks, that makes sense"
	if n := testing.AllocsPerRun(200, func() { _, _ = d.TryHandle(ctx, reply) }); n != 0 {
		t.Errorf("TryHandle allocates %v times for an ordinary reply", n)
	}
}
