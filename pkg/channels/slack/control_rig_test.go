package slack

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"
	"github.com/stretchr/testify/require"

	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/channels"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/control"
	"github.com/cryptoquantumwave/khunquant/pkg/providers"
)

const (
	ownerID    = "U0OWNER"
	otherID    = "U0OTHER"
	ownerDM    = "D0OWNERDM"
	publicChan = "C0PUBLIC"
	cardTS     = "1700000000.000100"
	approveVal = "kq1|apv|7F3K.abcd1234"
	rejectVal  = "kq1|rej|7F3K.abcd1234"
)

// ---------------------------------------------------------------------------
// A Slack Web API stub. A real slack-go client talks to it over HTTP (the API
// URL option), so what is checked is what would go on the wire.
// ---------------------------------------------------------------------------

type apiCall struct {
	Method string // "chat.postMessage", "chat.postEphemeral", "chat.update", ...
	Form   url.Values
}

type apiStub struct {
	srv *httptest.Server

	mu         sync.Mutex
	calls      []apiCall
	nextTS     int
	failUpdate bool
}

func newAPIStub(t *testing.T) *apiStub {
	t.Helper()
	s := &apiStub{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		method := strings.TrimPrefix(r.URL.Path, "/")
		s.mu.Lock()
		s.calls = append(s.calls, apiCall{Method: method, Form: r.PostForm})
		s.nextTS++
		ts := "1700000000.00" + strconv.Itoa(1000+s.nextTS)
		fail := s.failUpdate && method == "chat.update"
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if fail {
			_, _ = w.Write([]byte(`{"ok":false,"error":"message_not_found"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "channel": r.PostForm.Get("channel"), "ts": ts, "message_ts": ts,
		})
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *apiStub) callsTo(method string) []apiCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []apiCall
	for _, c := range s.calls {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

func (s *apiStub) total() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

// blocks decodes the "blocks" form field of a call.
func (c apiCall) blocks(t *testing.T) []map[string]any {
	t.Helper()
	raw := c.Form.Get("blocks")
	if raw == "" {
		return nil
	}
	var out []map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &out))
	return out
}

// ---------------------------------------------------------------------------
// The rig: a real SlackChannel on the stub, running.
// ---------------------------------------------------------------------------

type rig struct {
	t    *testing.T
	api  *apiStub
	ch   *SlackChannel
	mb   *bus.MessageBus
	cfg  *config.Config
	acks []string
	mu   sync.Mutex
}

func ownerConfig() *config.Config {
	cfg := config.DefaultConfig()
	cfg.Proactive.Enabled = true
	cfg.Proactive.Owner = config.OwnerConfig{Channel: "slack", ChatID: ownerDM, SenderID: "slack:" + ownerID}
	cfg.Proactive.Approvals = config.ApprovalsConfig{Enabled: true, ExpiryMinutes: 10}
	return cfg
}

func newRigWith(t *testing.T, mb *bus.MessageBus, allow []string, router channels.ControlRouter) *rig {
	t.Helper()
	stub := newAPIStub(t)
	base := channels.NewBaseChannel("slack", config.SlackConfig{}, mb, allow,
		channels.WithMaxMessageLength(maxMessageLength))
	base.SetRunning(true)
	if router != nil {
		base.SetControlRouter(router)
	}
	r := &rig{t: t, api: stub, mb: mb, cfg: ownerConfig()}
	r.ch = &SlackChannel{
		BaseChannel: base,
		api:         slack.New("xoxb-test", slack.OptionAPIURL(stub.srv.URL+"/")),
		botUserID:   "U0BOT",
		ctx:         context.Background(),
		ackFn: func(req socketmode.Request, _ ...any) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.acks = append(r.acks, req.EnvelopeID)
			return nil
		},
	}
	return r
}

func newRig(t *testing.T, allow []string, router channels.ControlRouter) *rig {
	return newRigWith(t, bus.NewMessageBus(), allow, router)
}

// newRigWithApprover is a rig whose router is the real control.Dispatcher (a
// fresh one: it rate-limits per sender).
func newRigWithApprover(t *testing.T, appr control.Approver, allow []string) *rig {
	t.Helper()
	cfg := ownerConfig()
	r := newRig(t, allow, control.New(control.Deps{Cfg: cfg, Approvals: appr}))
	r.cfg = cfg
	return r
}

func (r *rig) ackList() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.acks...)
}

type tap struct {
	user, channel, value string
	actionID             string // default: derived from value
	threadTS, messageTS  string
}

// callback builds the payload as Slack sends it, and decodes it with slack-go's
// own decoder.
func (tp tap) callback(t *testing.T) slack.InteractionCallback {
	t.Helper()
	if tp.channel == "" {
		tp.channel = ownerDM
	}
	if tp.messageTS == "" {
		tp.messageTS = cardTS
	}
	if tp.actionID == "" {
		if _, rest, ok := strings.Cut(tp.value, "|"); ok {
			code, _, _ := strings.Cut(rest, "|")
			tp.actionID = "kq1_" + code
		}
	}
	msg := map[string]any{"type": "message", "ts": tp.messageTS}
	container := map[string]any{"type": "message", "message_ts": tp.messageTS, "channel_id": tp.channel}
	if tp.threadTS != "" {
		msg["thread_ts"] = tp.threadTS
		container["thread_ts"] = tp.threadTS
	}
	raw, err := json.Marshal(map[string]any{
		"type": "block_actions", "trigger_id": "trg-1",
		"user":      map[string]any{"id": tp.user},
		"channel":   map[string]any{"id": tp.channel},
		"container": container, "message": msg,
		"actions": []any{map[string]any{
			"action_id": tp.actionID, "block_id": "kq1_actions", "type": "button", "value": tp.value,
		}},
	})
	require.NoError(t, err)
	var cb slack.InteractionCallback
	require.NoError(t, json.Unmarshal(raw, &cb))
	return cb
}

// press handles a tap to completion (handleBlockActions is synchronous; the event
// loop is what runs it in a goroutine).
func (r *rig) press(tp tap) { r.ch.handleBlockActions(tp.callback(r.t)) }

func (r *rig) lastEphemeral() apiCall {
	r.t.Helper()
	c := r.api.callsTo("chat.postEphemeral")
	require.NotEmpty(r.t, c, "no ephemeral message was sent")
	return c[len(c)-1]
}

// ---------------------------------------------------------------------------
// Control-path fakes.
// ---------------------------------------------------------------------------

type fakeApprover struct {
	mu      sync.Mutex
	props   map[string]*fakeProposal
	decides []decision
}

type fakeProposal struct {
	nonce  string
	status approval.Status
}

type decision struct {
	Code, Nonce, Sender string
	Approve             bool
}

func newFakeApprover(code, nonce string) *fakeApprover {
	return &fakeApprover{props: map[string]*fakeProposal{code: {nonce: nonce, status: approval.StatusPending}}}
}

func (a *fakeApprover) Decide(_ context.Context, code, nonce string, approve bool, sender string) (approval.Proposal, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.decides = append(a.decides, decision{code, nonce, sender, approve})
	if sender != "slack:"+ownerID {
		return approval.Proposal{}, approval.ErrNotOwner
	}
	p, ok := a.props[code]
	if !ok || (nonce != "" && nonce != p.nonce) {
		return approval.Proposal{}, approval.ErrNotFound
	}
	if p.status != approval.StatusPending {
		return approval.Proposal{}, &approval.StatusError{Code: code, Status: p.status}
	}
	p.status = approval.StatusRejected
	st := approval.StatusRejected
	if approve {
		p.status = approval.StatusApproved
		st = approval.StatusExecuting
	}
	return approval.Proposal{Code: code, Summary: "sell 0.01 BTC on okx", Status: st}, nil
}

func (a *fakeApprover) PendingByCard(context.Context, string, string, string) (approval.Proposal, error) {
	return approval.Proposal{}, approval.ErrNotFound
}

func (a *fakeApprover) decisions() []decision {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]decision(nil), a.decides...)
}

func (a *fakeApprover) statusOf(code string) approval.Status {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.props[code].status
}

// scriptedRouter records what it is offered and answers from a function.
type scriptedRouter struct {
	mu  sync.Mutex
	got []channels.ControlInput
	fn  func(channels.ControlInput) (channels.ControlReply, bool)
}

func (s *scriptedRouter) TryHandle(_ context.Context, in channels.ControlInput) (channels.ControlReply, bool) {
	s.mu.Lock()
	s.got = append(s.got, in)
	fn := s.fn
	s.mu.Unlock()
	if fn == nil {
		return channels.ControlReply{}, false
	}
	return fn(in)
}
func (s *scriptedRouter) Recognises(channels.ControlInput) bool { return false }

func (s *scriptedRouter) inputs() []channels.ControlInput {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]channels.ControlInput(nil), s.got...)
}

func approvalActions() []bus.Action {
	return []bus.Action{
		{ID: "approve", Label: "Approve", Style: "primary", Payload: "7F3K.abcd1234", TextCommand: "/approve 7F3K"},
		{ID: "reject", Label: "Reject", Style: "danger", Payload: "7F3K.abcd1234", TextCommand: "/reject 7F3K"},
	}
}

// countingProvider answers at once and counts the model calls and what it was shown.
type countingProvider struct {
	mu    sync.Mutex
	calls int
	seen  []string
}

func (p *countingProvider) Chat(_ context.Context, msgs []providers.Message, _ []providers.ToolDefinition, _ string, _ map[string]any) (*providers.LLMResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	for _, m := range msgs {
		p.seen = append(p.seen, m.Content)
	}
	return &providers.LLMResponse{Content: "ok"}, nil
}
func (p *countingProvider) GetDefaultModel() string { return "count-model" }

func (p *countingProvider) snapshot() (int, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls, strings.Join(p.seen, "\n")
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
