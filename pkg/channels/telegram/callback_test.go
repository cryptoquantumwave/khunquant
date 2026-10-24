package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mymmrac/telego"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cryptoquantumwave/khunquant/pkg/agent"
	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/channels"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/control"
	"github.com/cryptoquantumwave/khunquant/pkg/providers"
)

const (
	ownerID   = "1001"
	otherID   = "2002"
	cardMsgID = 55
)

// ---------------------------------------------------------------------------
// A Telegram Bot API stub. A real telego bot talks to it over HTTP (base_url),
// so what is checked is what would go on the wire: the requests the channel
// makes, and the updates it polls.
// ---------------------------------------------------------------------------

type apiCall struct {
	Method string
	Body   map[string]any
}

type botStub struct {
	t   *testing.T
	srv *httptest.Server

	mu      sync.Mutex
	calls   []apiCall
	nextMsg int

	updates chan map[string]any

	// failEdits makes every editMessageText fail, as for a message that can no
	// longer be edited. Set it before the channel starts polling.
	failEdits bool
}

func newBotStub(t *testing.T) *botStub {
	t.Helper()
	b := &botStub{t: t, nextMsg: 900, updates: make(chan map[string]any, 32)}
	b.srv = httptest.NewServer(http.HandlerFunc(b.serve))
	t.Cleanup(b.srv.Close)
	return b
}

func (b *botStub) serve(w http.ResponseWriter, r *http.Request) {
	idx := strings.LastIndex(r.URL.Path, "/")
	method := r.URL.Path[idx+1:]
	raw, _ := io.ReadAll(r.Body)
	body := map[string]any{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &body)
	}

	if method == "getUpdates" {
		b.record(method, body)
		select {
		case u := <-b.updates:
			writeResult(w, []map[string]any{u})
		case <-time.After(50 * time.Millisecond):
			writeResult(w, []map[string]any{})
		case <-r.Context().Done():
		}
		return
	}

	b.mu.Lock()
	b.calls = append(b.calls, apiCall{Method: method, Body: body})
	b.nextMsg++
	id := b.nextMsg
	failEdits := b.failEdits
	b.mu.Unlock()

	if failEdits && method == "editMessageText" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: message can't be edited"}`))
		return
	}

	switch method {
	case "getMe":
		writeResult(w, map[string]any{"id": 777, "is_bot": true, "first_name": "kq", "username": "kqbot"})
	case "sendMessage", "editMessageText":
		chat := map[string]any{"id": 1, "type": "private"}
		if c, ok := body["chat_id"].(float64); ok {
			chat["id"] = c
		}
		mid := id
		if m, ok := body["message_id"].(float64); ok && method == "editMessageText" {
			mid = int(m)
		}
		writeResult(w, map[string]any{"message_id": mid, "date": 1, "chat": chat})
	case "getMyCommands":
		writeResult(w, []any{})
	default:
		writeResult(w, true)
	}
}

func (b *botStub) record(method string, body map[string]any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, apiCall{Method: method, Body: body})
}

func writeResult(w http.ResponseWriter, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
}

func (b *botStub) callsTo(method string) []apiCall {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []apiCall
	for _, c := range b.calls {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

func (b *botStub) waitCalls(method string, n int) []apiCall {
	b.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got := b.callsTo(method); len(got) >= n {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	b.t.Fatalf("waited for %d %s call(s), saw %d", n, method, len(b.callsTo(method)))
	return nil
}

// settle waits until no call has been made for a moment, so a test can assert
// that something did NOT happen.
func (b *botStub) settle() {
	last := -1
	for i := 0; i < 100; i++ {
		b.mu.Lock()
		n := 0
		for _, c := range b.calls {
			if c.Method != "getUpdates" {
				n++
			}
		}
		b.mu.Unlock()
		if n == last {
			return
		}
		last = n
		time.Sleep(60 * time.Millisecond)
	}
}

// tap is the callback_query update of a button press on the card message.
type tap struct {
	queryID  string
	from     int64
	chatType string // "private" (default) or "supergroup"
	chatID   int64
	msgID    int
	threadID int
	forum    bool
	data     string
	// inaccessible makes the card a message the bot can no longer read (date 0).
	inaccessible bool
	// noMessage drops the message, as for a button on an inline-mode message.
	noMessage bool
}

func (tp tap) update(updateID int) map[string]any {
	if tp.chatType == "" {
		tp.chatType = "private"
	}
	if tp.msgID == 0 {
		tp.msgID = cardMsgID
	}
	chat := map[string]any{"id": tp.chatID, "type": tp.chatType}
	if tp.forum {
		chat["is_forum"] = true
	}
	msg := map[string]any{"message_id": tp.msgID, "date": 1700000000, "chat": chat, "text": "card"}
	if tp.inaccessible {
		msg = map[string]any{"message_id": tp.msgID, "date": 0, "chat": chat}
	}
	if tp.threadID != 0 {
		msg["message_thread_id"] = tp.threadID
	}
	cq := map[string]any{
		"id":            tp.queryID,
		"from":          map[string]any{"id": tp.from, "is_bot": false, "first_name": "U"},
		"chat_instance": "ci",
		"data":          tp.data,
	}
	if !tp.noMessage {
		cq["message"] = msg
	}
	return map[string]any{"update_id": updateID, "callback_query": cq}
}

func textUpdate(updateID int, from int64, text string) map[string]any {
	return map[string]any{"update_id": updateID, "message": map[string]any{
		"message_id": 70 + updateID, "date": 1700000000, "text": text,
		"chat": map[string]any{"id": from, "type": "private"},
		"from": map[string]any{"id": from, "is_bot": false, "first_name": "U"},
	}}
}

// ---------------------------------------------------------------------------
// The rig: a real TelegramChannel (built by NewTelegramChannel, started, polling
// the stub), with a router set on it.
// ---------------------------------------------------------------------------

type rig struct {
	t    *testing.T
	api  *botStub
	ch   *TelegramChannel
	mb   *bus.MessageBus
	cfg  *config.Config
	next int
}

type rigOpts struct {
	allow  []string
	router channels.ControlRouter
	// noStart builds the channel and marks it running without polling.
	noStart bool
	// learning turns proactive.learning on (reactions).
	learning bool
}

func ownerConfig(stub *botStub, allow []string) *config.Config {
	cfg := config.DefaultConfig()
	cfg.Channels.Telegram.Token.Set(testToken)
	cfg.Channels.Telegram.BaseURL = stub.srv.URL
	cfg.Channels.Telegram.AllowFrom = allow
	cfg.Channels.Telegram.PairingEnabled = false // the default config turns it on; one test turns it on again
	cfg.Proactive.Enabled = true
	cfg.Proactive.Owner = config.OwnerConfig{Channel: "telegram", ChatID: ownerID, SenderID: "telegram:" + ownerID}
	cfg.Proactive.Approvals = config.ApprovalsConfig{Enabled: true, ExpiryMinutes: 10}
	return cfg
}

func newRig(t *testing.T, o rigOpts) *rig {
	t.Helper()
	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		t.Setenv(k, "")
	}
	stub := newBotStub(t)
	cfg := ownerConfig(stub, o.allow)
	cfg.Proactive.Learning.Enabled = o.learning
	mb := bus.NewMessageBus()
	ch, err := NewTelegramChannel(cfg, mb)
	require.NoError(t, err)
	if o.router != nil {
		ch.SetControlRouter(o.router)
	}
	r := &rig{t: t, api: stub, ch: ch, mb: mb, cfg: cfg}
	if o.noStart {
		ch.SetRunning(true)
		return r
	}
	require.NoError(t, ch.Start(context.Background()))
	t.Cleanup(func() { _ = ch.Stop(context.Background()) })
	return r
}

// press queues a tap and returns once the channel has answered it.
func (r *rig) press(tp tap) {
	r.t.Helper()
	r.next++
	if tp.queryID == "" {
		tp.queryID = fmt.Sprintf("q%d", r.next)
	}
	before := len(r.api.callsTo("answerCallbackQuery"))
	r.api.updates <- tp.update(r.next)
	r.api.waitCalls("answerCallbackQuery", before+1)
}

func (r *rig) lastAnswer() apiCall {
	r.t.Helper()
	a := r.api.callsTo("answerCallbackQuery")
	require.NotEmpty(r.t, a)
	return a[len(a)-1]
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
	if sender != "telegram:"+ownerID {
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
	if approve {
		p.status = approval.StatusApproved
	}
	st := approval.StatusExecuting
	if !approve {
		st = approval.StatusRejected
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

func realDispatcher(cfg *config.Config, appr control.Approver) *control.Dispatcher {
	return control.New(control.Deps{Cfg: cfg, Approvals: appr})
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

const (
	approveData = "kq1|apv|7F3K.abcd1234"
	rejectData  = "kq1|rej|7F3K.abcd1234"
)

func toastOf(c apiCall) string { s, _ := c.Body["text"].(string); return s }

// ---------------------------------------------------------------------------
// Sending: the keyboard.
// ---------------------------------------------------------------------------

func approvalActions() []bus.Action {
	return []bus.Action{
		{ID: "approve", Label: "Approve", Style: "primary", Payload: "7F3K.abcd1234", TextCommand: "/approve 7F3K"},
		{ID: "reject", Label: "Reject", Style: "danger", Payload: "7F3K.abcd1234", TextCommand: "/reject 7F3K"},
	}
}

func keyboardOf(t *testing.T, c apiCall) [][]map[string]any {
	t.Helper()
	rm, ok := c.Body["reply_markup"].(map[string]any)
	if !ok {
		return nil
	}
	var rows [][]map[string]any
	for _, row := range rm["inline_keyboard"].([]any) {
		var r []map[string]any
		for _, b := range row.([]any) {
			r = append(r, b.(map[string]any))
		}
		rows = append(rows, r)
	}
	return rows
}

var _ channels.ActionRenderer = (*TelegramChannel)(nil)

func TestTelegram_SupportsActions(t *testing.T) {
	r := newRig(t, rigOpts{noStart: true})
	assert.True(t, r.ch.SupportsActions())
}

func TestTelegram_KeyboardOnLastChunk(t *testing.T) {
	r := newRig(t, rigOpts{noStart: true})

	t.Run("one message carries the keyboard and keeps the text fallback", func(t *testing.T) {
		_, err := r.ch.SendWithIDs(context.Background(), bus.OutboundMessage{
			ChatID: ownerID, Content: "Proposal 7F3K", Actions: approvalActions(),
		})
		require.NoError(t, err)
		calls := r.api.callsTo("sendMessage")
		require.Len(t, calls, 1)
		rows := keyboardOf(t, calls[0])
		require.Len(t, rows, 1)
		require.Len(t, rows[0], 2)
		assert.Equal(t, "Approve", rows[0][0]["text"])
		assert.Equal(t, approveData, rows[0][0]["callback_data"])
		assert.Equal(t, "primary", rows[0][0]["style"])
		assert.Equal(t, "Reject", rows[0][1]["text"])
		assert.Equal(t, rejectData, rows[0][1]["callback_data"])
		assert.Equal(t, "danger", rows[0][1]["style"])
		text, _ := calls[0].Body["text"].(string)
		assert.Contains(t, text, "/approve 7F3K")
		assert.Contains(t, text, "/reject 7F3K")
	})

	t.Run("a message split in several has the keyboard on the last one only", func(t *testing.T) {
		before := len(r.api.callsTo("sendMessage"))
		// 1500 "& " expands to far more than 4096 runes of HTML, so the adapter splits.
		_, err := r.ch.SendWithIDs(context.Background(), bus.OutboundMessage{
			ChatID: ownerID, Content: strings.Repeat("& ", 1500), Actions: approvalActions(),
		})
		require.NoError(t, err)
		calls := r.api.callsTo("sendMessage")[before:]
		require.Greater(t, len(calls), 1, "the fixture should have forced several messages")
		for i, c := range calls[:len(calls)-1] {
			assert.NotContains(t, c.Body, "reply_markup", "message %d of %d", i+1, len(calls))
		}
		last := calls[len(calls)-1]
		require.Len(t, keyboardOf(t, last), 1)
		lastText, _ := last.Body["text"].(string)
		assert.Contains(t, lastText, "/approve 7F3K", "the text fallback ends the last message")
	})

	t.Run("at most three buttons to a row", func(t *testing.T) {
		before := len(r.api.callsTo("sendMessage"))
		acts := append(approvalActions(),
			bus.Action{ID: "snooze_1h", Label: "1h", Payload: "01HZ", TextCommand: "/snooze 1h"},
			bus.Action{ID: "snooze_4h", Label: "4h", Payload: "01HZ", TextCommand: "/snooze 4h"},
			bus.Action{ID: "snooze_1d", Label: "1d", Payload: "01HZ", TextCommand: "/snooze 1d"},
			bus.Action{ID: "useful", Label: "Useful", Payload: "01HZ"},
			bus.Action{ID: "not_useful", Label: "Not useful", Payload: "01HZ"},
		)
		_, err := r.ch.SendWithIDs(context.Background(), bus.OutboundMessage{ChatID: ownerID, Content: "x", Actions: acts})
		require.NoError(t, err)
		rows := keyboardOf(t, r.api.callsTo("sendMessage")[before])
		var sizes []int
		for _, row := range rows {
			sizes = append(sizes, len(row))
		}
		assert.Equal(t, []int{3, 3, 1}, sizes)
		assert.Equal(t, "kq1|snz1d|01HZ", rows[1][1]["callback_data"])
		assert.Equal(t, "kq1|nu|01HZ", rows[2][0]["callback_data"])
	})

	t.Run("a forum topic keeps its thread", func(t *testing.T) {
		before := len(r.api.callsTo("sendMessage"))
		_, err := r.ch.SendWithIDs(context.Background(), bus.OutboundMessage{
			ChatID: "-100123/7", Content: "x", Actions: approvalActions(),
		})
		require.NoError(t, err)
		c := r.api.callsTo("sendMessage")[before]
		assert.EqualValues(t, 7, c.Body["message_thread_id"])
		assert.EqualValues(t, -100123, c.Body["chat_id"])
		assert.Len(t, keyboardOf(t, c), 1)
	})

	t.Run("a message without actions has no keyboard and no footer", func(t *testing.T) {
		before := len(r.api.callsTo("sendMessage"))
		_, err := r.ch.SendWithIDs(context.Background(), bus.OutboundMessage{ChatID: ownerID, Content: "plain"})
		require.NoError(t, err)
		c := r.api.callsTo("sendMessage")[before]
		assert.NotContains(t, c.Body, "reply_markup")
		assert.Equal(t, "plain", c.Body["text"])
	})
}

// A button that cannot be encoded is a programming error. The message still goes
// out, without any keyboard, with the commands in its text.
func TestTelegram_KeyboardRefusedForBadAction(t *testing.T) {
	r := newRig(t, rigOpts{noStart: true})
	for name, bad := range map[string]bus.Action{
		"payload over the limit": {ID: "snooze_1h", Label: "1h", Payload: strings.Repeat("A", 49), TextCommand: "/snooze 1h"},
		"unknown action":         {ID: "launch_missiles", Label: "Go", Payload: "X", TextCommand: "/go"},
		"payload with a pipe":    {ID: "snooze_1h", Label: "1h", Payload: "a|b", TextCommand: "/snooze 1h"},
		"approval without nonce": {ID: "approve", Label: "Approve", Payload: "7F3K", TextCommand: "/approve 7F3K"},
	} {
		t.Run(name, func(t *testing.T) {
			before := len(r.api.callsTo("sendMessage"))
			acts := append(approvalActions(), bad)
			ids, err := r.ch.SendWithIDs(context.Background(), bus.OutboundMessage{ChatID: ownerID, Content: "card", Actions: acts})
			require.NoError(t, err)
			require.Len(t, ids, 1)
			calls := r.api.callsTo("sendMessage")[before:]
			require.Len(t, calls, 1)
			assert.NotContains(t, calls[0].Body, "reply_markup", "all of the buttons or none")
			text, _ := calls[0].Body["text"].(string)
			assert.Contains(t, text, "/approve 7F3K")
		})
	}
}

func TestCallbackData_Max64Bytes(t *testing.T) {
	longest := strings.Repeat("A", bus.MaxActionPayloadBytes)
	for id, code := range callbackCodes {
		payload := longest
		if code == "apv" || code == "rej" {
			payload = longest[:20] + "." + longest[:bus.MaxActionPayloadBytes-21]
		}
		data, err := encodeCallbackData(bus.Action{ID: id, Payload: payload})
		require.NoError(t, err, id)
		assert.LessOrEqual(t, len(data), maxCallbackDataBytes, "%s: %q", id, data)

		gotID, gotPayload, err := parseCallbackData(data)
		require.NoError(t, err, id)
		assert.Equal(t, id, gotID)
		assert.Equal(t, payload, gotPayload)

		_, err = encodeCallbackData(bus.Action{ID: id, Payload: payload + "A"})
		assert.Error(t, err, "%s: one byte over the payload limit is refused, not cut", id)
	}

	// The bound is on the whole string, and the parser holds it too.
	_, _, err := parseCallbackData("kq1|snz1h|" + strings.Repeat("A", maxCallbackDataBytes))
	assert.Error(t, err)

	// What the real cards send fits.
	for _, a := range approvalActions() {
		data, err := encodeCallbackData(a)
		require.NoError(t, err)
		assert.LessOrEqual(t, len(data), maxCallbackDataBytes)
	}
	// A 26-character intervention id fits every action.
	for id := range callbackCodes {
		if id == "approve" || id == "reject" {
			continue
		}
		data, err := encodeCallbackData(bus.Action{ID: id, Payload: "01HZX3K8V0F9Q2M4N6P8R0S2T4"})
		require.NoError(t, err, id)
		assert.LessOrEqual(t, len(data), maxCallbackDataBytes)
	}
}

func TestCallbackData_ParseIsStrict(t *testing.T) {
	good := map[string][2]string{
		approveData:                 {"approve", "7F3K.abcd1234"},
		rejectData:                  {"reject", "7F3K.abcd1234"},
		"kq1|snz1h|01HZX3K8V0F9Q2":  {"snooze_1h", "01HZX3K8V0F9Q2"},
		"kq1|snz4h|":                {"snooze_4h", ""},
		"kq1|snz1d|id":              {"snooze_1d", "id"},
		"kq1|use|id":                {"useful", "id"},
		"kq1|nu|id":                 {"not_useful", "id"},
		"kq1|use|01HZ.x-y_z:1":      {"useful", "01HZ.x-y_z:1"},
		"kq1|apv|AAAA.BBBBBBBBBBBB": {"approve", "AAAA.BBBBBBBBBBBB"},
	}
	for data, want := range good {
		id, payload, err := parseCallbackData(data)
		require.NoError(t, err, data)
		assert.Equal(t, want[0], id, data)
		assert.Equal(t, want[1], payload, data)
	}
	for _, data := range []string{
		"",
		"kq1",
		"kq1|apv",
		"kq2|apv|7F3K.abcd1234",
		"KQ1|apv|7F3K.abcd1234",
		"kq1|ci|7F3K.abcd1234", // confirm-intent is not handled by the control path
		"kq1|approve|7F3K.abcd1234",
		"kq1|APV|7F3K.abcd1234",
		"kq1|apv|7F3K",
		"kq1|apv|7F3K.",
		"kq1|apv|.abcd1234",
		"kq1|apv|7F3K.ab.cd",
		"kq1|apv|7F3K.abcd1234|x",
		"kq1|apv||7F3K.abcd1234",
		"kq1|snz1h|has space",
		"kq1|snz1h|new\nline",
		"kq1|snz1h|über",
		" kq1|snz1h|id",
		"kq1|snz1h|" + strings.Repeat("A", bus.MaxActionPayloadBytes+1),
	} {
		_, _, err := parseCallbackData(data)
		assert.Error(t, err, "%q must be dropped", data)
	}
}

func TestAllowedUpdatesSet(t *testing.T) {
	assert.Equal(t, []string{"message", "callback_query"}, allowedUpdates(false))
	assert.Equal(t, []string{"message", "callback_query", "message_reaction"}, allowedUpdates(true))
	// A fresh slice each time: telego keeps the one it is given.
	a := allowedUpdates(false)
	a[0] = "x"
	assert.Equal(t, "message", allowedUpdates(false)[0])

	// And it is what goes on the wire: with learning off exactly what it was before
	// reactions existed, with it on the reaction updates as well.
	r := newRig(t, rigOpts{})
	polls := r.api.waitCalls("getUpdates", 1)
	assert.Equal(t, []any{"message", "callback_query"}, polls[0].Body["allowed_updates"])

	r = newRig(t, rigOpts{learning: true})
	polls = r.api.waitCalls("getUpdates", 1)
	assert.Equal(t, []any{"message", "callback_query", "message_reaction"}, polls[0].Body["allowed_updates"])
}

// ---------------------------------------------------------------------------
// Receiving: taps.
// ---------------------------------------------------------------------------

func TestTelegram_CallbackRoutesToControl(t *testing.T) {
	appr := newFakeApprover("7F3K", "abcd1234")
	r := newRigWithApprover(t, appr, nil)

	r.press(tap{from: 1001, chatID: 1001, data: approveData, queryID: "tap-1"})

	got := appr.decisions()
	require.Len(t, got, 1)
	assert.Equal(t, decision{Code: "7F3K", Nonce: "abcd1234", Sender: "telegram:1001", Approve: true}, got[0])
	assert.Equal(t, approval.StatusApproved, appr.statusOf("7F3K"))

	ans := r.lastAnswer()
	assert.Equal(t, "tap-1", ans.Body["callback_query_id"])
	assert.Contains(t, toastOf(ans), "Approved proposal 7F3K")

	// The full reply (the toast shows its first line only) follows as a reply to the card.
	sent := r.api.waitCalls("sendMessage", 1)[0]
	text, _ := sent.Body["text"].(string)
	assert.Contains(t, text, "Placing the order now")
	rp, _ := sent.Body["reply_parameters"].(map[string]any)
	assert.EqualValues(t, cardMsgID, rp["message_id"])
	assert.EqualValues(t, 1001, sent.Body["chat_id"])

	// Reject and a snooze-shaped action take the same road.
	appr2 := newFakeApprover("9ZZZ", "n0nce000")
	r2 := newRigWithApprover(t, appr2, nil)
	r2.press(tap{from: 1001, chatID: 1001, data: "kq1|rej|9ZZZ.n0nce000"})
	require.Len(t, appr2.decisions(), 1)
	assert.False(t, appr2.decisions()[0].Approve)
	assert.Contains(t, toastOf(r2.lastAnswer()), "Rejected proposal 9ZZZ")
	assert.Equal(t, approval.StatusRejected, appr2.statusOf("9ZZZ"))
}

// newRigWithApprover is a rig whose router is the real control.Dispatcher.
func newRigWithApprover(t *testing.T, appr control.Approver, allow []string) *rig {
	t.Helper()
	r := newRig(t, rigOpts{allow: allow})
	r.ch.SetControlRouter(realDispatcher(r.cfg, appr))
	return r
}

func TestTelegram_CallbackBuildsTheControlInput(t *testing.T) {
	router := &scriptedRouter{fn: func(channels.ControlInput) (channels.ControlReply, bool) {
		return channels.ControlReply{Text: "ok", AnswerCallback: "ok"}, true
	}}
	r := newRig(t, rigOpts{router: router})
	r.press(tap{from: 1001, chatID: 1001, data: "kq1|snz4h|01HZ", queryID: "cbq-9"})

	in := router.inputs()
	require.Len(t, in, 1)
	assert.Equal(t, "telegram", in[0].Channel)
	assert.Equal(t, "1001", in[0].ChatID)
	assert.Equal(t, "telegram:1001", in[0].SenderID)
	assert.Equal(t, "telegram:1001", in[0].Sender.CanonicalID)
	assert.Equal(t, "direct", in[0].PeerKind)
	assert.Equal(t, "55", in[0].MessageID)
	assert.Equal(t, "55", in[0].ReplyToMessageID, "for a button, the message it was on")
	assert.Empty(t, in[0].Text)
	require.NotNil(t, in[0].Callback)
	assert.Equal(t, channels.ControlCallback{Action: "snooze_4h", Payload: "01HZ", CallbackID: "cbq-9"}, *in[0].Callback)
}

func TestTelegram_CallbackNonOwnerToastDenied(t *testing.T) {
	t.Run("allow-listed, but not the owner", func(t *testing.T) {
		appr := newFakeApprover("7F3K", "abcd1234")
		r := newRigWithApprover(t, appr, []string{ownerID, otherID})
		r.press(tap{from: 2002, chatID: 2002, data: approveData})

		assert.Equal(t, "Not authorized.", toastOf(r.lastAnswer()))
		assert.Empty(t, appr.decisions(), "the approval service was never asked")
		assert.Equal(t, approval.StatusPending, appr.statusOf("7F3K"))
		r.api.settle()
		assert.Empty(t, r.api.callsTo("editMessageText"))
	})

	t.Run("not on the allow-list at all", func(t *testing.T) {
		router := &scriptedRouter{fn: func(channels.ControlInput) (channels.ControlReply, bool) {
			return channels.ControlReply{Text: "should not happen"}, true
		}}
		r := newRig(t, rigOpts{allow: []string{ownerID}, router: router})
		// Valid data, malformed data and garbage all get the same answer.
		for _, data := range []string{approveData, "kq1|bogus|x", "junk", ""} {
			r.press(tap{from: 2002, chatID: 2002, data: data})
			assert.Equal(t, "Not authorized.", toastOf(r.lastAnswer()), "data %q", data)
		}
		assert.Empty(t, router.inputs(), "the control path never saw it")
		r.api.settle()
		assert.Empty(t, r.api.callsTo("sendMessage"), "nothing else is said")
		assert.Empty(t, r.api.callsTo("editMessageText"))
	})

	t.Run("pairing on and nobody approved yet: deny-all, as for messages", func(t *testing.T) {
		router := &scriptedRouter{}
		stub := newBotStub(t)
		t.Setenv("HTTP_PROXY", "")
		cfg := ownerConfig(stub, nil)
		cfg.Channels.Telegram.PairingEnabled = true
		ch, err := NewTelegramChannel(cfg, bus.NewMessageBus())
		require.NoError(t, err)
		ch.SetControlRouter(router)
		require.NoError(t, ch.Start(context.Background()))
		t.Cleanup(func() { _ = ch.Stop(context.Background()) })

		stub.updates <- tap{queryID: "p1", from: 1001, chatID: 1001, data: approveData}.update(1)
		stub.waitCalls("answerCallbackQuery", 1)
		assert.Equal(t, "Not authorized.", toastOf(stub.callsTo("answerCallbackQuery")[0]))
		assert.Empty(t, router.inputs())
		stub.settle()
		assert.Empty(t, stub.callsTo("sendMessage"), "no pairing request is started by a tap")
	})
}

func TestTelegram_ForgedPayloadIsRefusedByDecide(t *testing.T) {
	appr := newFakeApprover("7F3K", "abcd1234")
	r := newRigWithApprover(t, appr, nil)

	// A valid code with a nonce that is not the proposal's.
	r.press(tap{from: 1001, chatID: 1001, data: "kq1|apv|7F3K.deadbeef"})
	require.Len(t, appr.decisions(), 1, "it reached Decide, which is where it is refused")
	assert.Equal(t, "deadbeef", appr.decisions()[0].Nonce)
	assert.Equal(t, approval.StatusPending, appr.statusOf("7F3K"), "nothing was decided")
	assert.Contains(t, toastOf(r.lastAnswer()), "No pending proposal 7F3K")

	// The real nonce still works afterwards: the forgery cost the owner nothing.
	r.press(tap{from: 1001, chatID: 1001, data: approveData})
	assert.Equal(t, approval.StatusApproved, appr.statusOf("7F3K"))
}

func TestTelegram_DoubleTapApprovesOnce(t *testing.T) {
	t.Run("one after the other", func(t *testing.T) {
		appr := newFakeApprover("7F3K", "abcd1234")
		r := newRigWithApprover(t, appr, nil)
		r.press(tap{from: 1001, chatID: 1001, data: approveData})
		r.press(tap{from: 1001, chatID: 1001, data: approveData})

		require.Len(t, appr.decisions(), 2)
		assert.Equal(t, approval.StatusApproved, appr.statusOf("7F3K"))
		answers := r.api.callsTo("answerCallbackQuery")
		require.Len(t, answers, 2)
		assert.Contains(t, toastOf(answers[0]), "Approved proposal 7F3K")
		assert.Contains(t, toastOf(answers[1]), "already approved")
	})

	t.Run("approve then reject: the first decision stands", func(t *testing.T) {
		appr := newFakeApprover("7F3K", "abcd1234")
		r := newRigWithApprover(t, appr, nil)
		r.press(tap{from: 1001, chatID: 1001, data: approveData})
		r.press(tap{from: 1001, chatID: 1001, data: rejectData})
		assert.Equal(t, approval.StatusApproved, appr.statusOf("7F3K"))
		assert.Contains(t, toastOf(r.lastAnswer()), "already approved")
	})
}

func TestTelegram_EditsCardAfterDecision(t *testing.T) {
	router := &scriptedRouter{fn: func(channels.ControlInput) (channels.ControlReply, bool) {
		return channels.ControlReply{
			Text: "Approved proposal 7F3K: sell 0.01 BTC on okx", EditOriginal: true,
			AnswerCallback: "Approved",
		}, true
	}}
	r := newRig(t, rigOpts{router: router})
	r.press(tap{from: 1001, chatID: 1001, data: approveData})

	assert.Equal(t, "Approved", toastOf(r.lastAnswer()))
	edits := r.api.waitCalls("editMessageText", 1)
	require.Len(t, edits, 1)
	assert.EqualValues(t, 1001, edits[0].Body["chat_id"])
	assert.EqualValues(t, cardMsgID, edits[0].Body["message_id"])
	assert.Contains(t, edits[0].Body["text"], "Approved proposal 7F3K")
	assert.NotContains(t, edits[0].Body, "reply_markup", "no reply_markup on an edit removes the keyboard")
	r.api.settle()
	assert.Empty(t, r.api.callsTo("sendMessage"), "the card was edited, not answered with a new message")

	t.Run("a card that cannot be edited gets the reply as a message", func(t *testing.T) {
		stub := newBotStub(t)
		t.Setenv("HTTP_PROXY", "")
		cfg := ownerConfig(stub, nil)
		ch, err := NewTelegramChannel(cfg, bus.NewMessageBus())
		require.NoError(t, err)
		stub.failEdits = true // an API that refuses every edit
		ch.SetControlRouter(router)
		require.NoError(t, ch.Start(context.Background()))
		t.Cleanup(func() { _ = ch.Stop(context.Background()) })

		stub.updates <- tap{queryID: "e1", from: 1001, chatID: 1001, data: approveData}.update(1)
		stub.waitCalls("answerCallbackQuery", 1)
		sent := stub.waitCalls("sendMessage", 1)
		assert.Contains(t, sent[0].Body["text"], "Approved proposal 7F3K")
	})
}

func TestTelegram_ForumTopicChatIDRoundTrips(t *testing.T) {
	router := &scriptedRouter{fn: func(in channels.ControlInput) (channels.ControlReply, bool) {
		return channels.ControlReply{Text: "Rejected: first line\nsecond line", AnswerCallback: "Rejected"}, true
	}}
	r := newRig(t, rigOpts{router: router})

	r.press(tap{from: 1001, chatID: -100123, chatType: "supergroup", forum: true, threadID: 7, data: rejectData})
	in := router.inputs()
	require.Len(t, in, 1)
	assert.Equal(t, "-100123/7", in[0].ChatID, "chatID/threadID, as the message path forms it")
	assert.Equal(t, "group", in[0].PeerKind)

	// The reply goes back into the topic.
	sent := r.api.waitCalls("sendMessage", 1)[0]
	assert.EqualValues(t, -100123, sent.Body["chat_id"])
	assert.EqualValues(t, 7, sent.Body["message_thread_id"])

	// The same id the message path builds for a message in that topic.
	mr := newRig(t, rigOpts{router: &recordingRouter{}, noStart: true})
	mr.ch.ctx = context.Background()
	require.NoError(t, mr.ch.handleMessage(context.Background(), &telego.Message{
		Text: "hi", MessageID: 9, MessageThreadID: 7,
		Chat: telego.Chat{ID: -100123, Type: "supergroup", IsForum: true},
		From: &telego.User{ID: 1001, FirstName: "A"},
	}))
	rr := mr.ch.ControlRouter().(*recordingRouter)
	require.Len(t, rr.got, 1)
	assert.Equal(t, in[0].ChatID, rr.got[0].ChatID)

	t.Run("a group that is not a forum has no thread", func(t *testing.T) {
		router.mu.Lock()
		router.got = nil
		router.mu.Unlock()
		r.press(tap{from: 1001, chatID: -100456, chatType: "supergroup", threadID: 3, data: rejectData})
		in := router.inputs()
		require.Len(t, in, 1)
		assert.Equal(t, "-100456", in[0].ChatID, "a reply thread in an ordinary group shares the group's session")
	})
}

func TestTelegram_CallbackInGroupFollowsTheGroupRule(t *testing.T) {
	appr := newFakeApprover("7F3K", "abcd1234")
	r := newRigWithApprover(t, appr, nil)
	// The owner, tapping in a group.
	r.press(tap{from: 1001, chatID: -100123, chatType: "supergroup", data: approveData})
	assert.Empty(t, appr.decisions(), "an owner command does not work outside the owner chat")
	assert.Equal(t, approval.StatusPending, appr.statusOf("7F3K"))
	assert.Contains(t, toastOf(r.lastAnswer()), "Owner commands only work in your owner chat")
}

// Whatever happens to a tap, it is answered, once, with its own query id.
func TestTelegram_EveryCallbackIsAnswered(t *testing.T) {
	appr := newFakeApprover("7F3K", "abcd1234")
	handling := &scriptedRouter{}
	handling.fn = func(in channels.ControlInput) (channels.ControlReply, bool) {
		switch in.Callback.Action {
		case "snooze_1h":
			panic("boom")
		case "useful":
			return channels.ControlReply{}, false // a router that does not know it
		}
		return channels.ControlReply{Text: "fine", AnswerCallback: "fine"}, true
	}

	cases := []struct {
		name  string
		tap   tap
		toast string // a prefix; "" means no toast text
		route channels.ControlRouter
		allow []string
	}{
		{"handled", tap{from: 1001, chatID: 1001, data: "kq1|snz4h|x"}, "fine", handling, nil},
		{"router panics", tap{from: 1001, chatID: 1001, data: "kq1|snz1h|x"}, "That command could not be handled", handling, nil},
		{"router does not know it", tap{from: 1001, chatID: 1001, data: "kq1|use|x"}, "That button is not valid.", handling, nil},
		{"unknown version", tap{from: 1001, chatID: 1001, data: "kq9|apv|7F3K.abcd1234"}, "That button is not valid.", handling, nil},
		{"unknown action", tap{from: 1001, chatID: 1001, data: "kq1|zzz|x"}, "That button is not valid.", handling, nil},
		{"malformed payload", tap{from: 1001, chatID: 1001, data: "kq1|apv|7F3K"}, "That button is not valid.", handling, nil},
		{"no data", tap{from: 1001, chatID: 1001, data: ""}, "That button is not valid.", handling, nil},
		{"data too long", tap{from: 1001, chatID: 1001, data: "kq1|snz1h|" + strings.Repeat("a", 80)}, "That button is not valid.", handling, nil},
		{"no message", tap{from: 1001, chatID: 1001, data: "kq1|snz1h|x", noMessage: true}, "That button is not valid.", handling, nil},
		{"inaccessible message", tap{from: 1001, chatID: 1001, data: "kq1|snz4h|x", inaccessible: true}, "fine", handling, nil},
		{"not allow-listed", tap{from: 2002, chatID: 2002, data: "kq1|snz4h|x"}, "Not authorized.", handling, []string{ownerID}},
		{"no router", tap{from: 1001, chatID: 1001, data: "kq1|snz4h|x"}, "Buttons are not available right now.", nil, nil},
		{"real dispatcher, bad nonce", tap{from: 1001, chatID: 1001, data: "kq1|apv|7F3K.00000000"}, "No pending proposal", realDispatcher(ownerConfig(newBotStub(t), nil), appr), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, rigOpts{allow: tc.allow, router: tc.route})
			tc.tap.queryID = "id-" + strings.ReplaceAll(tc.name, " ", "-")
			r.press(tc.tap)
			r.api.settle()
			answers := r.api.callsTo("answerCallbackQuery")
			require.Len(t, answers, 1, "exactly one answer")
			assert.Equal(t, tc.tap.queryID, answers[0].Body["callback_query_id"])
			assert.True(t, strings.HasPrefix(toastOf(answers[0]), tc.toast), "toast %q, want prefix %q", toastOf(answers[0]), tc.toast)
			assert.LessOrEqual(t, len([]rune(toastOf(answers[0]))), maxToastRunes)
			select {
			case m := <-r.mb.InboundChan():
				t.Fatalf("a tap became an inbound message: %+v", m)
			default:
			}
		})
	}
}

func TestTelegram_ToastIsCutToTheAPILimit(t *testing.T) {
	router := &scriptedRouter{fn: func(channels.ControlInput) (channels.ControlReply, bool) {
		return channels.ControlReply{AnswerCallback: strings.Repeat("é", 500)}, true
	}}
	r := newRig(t, rigOpts{router: router})
	r.press(tap{from: 1001, chatID: 1001, data: "kq1|snz1h|x"})
	assert.Len(t, []rune(toastOf(r.lastAnswer())), maxToastRunes)
}

// ---------------------------------------------------------------------------
// No model ever sees a tap.
// ---------------------------------------------------------------------------

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

// The channel and a real AgentLoop share one bus. Taps of every kind (handled,
// refused, malformed, from a stranger) publish nothing for the agent, and the
// model is never called; the same channel does feed the agent an ordinary
// message, so the check can fail.
func TestTelegram_NoModelEverSeesACallback(t *testing.T) {
	stub := newBotStub(t)
	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		t.Setenv(k, "")
	}
	cfg := ownerConfig(stub, []string{ownerID, otherID})
	mb := bus.NewMessageBus()
	prov := &countingProvider{}

	al := agent.NewAgentLoop(&config.Config{
		Agents: config.AgentsConfig{Defaults: config.AgentDefaults{
			Workspace: t.TempDir(), Model: "count-model", MaxTokens: 4096, MaxToolIterations: 5,
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

	ch, err := NewTelegramChannel(cfg, mb)
	require.NoError(t, err)
	appr := newFakeApprover("7F3K", "abcd1234")
	ch.SetControlRouter(realDispatcher(cfg, appr))
	require.NoError(t, ch.Start(context.Background()))
	t.Cleanup(func() { _ = ch.Stop(context.Background()) })

	taps := []tap{
		{from: 1001, chatID: 1001, data: approveData},                // handled
		{from: 1001, chatID: 1001, data: approveData},                // already decided
		{from: 2002, chatID: 2002, data: approveData},                // allow-listed, not the owner
		{from: 9999, chatID: 9999, data: approveData},                // a stranger
		{from: 1001, chatID: 1001, data: "kq1|zzz|7F3K.abcd1234"},    // unknown action
		{from: 1001, chatID: 1001, data: "kq1|apv|7F3K"},             // malformed
		{from: 1001, chatID: 1001, data: "kq1|snz1h|7F3K abcd1234 "}, // junk payload
	}
	for i, tp := range taps {
		tp.queryID = fmt.Sprintf("t%d", i)
		stub.updates <- tp.update(100 + i)
	}
	stub.waitCalls("answerCallbackQuery", len(taps))
	time.Sleep(200 * time.Millisecond) // a (wrongly) queued message would be picked up by now

	calls, seen := prov.snapshot()
	assert.Zero(t, calls, "the model was called for a tap")
	assert.NotContains(t, seen, "7F3K")
	assert.Empty(t, mb.InboundChan(), "a tap was published as an inbound message")

	// Positive control: the same wiring does hand an ordinary message to the agent.
	stub.updates <- textUpdate(200, 1001, "how is the market today?")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if n, _ := prov.snapshot(); n > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	n, seen := prov.snapshot()
	assert.Equal(t, 1, n, "an ordinary message should reach the model")
	assert.Contains(t, seen, "how is the market today?")
}
