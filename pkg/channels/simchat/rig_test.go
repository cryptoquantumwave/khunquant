package simchat

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/channels"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labjournal"
)

const testToken = "t-0123456789abcdef0123456789abcdef"

// lockForTest locks the test process (the only way simchat exists) and unlocks it after.
func lockForTest(t *testing.T) {
	t.Helper()
	sandbox.ResetLockForTest()
	t.Cleanup(sandbox.ResetLockForTest)
	if err := sandbox.Lock(sandbox.LockOptions{Token: testToken}); err != nil {
		t.Fatal(err)
	}
}

func testConfig(mutate ...func(*config.SimChatConfig)) *config.Config {
	cfg := config.DefaultConfig()
	cfg.Channels.SimChat.Enabled = true
	cfg.Channels.SimChat.AllowFrom = config.FlexibleStringSlice{"*"}
	for _, m := range mutate {
		m(&cfg.Channels.SimChat)
	}
	return cfg
}

// fakeRouter is the control router: it records what it is offered and handles the
// inputs handle says it does.
type fakeRouter struct {
	mu        sync.Mutex
	taps      []channels.ControlInput
	reactions []string
	reactIn   []channels.ControlInput
	handle    func(channels.ControlInput) (channels.ControlReply, bool)
}

func (f *fakeRouter) TryHandle(_ context.Context, in channels.ControlInput) (channels.ControlReply, bool) {
	f.mu.Lock()
	f.taps = append(f.taps, in)
	h := f.handle
	f.mu.Unlock()
	if h == nil {
		return channels.ControlReply{}, false
	}
	return h(in)
}

func (f *fakeRouter) Recognises(in channels.ControlInput) bool {
	return in.Callback != nil || strings.HasPrefix(in.Text, "/")
}

func (f *fakeRouter) ObserveReply(context.Context, channels.ControlInput) {}

func (f *fakeRouter) ObserveReaction(_ context.Context, in channels.ControlInput, emoji string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reactions, f.reactIn = append(f.reactions, emoji), append(f.reactIn, in)
}

func (f *fakeRouter) seen() []channels.ControlInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]channels.ControlInput(nil), f.taps...)
}

type rig struct {
	t      *testing.T
	b      *bus.MessageBus
	j      *labjournal.Journal
	w      *World
	ch     *Channel
	router *fakeRouter
}

// newRig locks the process and builds a running, attached channel on a real journal.
func newRig(t *testing.T, mutate ...func(*config.SimChatConfig)) *rig {
	t.Helper()
	lockForTest(t)
	cfg := testConfig(mutate...)
	r := &rig{t: t, b: bus.NewMessageBus(), router: &fakeRouter{}}
	t.Cleanup(r.b.Close)
	var err error
	if r.j, err = labjournal.Open(labjournal.Options{Path: filepath.Join(t.TempDir(), "journal.db")}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.j.Close() })
	r.w = NewWorld(r.j, time.Now, cfg.Channels.SimChat)
	if r.ch, err = New(cfg, r.b); err != nil {
		t.Fatal(err)
	}
	r.ch.SetControlRouter(r.router)
	r.ch.SetOwner(r.ch)
	r.ch.Attach(r.w)
	if err := r.ch.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	return r
}

// run calls an op of the chat group with args (a JSON object).
func (r *rig) run(op, args string) (any, error) {
	r.t.Helper()
	for _, o := range r.w.Group().Ops {
		if o.Name == op {
			return o.Run(context.Background(), json.RawMessage(args))
		}
	}
	r.t.Fatalf("no op %q", op)
	return nil, nil
}

func (r *rig) mustRun(op, args string) map[string]any {
	r.t.Helper()
	res, err := r.run(op, args)
	if err != nil {
		r.t.Fatalf("%s %s: %v", op, args, err)
	}
	b, _ := json.Marshal(res)
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		r.t.Fatal(err)
	}
	return m
}

// inbound returns the next message published to the agent, or false if none arrives.
func (r *rig) inbound() (bus.InboundMessage, bool) {
	select {
	case m := <-r.b.InboundChan():
		return m, true
	case <-time.After(300 * time.Millisecond):
		return bus.InboundMessage{}, false
	}
}

// entries returns the journal's entries of a kind.
func (r *rig) entries(kind string) []labjournal.Entry {
	r.t.Helper()
	es, err := r.j.Query(context.Background(), labjournal.Query{Kinds: []string{kind}})
	if err != nil {
		r.t.Fatal(err)
	}
	return es
}

func payloadOf(t *testing.T, e labjournal.Entry) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(e.Payload, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// card is a proactive approval card with buttons, as the approval service sends one.
func card(chat string) bus.OutboundMessage {
	return bus.OutboundMessage{
		Channel: "simchat", ChatID: chat, Content: "Buy 0.1 BTC?", Severity: 2,
		DedupKey: "order:BTC", InterventionID: "I-1", DisableNotification: true,
		Actions: []bus.Action{
			{ID: "approve", Label: "Approve", Payload: "AB12.nonce1", TextCommand: "/approve AB12"},
			{ID: "reject", Label: "Reject", Payload: "AB12.nonce1", TextCommand: "/reject AB12"},
			{ID: "mute", Label: "Mute", Payload: "I-1", TextCommand: "/mute price_move"},
		},
	}
}
