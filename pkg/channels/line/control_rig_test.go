package line

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/channels"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/control"
	"github.com/cryptoquantumwave/khunquant/pkg/providers"
)

const (
	ownerID   = "Uowner0000000000000000000000000001"
	otherID   = "Uother0000000000000000000000000002"
	groupID   = "Cgroup0000000000000000000000000003"
	approveDt = "kq1|apv|7F3K.abcd1234"
	rejectDt  = "kq1|rej|7F3K.abcd1234"
)

// ---------------------------------------------------------------------------
// A LINE Messaging API stub, as the channel's own HTTP client sees it: the
// requests the channel makes are what would go on the wire.
// ---------------------------------------------------------------------------

type lineCall struct {
	Kind string // "reply" or "push"
	Body map[string]any
}

type lineStub struct {
	mu        sync.Mutex
	calls     []lineCall
	nextID    int
	failReply bool
}

func (s *lineStub) RoundTrip(r *http.Request) (*http.Response, error) {
	raw, _ := io.ReadAll(r.Body)
	body := map[string]any{}
	_ = json.Unmarshal(raw, &body)
	kind := "other"
	switch {
	case strings.HasSuffix(r.URL.Path, "/message/reply"):
		kind = "reply"
	case strings.HasSuffix(r.URL.Path, "/message/push"):
		kind = "push"
	}
	s.mu.Lock()
	s.calls = append(s.calls, lineCall{Kind: kind, Body: body})
	fail := kind == "reply" && s.failReply
	msgs, _ := body["messages"].([]any)
	var sent []map[string]string
	for range msgs {
		s.nextID++
		sent = append(sent, map[string]string{"id": fmt.Sprintf("M%d", s.nextID)})
	}
	s.mu.Unlock()
	status, payload := http.StatusOK, mustJSON(map[string]any{"sentMessages": sent})
	if fail {
		status, payload = http.StatusBadRequest, `{"message":"Invalid reply token"}`
	}
	return &http.Response{
		StatusCode: status, Status: http.StatusText(status),
		Header:  http.Header{"Content-Type": []string{"application/json"}},
		Body:    io.NopCloser(strings.NewReader(payload)),
		Request: r,
	}, nil
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func (s *lineStub) callsOf(kind string) []lineCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []lineCall
	for _, c := range s.calls {
		if c.Kind == kind {
			out = append(out, c)
		}
	}
	return out
}

func (s *lineStub) total() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

// messages returns the messages of a call.
func (c lineCall) messages() []map[string]any {
	var out []map[string]any
	list, _ := c.Body["messages"].([]any)
	for _, m := range list {
		out = append(out, m.(map[string]any))
	}
	return out
}

func (c lineCall) text() string {
	m := c.messages()
	if len(m) == 0 {
		return ""
	}
	s, _ := m[0]["text"].(string)
	return s
}

// ---------------------------------------------------------------------------
// The rig: a real LINEChannel (built by NewLINEChannel) whose HTTP client is the
// stub, running, with a context.
// ---------------------------------------------------------------------------

type rig struct {
	t    *testing.T
	api  *lineStub
	ch   *LINEChannel
	mb   *bus.MessageBus
	cfg  *config.Config
	next int
}

func ownerConfig() *config.Config {
	cfg := config.DefaultConfig()
	cfg.Proactive.Enabled = true
	cfg.Proactive.Owner = config.OwnerConfig{Channel: "line", ChatID: ownerID, SenderID: "line:" + ownerID}
	cfg.Proactive.Approvals = config.ApprovalsConfig{Enabled: true, ExpiryMinutes: 10}
	return cfg
}

func newRigWith(t *testing.T, mb *bus.MessageBus, allow []string, router channels.ControlRouter) *rig {
	t.Helper()
	cfg := ownerConfig()
	cfg.Channels.LINE.ChannelSecret.Set("secret")
	cfg.Channels.LINE.ChannelAccessToken.Set("token")
	cfg.Channels.LINE.AllowFrom = allow
	ch, err := NewLINEChannel(cfg.Channels.LINE, mb)
	require.NoError(t, err)
	stub := &lineStub{}
	ch.apiClient = &http.Client{Transport: stub}
	ch.ctx = context.Background()
	ch.SetRunning(true)
	if router != nil {
		ch.SetControlRouter(router)
	}
	return &rig{t: t, api: stub, ch: ch, mb: mb, cfg: cfg}
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

type tap struct {
	user, srcType, groupID, data string
	noReplyToken                 bool
}

func (r *rig) event(tp tap) lineEvent {
	r.next++
	if tp.srcType == "" {
		tp.srcType = "user"
	}
	ev := lineEvent{
		Type:           "postback",
		ReplyToken:     fmt.Sprintf("rt-%d", r.next),
		WebhookEventID: fmt.Sprintf("evt-%d", r.next),
		Source:         lineSource{Type: tp.srcType, UserID: tp.user},
		Postback:       &linePostback{Data: tp.data},
	}
	switch tp.srcType {
	case "group":
		ev.Source.GroupID = tp.groupID
	case "room":
		ev.Source.RoomID = tp.groupID
	}
	if tp.noReplyToken {
		ev.ReplyToken = ""
	}
	return ev
}

// press handles a tap to completion (processEvent is synchronous; the webhook is
// what runs it in a goroutine).
func (r *rig) press(tp tap) { r.ch.processEvent(r.event(tp)) }

func (r *rig) lastReplyText() string {
	r.t.Helper()
	c := r.api.callsOf("reply")
	require.NotEmpty(r.t, c, "no reply was sent")
	return c[len(c)-1].text()
}

// ---------------------------------------------------------------------------
// Control-path fakes.
// ---------------------------------------------------------------------------

// fakeApprover mimics approval.Service.Decide for the cases that matter here:
// the owner check, a proposal only while pending, one winner per proposal, and
// the wrong nonce answering as "not found".
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
	if sender != "line:"+ownerID {
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
