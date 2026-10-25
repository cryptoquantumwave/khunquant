package control_test

import (
	"context"
	"sync"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

// markApprover records whether the context of each Decide carried the web marker.
type markApprover struct {
	*fakeApprover
	mu  sync.Mutex
	web []bool
}

func (m *markApprover) Decide(ctx context.Context, code, nonce string, approve bool, sender string) (approval.Proposal, error) {
	m.mu.Lock()
	m.web = append(m.web, approval.IsWebDecider(ctx))
	m.mu.Unlock()
	return m.fakeApprover.Decide(ctx, code, nonce, approve, sender)
}

// webRig is a dispatcher whose configured owner is on Telegram, fronted by the pico
// channel, with condition (c) armed as given.
func webRig(t *testing.T, gate bool, mutate func(*config.Config)) (*rig, *markApprover) {
	t.Helper()
	config.SetWebLauncherGate(func(string) (bool, string) { return gate, "no" })
	t.Cleanup(func() { config.SetWebLauncherGate(nil) })
	r := newRig(t, "pico", func(c *config.Config) {
		c.Proactive.Owner = config.OwnerConfig{Channel: "telegram", ChatID: ownerChat, SenderID: "telegram:" + ownerUser}
		if mutate != nil {
			mutate(c)
		}
	})
	m := &markApprover{fakeApprover: r.appr}
	deps := r.d.Deps()
	deps.Approvals = m
	r.d.SetDeps(deps)
	return r, m
}

// sayPico delivers a dashboard chat message the way the pico channel does.
func (r *rig) sayPico(authenticated bool, text string) {
	r.ch.HandleMessage(context.Background(), bus.Peer{Kind: "direct", ID: "pico:s1"}, "m1", "pico-user", "pico:s1", text, nil, nil,
		bus.SenderInfo{Platform: "pico", PlatformID: "pico-user", CanonicalID: "pico:pico-user", WebAuthenticated: authenticated})
}

func TestControl_WebChatIsTheOwner(t *testing.T) {
	r, m := webRig(t, true, nil)
	r.sayPico(true, "/approve 7F3K")
	mustContain(t, "reply", r.ch.onlyReply(t).Content, "Approved proposal 7F3K")
	if len(m.web) != 1 || !m.web[0] {
		t.Fatalf("Decide marker = %v, want [true]", m.web)
	}
	r.sayPico(true, "/stop")
	if got := r.replies(); len(got) < 2 || !r.kill.Killed() {
		t.Errorf("/stop from the web chat did not engage the kill switch (replies %d)", len(got))
	}
}

func (r *rig) replies() []bus.OutboundMessage { return r.ch.replies() }

func TestControl_WebChatIsNotTheOwnerWhenAConditionFails(t *testing.T) {
	off := false
	cases := []struct {
		name          string
		gate          bool
		authenticated bool
		mutate        func(*config.Config)
	}{
		{"(b) connection not token-authenticated", true, false, nil},
		{"(c) launcher authentication not established", false, true, nil},
		{"(a) no owner configured", true, true, func(c *config.Config) { c.Proactive.Owner = config.OwnerConfig{} }},
		{"web_is_owner false", true, true, func(c *config.Config) { c.Proactive.Owner.WebIsOwner = &off }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, m := webRig(t, tc.gate, tc.mutate)
			r.sayPico(tc.authenticated, "/approve 7F3K")
			rep := r.ch.onlyReply(t).Content
			if len(m.web) != 0 {
				t.Fatalf("Decide was called (%v) for a message that is not the owner: %q", m.web, rep)
			}
			if tc.name == "(a) no owner configured" {
				mustContain(t, "reply", rep, "No owner is configured")
			} else {
				mustContain(t, "reply", rep, "Not authorized.")
			}
		})
	}
}

// A telegram message cannot borrow the web route by carrying the flag.
func TestControl_OnlyThePicoChannelCanBeTheWebOwner(t *testing.T) {
	config.SetWebLauncherGate(func(string) (bool, string) { return true, "" })
	t.Cleanup(func() { config.SetWebLauncherGate(nil) })
	r := newRig(t, "telegram", func(c *config.Config) {
		c.Proactive.Owner = config.OwnerConfig{Channel: "discord", ChatID: "9", SenderID: "discord:9"}
	})
	r.ch.HandleMessage(context.Background(), bus.Peer{Kind: "direct", ID: otherUser}, "m1", otherUser, otherUser, "/approve 7F3K", nil, nil,
		bus.SenderInfo{Platform: "telegram", PlatformID: otherUser, CanonicalID: "telegram:" + otherUser, WebAuthenticated: true})
	mustContain(t, "reply", r.ch.onlyReply(t).Content, "Not authorized.")
	if len(r.appr.calls()) != 0 {
		t.Error("Decide was called")
	}
}
