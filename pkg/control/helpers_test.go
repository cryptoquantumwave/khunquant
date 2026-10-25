package control_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/channels"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/control"
)

const (
	ownerUser = "1001"
	ownerChat = "1001"
	otherUser = "2002"
)

// cfgFor is a config with proactive on and the owner on the given channel, in a
// direct chat whose id is the owner's user id.
func cfgFor(channel string) *config.Config {
	cfg := config.DefaultConfig()
	cfg.Proactive.Enabled = true
	cfg.Proactive.Owner = config.OwnerConfig{Channel: channel, ChatID: ownerChat, SenderID: channel + ":" + ownerUser}
	cfg.Proactive.Approvals = config.ApprovalsConfig{Enabled: true, ExpiryMinutes: 10}
	return cfg
}

// fakeChannel is a channel as the control path sees one: a BaseChannel with a
// recording Send, and the typing, reaction and placeholder capabilities a real
// platform has, each counting its calls. The repo has no fake Telegram, Discord or
// LINE client; tests name a fakeChannel after the platform instead.
type fakeChannel struct {
	*channels.BaseChannel
	mu   sync.Mutex
	sent []bus.OutboundMessage

	typing, reactions, placeholders atomic.Int32
}

func newFakeChannel(name string, mb *bus.MessageBus, router channels.ControlRouter, allow ...string) *fakeChannel {
	f := &fakeChannel{BaseChannel: channels.NewBaseChannel(name, nil, mb, allow)}
	f.SetOwner(f)
	f.SetPlaceholderRecorder(nopRecorder{})
	if router != nil {
		f.SetControlRouter(router)
	}
	return f
}

func (f *fakeChannel) Start(context.Context) error { return nil }
func (f *fakeChannel) Stop(context.Context) error  { return nil }
func (f *fakeChannel) Send(_ context.Context, m bus.OutboundMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, m)
	return nil
}

func (f *fakeChannel) StartTyping(context.Context, string) (func(), error) {
	f.typing.Add(1)
	return func() {}, nil
}

func (f *fakeChannel) ReactToMessage(context.Context, string, string) (func(), error) {
	f.reactions.Add(1)
	return func() {}, nil
}

func (f *fakeChannel) SendPlaceholder(context.Context, string) (string, error) {
	f.placeholders.Add(1)
	return "ph1", nil
}

func (f *fakeChannel) replies() []bus.OutboundMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]bus.OutboundMessage(nil), f.sent...)
}

// lastReply is the text of the only reply, failing if there is not exactly one.
func (f *fakeChannel) onlyReply(t *testing.T) bus.OutboundMessage {
	t.Helper()
	r := f.replies()
	if len(r) != 1 {
		t.Fatalf("%d replies, want 1: %+v", len(r), r)
	}
	return r[0]
}

type nopRecorder struct{}

func (nopRecorder) RecordPlaceholder(string, string, string)  {}
func (nopRecorder) RecordTypingStop(string, string, func())   {}
func (nopRecorder) RecordReactionUndo(string, string, func()) {}

// say delivers a message from user in chat the way a platform adapter would.
func (f *fakeChannel) say(user, chat, text string, meta map[string]string) {
	f.sayAs("direct", user, chat, text, meta)
}

func (f *fakeChannel) sayAs(peerKind, user, chat, text string, meta map[string]string) {
	platform := f.Name()
	f.HandleMessage(context.Background(), bus.Peer{Kind: peerKind, ID: user}, "m1", user, chat, text, nil, meta,
		bus.SenderInfo{Platform: platform, PlatformID: user, CanonicalID: platform + ":" + user})
}

// fakeApprover records Decide calls and answers from a script.
type fakeApprover struct {
	mu       sync.Mutex
	decides  []decideCall
	decideFn func(code string, approve bool) (approval.Proposal, error)
	cards    map[string]approval.Proposal // "channel|chat|message" -> proposal
	cardErr  error
}

type decideCall struct {
	Code, Nonce, Sender string
	Approve             bool
}

func (a *fakeApprover) Decide(_ context.Context, code, nonce string, approve bool, sender string) (approval.Proposal, error) {
	a.mu.Lock()
	a.decides = append(a.decides, decideCall{code, nonce, sender, approve})
	fn := a.decideFn
	a.mu.Unlock()
	if fn != nil {
		return fn(code, approve)
	}
	st := approval.StatusExecuting
	if !approve {
		st = approval.StatusRejected
	}
	return approval.Proposal{Code: code, Summary: "sell 0.01 BTC on okx", Status: st}, nil
}

func (a *fakeApprover) PendingByCard(_ context.Context, channel, chatID, messageID string) (approval.Proposal, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cardErr != nil {
		return approval.Proposal{}, a.cardErr
	}
	if p, ok := a.cards[channel+"|"+chatID+"|"+messageID]; ok {
		return p, nil
	}
	return approval.Proposal{}, approval.ErrNotFound
}

func (a *fakeApprover) calls() []decideCall {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]decideCall(nil), a.decides...)
}

// fakeSent maps a platform message id to an intervention id.
type fakeSent map[string]string

func (s fakeSent) LookupByPlatformMessageID(_ context.Context, _, id string) (string, bool, error) {
	v, ok := s[id]
	return v, ok, nil
}

// fakeAttention records mutes.
type fakeAttention struct {
	mu     sync.Mutex
	mutes  map[string]time.Time
	unmute []string
	keys   map[string][]string // intervention id -> dedup keys
	err    error
}

func (a *fakeAttention) Mute(_ context.Context, prefix string, until time.Time, _ string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err != nil {
		return a.err
	}
	if a.mutes == nil {
		a.mutes = map[string]time.Time{}
	}
	a.mutes[prefix] = until
	return nil
}

// MuteExact is recorded under "exact:"+key, apart from the prefix mutes.
func (a *fakeAttention) MuteExact(_ context.Context, key string, until time.Time, _ string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err != nil {
		return a.err
	}
	if a.mutes == nil {
		a.mutes = map[string]time.Time{}
	}
	a.mutes["exact:"+key] = until
	return nil
}

func (a *fakeAttention) Unmute(_ context.Context, prefix string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.unmute = append(a.unmute, prefix)
	return nil
}

func (a *fakeAttention) InterventionKeys(_ context.Context, id string) ([]string, error) {
	return a.keys[id], nil
}

func (a *fakeAttention) muted() map[string]time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := map[string]time.Time{}
	for k, v := range a.mutes {
		out[k] = v
	}
	return out
}

// fakeKill is a KillSwitch in memory.
type fakeKill struct {
	mu sync.Mutex
	on bool
	by string
}

func (k *fakeKill) Killed() bool { k.mu.Lock(); defer k.mu.Unlock(); return k.on }
func (k *fakeKill) Engage(_ context.Context, by, _ string) (bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	ch := !k.on
	k.on, k.by = true, by
	return ch, nil
}

func (k *fakeKill) Clear(context.Context, string) (bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	ch := k.on
	k.on = false
	return ch, nil
}

var errBoom = errors.New("boom")

// rig is a dispatcher with fakes behind it and a channel in front of it.
type rig struct {
	t    *testing.T
	d    *control.Dispatcher
	ch   *fakeChannel
	bus  *bus.MessageBus
	appr *fakeApprover
	att  *fakeAttention
	kill *fakeKill
	sent fakeSent
}

func newRig(t *testing.T, channel string, mutate func(*config.Config)) *rig {
	t.Helper()
	cfg := cfgFor(channel)
	if mutate != nil {
		mutate(cfg)
	}
	r := &rig{
		t: t, bus: bus.NewMessageBus(),
		appr: &fakeApprover{cards: map[string]approval.Proposal{}},
		att:  &fakeAttention{keys: map[string][]string{}},
		kill: &fakeKill{},
		sent: fakeSent{},
	}
	r.d = control.New(control.Deps{
		Cfg: cfg, Kill: r.kill, Approvals: r.appr, Sent: r.sent, Attention: r.att,
	})
	r.ch = newFakeChannel(channel, r.bus, r.d)
	return r
}

// inboundCount drains what reached the agent's inbound queue.
func (r *rig) inbound() []bus.InboundMessage {
	var out []bus.InboundMessage
	for {
		select {
		case m := <-r.bus.InboundChan():
			out = append(out, m)
		case <-time.After(20 * time.Millisecond):
			return out
		}
	}
}
