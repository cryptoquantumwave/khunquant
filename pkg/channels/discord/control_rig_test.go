package discord

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/stretchr/testify/require"

	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/channels"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/control"
	"github.com/cryptoquantumwave/khunquant/pkg/providers"
)

const (
	ownerID    = "100000000000000001"
	otherID    = "100000000000000002"
	ownerDM    = "200000000000000001" // the owner's DM channel
	guildChan  = "300000000000000001"
	guildID    = "400000000000000001"
	cardID     = "500000000000000001"
	approveCID = "kq1|apv|7F3K.abcd1234"
	rejectCID  = "kq1|rej|7F3K.abcd1234"
)

// ---------------------------------------------------------------------------
// A Discord REST stub, as the session's own HTTP client sees it: the requests
// the channel makes are what would go on the wire.
// ---------------------------------------------------------------------------

type dcStub struct {
	mu     sync.Mutex
	reqs   []recordedRequest
	nextID int
}

func (s *dcStub) RoundTrip(r *http.Request) (*http.Response, error) {
	raw, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.reqs = append(s.reqs, recordedRequest{r.Method, r.URL.Path, string(raw)})
	s.nextID++
	id := strconv.Itoa(9000 + s.nextID)
	s.mu.Unlock()
	status, body := http.StatusOK, `{"id":"`+id+`","channel_id":"c"}`
	if strings.HasSuffix(r.URL.Path, "/callback") {
		status, body = http.StatusNoContent, ""
	}
	return &http.Response{
		StatusCode: status, Status: http.StatusText(status),
		Header:  http.Header{"Content-Type": []string{"application/json"}},
		Body:    io.NopCloser(strings.NewReader(body)),
		Request: r,
	}, nil
}

func (s *dcStub) requests() []recordedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]recordedRequest(nil), s.reqs...)
}

// matching returns the requests whose method is method and whose path ends with
// (or, for an infix starting with "~", contains) the suffix.
func (s *dcStub) matching(method, suffix string) []recordedRequest {
	var out []recordedRequest
	for _, r := range s.requests() {
		if r.method != method {
			continue
		}
		if strings.HasPrefix(suffix, "~") && strings.Contains(r.path, suffix[1:]) || strings.HasSuffix(r.path, suffix) {
			out = append(out, r)
		}
	}
	return out
}

// callbacks returns the interaction callbacks, decoded.
func (s *dcStub) callbacks() []callback {
	var out []callback
	for _, r := range s.matching(http.MethodPost, "/callback") {
		var c callback
		_ = json.Unmarshal([]byte(r.body), &c)
		out = append(out, c)
	}
	return out
}

type callback struct {
	Type int `json:"type"`
	Data struct {
		Content    string            `json:"content"`
		Flags      int               `json:"flags"`
		Components *[]map[string]any `json:"components"`
	} `json:"data"`
}

// ---------------------------------------------------------------------------
// The rig: a real DiscordChannel whose session talks to the stub, running.
// ---------------------------------------------------------------------------

type rig struct {
	t    *testing.T
	api  *dcStub
	ch   *DiscordChannel
	mb   *bus.MessageBus
	cfg  *config.Config
	next int
}

func ownerConfig() *config.Config {
	cfg := config.DefaultConfig()
	cfg.Proactive.Enabled = true
	cfg.Proactive.Owner = config.OwnerConfig{Channel: "discord", ChatID: ownerDM, SenderID: "discord:" + ownerID}
	cfg.Proactive.Approvals = config.ApprovalsConfig{Enabled: true, ExpiryMinutes: 10}
	return cfg
}

func newRigWith(t *testing.T, mb *bus.MessageBus, allow []string, router channels.ControlRouter) *rig {
	t.Helper()
	session, err := discordgo.New("Bot test-token")
	require.NoError(t, err)
	stub := &dcStub{}
	session.Client = &http.Client{Transport: stub}
	session.State.User = &discordgo.User{ID: "999"}
	cfg := ownerConfig()
	base := channels.NewBaseChannel("discord", config.DiscordConfig{}, mb, allow,
		channels.WithMaxMessageLength(maxMessageLength))
	base.SetRunning(true)
	if router != nil {
		base.SetControlRouter(router)
	}
	ch := &DiscordChannel{
		BaseChannel: base, session: session, ctx: context.Background(),
		typingStop: map[string]chan struct{}{}, botUserID: "999",
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
	id, user, customID string
	guild              bool   // a tap in a guild channel (Member.User) rather than a DM (User)
	channel            string // default: the owner's DM, or the guild channel
	noMessage          bool
}

func (r *rig) interaction(tp tap) *discordgo.InteractionCreate {
	r.next++
	if tp.id == "" {
		tp.id = "int-" + strconv.Itoa(r.next)
	}
	i := &discordgo.Interaction{
		ID: tp.id, AppID: "app1", Token: "tok-" + tp.id, Type: discordgo.InteractionMessageComponent,
		Data: discordgo.MessageComponentInteractionData{CustomID: tp.customID, ComponentType: discordgo.ButtonComponent},
	}
	user := &discordgo.User{ID: tp.user, Username: "u" + tp.user}
	if tp.guild {
		i.GuildID = guildID
		i.ChannelID = guildChan
		i.Member = &discordgo.Member{User: user}
	} else {
		i.ChannelID = ownerDM
		i.User = user
	}
	if tp.channel != "" {
		i.ChannelID = tp.channel
	}
	if !tp.noMessage {
		i.Message = &discordgo.Message{ID: cardID}
	}
	return &discordgo.InteractionCreate{Interaction: i}
}

// press handles a tap to completion (the handler is synchronous).
func (r *rig) press(tp tap) {
	r.ch.handleInteraction(r.ch.session, r.interaction(tp))
}

func (r *rig) lastCallback() callback {
	r.t.Helper()
	c := r.api.callbacks()
	require.NotEmpty(r.t, c, "the interaction was not responded to")
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
	if sender != "discord:"+ownerID {
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
