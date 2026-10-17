package control_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/agent"
	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/attention"
	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/channels"
	"github.com/cryptoquantumwave/khunquant/pkg/commands"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/control"
	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/notify"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/providers"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex/sqlitextest"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

// blockingProvider blocks inside its first model call until the turn is cancelled,
// and remembers everything the model was shown.
type blockingProvider struct {
	started chan struct{}
	once    sync.Once
	mu      sync.Mutex
	calls   int
	seen    []string
}

func (p *blockingProvider) Chat(ctx context.Context, msgs []providers.Message, _ []providers.ToolDefinition, _ string, _ map[string]any) (*providers.LLMResponse, error) {
	p.mu.Lock()
	p.calls++
	for _, m := range msgs {
		p.seen = append(p.seen, m.Content)
	}
	p.mu.Unlock()
	p.once.Do(func() { close(p.started) })
	<-ctx.Done()
	return nil, ctx.Err()
}

func (p *blockingProvider) GetDefaultModel() string { return "block-model" }

func (p *blockingProvider) snapshot() (int, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls, strings.Join(p.seen, "\n")
}

// While the agent is in the middle of a long turn (a real AgentLoop.Run on the
// same bus, blocked inside the model call), /approve is handled and answered
// within a second, and neither the agent nor the model ever sees it. Telegram,
// Discord and LINE are BaseChannels named after the platform with a recording
// Send: the repo has no fake client for any of them.
func TestControl_HandledBeforeAgent_DuringLongTurn(t *testing.T) {
	for _, platform := range []string{"telegram", "discord", "line"} {
		t.Run(platform, func(t *testing.T) {
			mb := bus.NewMessageBus()
			prov := &blockingProvider{started: make(chan struct{})}
			al := agent.NewAgentLoop(&config.Config{
				Agents: config.AgentsConfig{Defaults: config.AgentDefaults{
					Workspace: t.TempDir(), Model: "block-model", MaxTokens: 4096, MaxToolIterations: 5,
				}},
				Session: config.SessionConfig{DMScope: "per-channel-peer"},
			}, mb, prov)

			runCtx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { defer close(done); _ = al.Run(runCtx) }()
			t.Cleanup(func() {
				cancel()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Error("the agent loop did not stop")
				}
				al.Stop()
				al.Close()
			})

			appr := &fakeApprover{cards: map[string]approval.Proposal{}}
			d := control.New(control.Deps{Cfg: cfgFor(platform), Kill: &fakeKill{}, Approvals: appr})
			ch := newFakeChannel(platform, mb, d)

			// A turn that will not finish.
			ch.say(ownerUser, ownerChat, "think about the market for a very long time", nil)
			select {
			case <-prov.started:
			case <-time.After(5 * time.Second):
				t.Fatal("the long turn never reached the model")
			}

			begin := time.Now()
			ch.say(ownerUser, ownerChat, "/approve 7F3K", nil)
			if took := time.Since(begin); took > time.Second {
				t.Errorf("/approve took %v while a turn was running, want under 1s", took)
			}
			mustContain(t, "reply", ch.onlyReply(t).Content, "Approved proposal 7F3K")
			if got := appr.calls(); len(got) != 1 || got[0].Code != "7F3K" || !got[0].Approve {
				t.Errorf("Decide calls = %+v", got)
			}

			// Give a (wrongly) queued message time to be picked up: the agent is
			// still in its one turn and the model was shown no code.
			time.Sleep(150 * time.Millisecond)
			calls, seen := prov.snapshot()
			if calls != 1 {
				t.Errorf("the model was called %d times, want 1 (the long turn only)", calls)
			}
			if strings.Contains(seen, "7F3K") || strings.Contains(seen, "/approve") {
				t.Errorf("the model saw the command:\n%s", seen)
			}
			if n := len(mb.InboundChan()); n != 0 {
				t.Errorf("%d messages are queued for the agent", n)
			}
		})
	}
}

// The operator approval of a held tool call has an eight-character code; a
// proposal's has four. The two cannot be mistaken for each other, and the control
// path leaves the operator one to the command path that answers it.
func TestControl_OperatorAndProposalCodesCannotCollide(t *testing.T) {
	store := tools.NewApprovalStore(0)
	key := tools.ApprovalKey{Channel: "telegram", ChatID: ownerChat, SenderID: "telegram:" + ownerUser, Tool: "place_order", ArgsHash: "h"}
	code, err := store.Request(key)
	if err != nil {
		t.Fatal(err)
	}
	if approval.ValidCode(approval.NormalizeCode(code)) {
		t.Fatalf("operator code %q has the shape of a proposal code", code)
	}
	r := newRig(t, "telegram", nil)
	r.ch.say(ownerUser, ownerChat, "/approve "+code, nil)
	assertAgentGot(t, r, "/approve "+code)
	r.ch.say(ownerUser, ownerChat, "/approve "+strings.ToLower(code), nil)
	assertAgentGot(t, r, "/approve "+strings.ToLower(code))

	// And through the command registry the agent runs, the operator word still works.
	var approved string
	def := findCommand(t, "approve")
	rt := &commands.Runtime{ApproveToolCall: func(c, _, _, _ string) (string, error) { approved = c; return "place_order", nil }}
	var said string
	if err := def.Handler(context.Background(), commands.Request{
		Channel: "telegram", ChatID: ownerChat, SenderID: "telegram:" + ownerUser, Text: "/approve " + code,
		Reply: func(s string) error { said = s; return nil },
	}, rt); err != nil || approved != code || !strings.Contains(said, "Approved place_order") {
		t.Errorf("operator /approve: err=%v approved=%q said=%q", err, approved, said)
	}
}

func findCommand(t *testing.T, name string) commands.Definition {
	t.Helper()
	for _, d := range commands.BuiltinDefinitions() {
		if d.Name == name {
			return d
		}
	}
	t.Fatalf("no command %q", name)
	return commands.Definition{}
}

// /whoami moved to the control path; the alias in pkg/commands that keeps the menu
// entry gives the same text.
func TestControl_WhoamiAliasAndControlPathAgree(t *testing.T) {
	type scenario struct {
		name     string
		cfg      func() *config.Config
		channel  string
		chat     string
		sender   string
		peerKind string
	}
	owner := func() *config.Config { return cfgFor("telegram") }
	scenarios := []scenario{
		{"owner in the owner chat", owner, "telegram", ownerChat, "telegram:" + ownerUser, "direct"},
		{"owner in a group", owner, "telegram", "-100500", "telegram:" + ownerUser, "group"},
		{"owner in another chat", owner, "telegram", "999", "telegram:" + ownerUser, "direct"},
		{"someone else", owner, "telegram", "2002", "telegram:2002", "direct"},
		{"a raw sender id", owner, "irc", "#c", "Bob", ""},
		{"no owner", func() *config.Config { c := owner(); c.Proactive.Owner = config.OwnerConfig{}; return c }, "discord", "c1", "discord:42", "direct"},
		{"a discord group", owner, "discord", "c9", "discord:42", "channel"},
	}
	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			cfg := s.cfg()
			d := control.New(control.Deps{Cfg: cfg})
			rep, ok := d.TryHandle(context.Background(), control.Input{
				Channel: s.channel, ChatID: s.chat, SenderID: s.sender, PeerKind: s.peerKind, Text: "/whoami",
			})
			if !ok {
				t.Fatal("the control path did not handle /whoami")
			}
			var alias string
			err := findCommand(t, "whoami").Handler(context.Background(), commands.Request{
				Channel: s.channel, ChatID: s.chat, SenderID: s.sender, PeerKind: s.peerKind, Text: "/whoami",
				Reply: func(text string) error { alias = text; return nil },
			}, &commands.Runtime{Config: cfg})
			if err != nil {
				t.Fatal(err)
			}
			if rep.Text != alias || rep.Text == "" {
				t.Errorf("control:\n%s\nalias:\n%s", rep.Text, alias)
			}
		})
	}
}

// /snooze end to end: a real governor decides and delivers an alert through the
// outbox and the notify service, and a reply to the message the platform reported
// finds the alert's dedup key through the decision log and mutes it. If notify and
// attention disagreed about the intervention id, every snooze would answer
// "nothing to snooze".
func TestControl_SnoozeMapsAReplyBackToTheAlert(t *testing.T) {
	ctx := context.Background()
	db := sqlitextest.Open(t)
	clock := func() time.Time { return time.Date(2026, 3, 10, 12, 0, 0, 0, time.FixedZone("ICT", 7*3600)) }

	cfg := cfgFor("telegram")
	sender := &recordingSender{ids: []string{"555"}}
	ns, err := notify.New(cfg, sender, nil, db, "main", clock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ns.Close(ctx) })

	profiles, err := intent.NewStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	gov, err := attention.New(db, profiles, config.AttentionConfig{Mode: config.ProactiveModeLive, DailyBudget: 3, DedupWindowHours: 24},
		config.ProactiveModeLive, clock)
	if err != nil {
		t.Fatal(err)
	}
	ob := attention.NewOutbox(gov, notifyDeliverer{ns}, 0, clock)

	cand := proactive.Candidate{TriggerID: "price_move", DedupKey: "price_move:BTC/USDT", Severity: proactive.SeverityWarning,
		Subject: "BTC/USDT", Title: "BTC moved 5%"}
	d, err := ob.Submit(ctx, attention.Item{Candidate: cand})
	if err != nil || d.Disposition != proactive.DispNotify {
		t.Fatalf("Submit: %+v, %v", d, err)
	}
	if err := ob.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if sender.count() != 1 {
		t.Fatalf("%d messages were sent", sender.count())
	}

	disp := control.New(control.Deps{Cfg: cfg, Sent: ns, Attention: ob}, control.WithClock(clock))
	ch := newFakeChannel("telegram", bus.NewMessageBus(), disp)

	ch.say(ownerUser, ownerChat, "/snooze 4h", replyMeta("555"))
	mustContain(t, "reply", ch.onlyReply(t).Content, "Snoozed for 4h")

	again, err := gov.Decide(ctx, cand)
	if err != nil {
		t.Fatal(err)
	}
	if again.Disposition != proactive.DispDrop || again.Reason != attention.ReasonMuted {
		t.Errorf("the alert after /snooze: %s/%s, want drop/muted", again.Disposition, again.Reason)
	}

	// The snooze is exact: a different threshold on the same symbol, whose key the
	// snoozed one is a prefix of, is still delivered.
	sibling := cand
	sibling.DedupKey = cand.DedupKey + ":65000"
	sibling.TriggerID = "price_move:other"
	sib, err := gov.Decide(ctx, sibling)
	if err != nil {
		t.Fatal(err)
	}
	if sib.Disposition != proactive.DispNotify {
		t.Errorf("a sibling alert after /snooze: %s/%s, want notify", sib.Disposition, sib.Reason)
	}

	// /mute of the kind still silences the family.
	ch3 := newFakeChannel("telegram", bus.NewMessageBus(), disp)
	ch3.say(ownerUser, ownerChat, "/mute price_move", nil)
	mustContain(t, "reply", ch3.onlyReply(t).Content, "Muted price_move")
	sibling.DedupKey, sibling.TriggerID = "price_move:ETH/USDT:2", "price_move"
	fam, err := gov.Decide(ctx, sibling)
	if err != nil || fam.Disposition != proactive.DispDrop || fam.Reason != attention.ReasonMuted {
		t.Errorf("the family after /mute: %+v, %v", fam, err)
	}

	// A reply to a message that was not an alert snoozes nothing.
	ch2 := newFakeChannel("telegram", bus.NewMessageBus(), disp)
	ch2.say(ownerUser, ownerChat, "/snooze", replyMeta("12345"))
	mustContain(t, "reply", ch2.onlyReply(t).Content, "Nothing to snooze")
}

type recordingSender struct {
	mu   sync.Mutex
	ids  []string
	sent []bus.OutboundMessage
}

func (s *recordingSender) SendAndReport(_ context.Context, m bus.OutboundMessage) channels.DeliveryReceipt {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, m)
	return channels.DeliveryReceipt{Msg: m, PlatformMessageIDs: s.ids}
}

func (s *recordingSender) count() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.sent) }

// notifyDeliverer is the gateway's attentionDeliverer, reduced to what the mapping needs.
type notifyDeliverer struct{ svc *notify.Service }

func (n notifyDeliverer) Deliver(ctx context.Context, m attention.Message) (bool, error) {
	res, err := n.svc.Deliver(ctx, notify.Notification{
		Severity: m.Severity, Title: m.Title, Body: m.Body, DedupKey: m.DedupKey, InterventionID: m.InterventionID,
	})
	return err == nil && res.Delivered, err
}
