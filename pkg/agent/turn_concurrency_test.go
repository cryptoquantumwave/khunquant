package agent

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/cron"
	"github.com/cryptoquantumwave/khunquant/pkg/providers"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

// Tests for turns that run alongside each other: per-turn message state,
// cancellation, the per-session lock and the turn registry.
//
// They drive real turns through the public entry points on a real bus and a
// real session store, with only the LLM provider faked. Every wait is on a
// signal from the fake provider or a fake tool; the few places that have to
// observe a state with no signal of its own ("a second turn is now blocked on
// the lock") poll that state against a deadline rather than sleeping a fixed
// time. Run them with -race.

const testWait = 5 * time.Second

// ====================== scaffolding ======================

// registerTestTurn registers a hand-built root turn for sessionKey the way
// runAgentLoop registers a real one, and unregisters it when the test ends.
func registerTestTurn(t *testing.T, al *AgentLoop, sessionKey string, ts *turnState) {
	t.Helper()
	ts.sessionKey = sessionKey
	al.activeTurnStates.register(ts)
	t.Cleanup(func() { al.activeTurnStates.unregister(ts) })
}

// turnHandler answers one LLM call of a turn. call counts that turn's calls
// from 1.
type turnHandler func(ctx context.Context, call int, msgs []providers.Message) (*providers.LLMResponse, error)

// routedProvider is a fake LLM that is safe to use from several turns at once.
// Each Chat call is dispatched to the handler whose token appears in the call's
// last user message - the prompt of the turn (or sub-turn) making the call - so
// one provider can script a user turn, a heartbeat and a subagent separately.
//
// Like a real provider it refuses to work on a cancelled context.
type routedProvider struct {
	mu       sync.Mutex
	handlers map[string]turnHandler
	calls    map[string]int
}

func newRoutedProvider() *routedProvider {
	return &routedProvider{handlers: map[string]turnHandler{}, calls: map[string]int{}}
}

// on registers the handler for turns whose prompt contains token. Tokens must
// not contain one another. Register every handler before starting a turn.
func (p *routedProvider) on(token string, h turnHandler) *routedProvider {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.handlers[token] = h
	return p
}

func (p *routedProvider) Chat(
	ctx context.Context,
	msgs []providers.Message,
	_ []providers.ToolDefinition,
	_ string,
	_ map[string]any,
) (*providers.LLMResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	prompt := lastUserContent(msgs)

	p.mu.Lock()
	var handler turnHandler
	call := 0
	for token, h := range p.handlers {
		if strings.Contains(prompt, token) {
			p.calls[token]++
			handler, call = h, p.calls[token]
			break
		}
	}
	p.mu.Unlock()

	if handler == nil {
		return nil, fmt.Errorf("routedProvider: no handler for prompt %q", prompt)
	}
	return handler(ctx, call, msgs)
}

func (p *routedProvider) GetDefaultModel() string { return "char-model" }

// callCount returns how many LLM calls the turn(s) with this token have made.
func (p *routedProvider) callCount(token string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls[token]
}

func lastUserContent(msgs []providers.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return msgs[i].Content
		}
	}
	return ""
}

func textResponse(content string) (*providers.LLMResponse, error) {
	return &providers.LLMResponse{Content: content}, nil
}

func toolCallResponse(name string, args map[string]any) (*providers.LLMResponse, error) {
	return &providers.LLMResponse{ToolCalls: []providers.ToolCall{{
		ID:        "call_" + name,
		Type:      "function",
		Name:      name,
		Arguments: args,
	}}}, nil
}

// blockUntilCancelled is an LLM call that never answers: it returns only when
// the turn's context is cancelled.
func blockUntilCancelled(ctx context.Context) (*providers.LLMResponse, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// signal is a one-shot event: fire may be called any number of times.
type signal struct {
	once sync.Once
	ch   chan struct{}
}

func newSignal() *signal { return &signal{ch: make(chan struct{})} }

func (s *signal) fire() { s.once.Do(func() { close(s.ch) }) }

func (s *signal) fired() <-chan struct{} { return s.ch }

func (s *signal) wait(t *testing.T, what string) {
	t.Helper()
	select {
	case <-s.ch:
	case <-time.After(testWait):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// waitUntil polls cond until it holds. It is for states that have no signal of
// their own to wait on.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(testWait)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// funcTool is a tool whose behaviour is a closure.
type funcTool struct {
	name string
	fn   func(ctx context.Context, args map[string]any) *tools.ToolResult
}

func (t *funcTool) Name() string        { return t.name }
func (t *funcTool) Description() string { return "test tool " + t.name }
func (t *funcTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

func (t *funcTool) Execute(ctx context.Context, args map[string]any) *tools.ToolResult {
	return t.fn(ctx, args)
}

// newTurnTestLoop builds an AgentLoop on a real bus and a real session store,
// with the message tool enabled (and the spawn tools, if asked for).
func newTurnTestLoop(t *testing.T, provider providers.LLMProvider, withSpawn bool) (*AgentLoop, *bus.MessageBus) {
	t.Helper()
	cfg := &config.Config{
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				Workspace:         t.TempDir(),
				Model:             "char-model",
				MaxTokens:         4096,
				MaxToolIterations: 10,
			},
		},
	}
	cfg.Tools.Message.Enabled = true
	if withSpawn {
		cfg.Tools.Spawn.Enabled = true
		cfg.Tools.Subagent.Enabled = true
	}
	msgBus := bus.NewMessageBus()
	al := NewAgentLoop(cfg, msgBus, provider)
	t.Cleanup(al.Close)
	return al, msgBus
}

type turnResult struct {
	response string
	err      error
}

// startTurn runs a root turn on its own goroutine, as cron and the CLI do, on
// exactly the session key given (keys with the "agent:" prefix are used as-is).
func startTurn(ctx context.Context, al *AgentLoop, prompt, sessionKey string) <-chan turnResult {
	done := make(chan turnResult, 1)
	go func() {
		response, err := al.ProcessDirectWithChannel(ctx, prompt, sessionKey, "telegram", "chat-1", false)
		done <- turnResult{response: response, err: err}
	}()
	return done
}

func awaitTurn(t *testing.T, what string, done <-chan turnResult) turnResult {
	t.Helper()
	select {
	case res := <-done:
		return res
	case <-time.After(testWait):
		t.Fatalf("timed out waiting for %s to return", what)
		return turnResult{}
	}
}

// outboundCollector drains the outbound bus so nothing blocks publishing to
// it, and lets a test wait for a particular message.
type outboundCollector struct {
	mu     sync.Mutex
	msgs   []bus.OutboundMessage
	notify chan struct{}
}

func collectOutbound(ctx context.Context, msgBus *bus.MessageBus) *outboundCollector {
	c := &outboundCollector{notify: make(chan struct{}, 1)}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-msgBus.OutboundChan():
				if !ok {
					return
				}
				c.mu.Lock()
				c.msgs = append(c.msgs, msg)
				c.mu.Unlock()
				select {
				case c.notify <- struct{}{}:
				default:
				}
			}
		}
	}()
	return c
}

func (c *outboundCollector) count(match func(bus.OutboundMessage) bool) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, m := range c.msgs {
		if match(m) {
			n++
		}
	}
	return n
}

func (c *outboundCollector) waitFor(t *testing.T, what string, match func(bus.OutboundMessage) bool) {
	t.Helper()
	deadline := time.After(testWait)
	for c.count(match) == 0 {
		select {
		case <-c.notify:
		case <-deadline:
			c.mu.Lock()
			got := fmt.Sprintf("%+v", c.msgs)
			c.mu.Unlock()
			t.Fatalf("timed out waiting for outbound %s; published so far: %s", what, got)
		}
	}
}

func outboundTo(chatID, content string) func(bus.OutboundMessage) bool {
	return func(m bus.OutboundMessage) bool { return m.ChatID == chatID && m.Content == content }
}

func outboundContent(content string) func(bus.OutboundMessage) bool {
	return func(m bus.OutboundMessage) bool { return m.Content == content }
}

// sessionLockRefs returns how many goroutines hold or wait for the session's
// lock: 1 is a holder alone, 2 a holder with one turn queued behind it.
func sessionLockRefs(al *AgentLoop, sessionKey string) int {
	al.sessionLocks.mu.Lock()
	defer al.sessionLocks.mu.Unlock()
	if e := al.sessionLocks.entries[sessionKey]; e != nil {
		return e.refs
	}
	return 0
}

func defaultMessageTool(t *testing.T, al *AgentLoop) *tools.MessageTool {
	t.Helper()
	tool, ok := al.GetRegistry().GetDefaultAgent().Tools.Get(tools.NameMessage)
	if !ok {
		t.Fatal("message tool is not registered")
	}
	mt, ok := tool.(*tools.MessageTool)
	if !ok {
		t.Fatalf("message tool has type %T", tool)
	}
	return mt
}

// runLoop starts al.Run and returns a function that stops it and waits for it
// to return, so no turn is left running into the next test.
func runLoop(t *testing.T, al *AgentLoop) (context.Context, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		_ = al.Run(ctx)
	}()
	return ctx, func() {
		cancel()
		select {
		case <-stopped:
		case <-time.After(testWait):
			t.Error("Run did not stop after its context was cancelled")
		}
	}
}

func publishInbound(t *testing.T, msgBus *bus.MessageBus, chatID, content string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	err := msgBus.PublishInbound(ctx, bus.InboundMessage{
		Channel:  "telegram",
		ChatID:   chatID,
		SenderID: "user-1",
		Content:  content,
	})
	if err != nil {
		t.Fatalf("publish inbound: %v", err)
	}
}

// ====================== per-turn message state ======================

// A heartbeat runs on its own goroutine while Run is busy with a user's
// message. When the heartbeat sends the user a message through the message
// tool, that is the heartbeat's send: the user's turn has said nothing yet, and
// its reply must still go out. With one sent-list per tool instance, the
// heartbeat's send made the user's reply look "already sent" and it was
// dropped.
func TestRun_ConcurrentHeartbeatMessage_DoesNotSuppressUserReply(t *testing.T) {
	userInLLM := newSignal()
	releaseUser := newSignal()

	prov := newRoutedProvider().
		on("USER_TURN", func(ctx context.Context, _ int, _ []providers.Message) (*providers.LLMResponse, error) {
			userInLLM.fire()
			select {
			case <-releaseUser.fired():
				return textResponse("user reply")
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}).
		on("HEARTBEAT_TURN", func(_ context.Context, call int, _ []providers.Message) (*providers.LLMResponse, error) {
			if call == 1 {
				return toolCallResponse(tools.NameMessage, map[string]any{"content": "heartbeat ping"})
			}
			return textResponse("HEARTBEAT_OK")
		})

	al, msgBus := newTurnTestLoop(t, prov, false)
	ctx, stop := runLoop(t, al)
	defer stop()
	out := collectOutbound(ctx, msgBus)

	publishInbound(t, msgBus, "chat-1", "USER_TURN hello")
	userInLLM.wait(t, "the user turn to reach the LLM")

	// The user's turn is now parked inside its LLM call. The heartbeat runs
	// start to finish meanwhile, and messages the very same chat.
	if _, err := al.ProcessHeartbeat(ctx, "HEARTBEAT_TURN check tasks", "telegram", "chat-1"); err != nil {
		t.Fatalf("ProcessHeartbeat: %v", err)
	}
	out.waitFor(t, "the heartbeat's message", outboundTo("chat-1", "heartbeat ping"))

	releaseUser.fire()
	out.waitFor(t, "the user's reply (suppressed as already sent?)", outboundTo("chat-1", "user reply"))

	if n := out.count(outboundContent("user reply")); n != 1 {
		t.Errorf("user reply published %d times, want 1", n)
	}
}

// The same bug in the other direction. A cron turn goes through
// processMessage, which used to reset the message tool's sent-list "for this
// round" - wiping what a concurrent user turn had recorded. The user's turn
// had already answered through the message tool, so its final text must be
// suppressed; after the wipe it was published as well, and the user got the
// answer twice.
func TestRun_ConcurrentCronTurn_DoesNotWipeUserSentState(t *testing.T) {
	userAfterTool := newSignal()
	releaseUser := newSignal()

	prov := newRoutedProvider().
		on("USER_TURN", func(ctx context.Context, call int, _ []providers.Message) (*providers.LLMResponse, error) {
			if call == 1 {
				return toolCallResponse(tools.NameMessage, map[string]any{"content": "answer sent by tool"})
			}
			userAfterTool.fire()
			select {
			case <-releaseUser.fired():
				return textResponse("duplicate final text")
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}).
		on("CRON_TURN", func(context.Context, int, []providers.Message) (*providers.LLMResponse, error) {
			return textResponse("cron report")
		}).
		on("SENTINEL_TURN", func(context.Context, int, []providers.Message) (*providers.LLMResponse, error) {
			return textResponse("sentinel reply")
		})

	al, msgBus := newTurnTestLoop(t, prov, false)
	ctx, stop := runLoop(t, al)
	defer stop()
	out := collectOutbound(ctx, msgBus)

	publishInbound(t, msgBus, "chat-1", "USER_TURN question")
	userAfterTool.wait(t, "the user turn to send through the message tool")
	out.waitFor(t, "the user's tool-sent answer", outboundTo("chat-1", "answer sent by tool"))

	// A cron job fires now, exactly as CronTool.ExecuteJob runs one: tagged
	// cron, with a turn ID of its own, on a session of its own (on the user's
	// session it would simply queue behind the user's turn).
	cronCtx := tools.WithTurnID(tools.WithToolOrigin(ctx, tools.OriginCron), tools.NewTurnID())
	cronSession := "agent:main:cron-isolated"
	response, err := al.ProcessDirectWithChannel(cronCtx, "CRON_TURN report", cronSession, "telegram", "chat-1", false)
	if err != nil {
		t.Fatalf("cron turn: %v", err)
	}
	al.PublishResponseIfNeeded(cronCtx, "telegram", "chat-1", cronSession, response)
	// The cron turn used no message tool, so its own response does go out.
	out.waitFor(t, "the cron response", outboundTo("chat-1", "cron report"))

	releaseUser.fire()

	// Run handles messages one after another, so once the sentinel's reply is
	// out, the user turn before it has been fully dealt with - including the
	// decision whether to publish its final text.
	publishInbound(t, msgBus, "chat-2", "SENTINEL_TURN")
	out.waitFor(t, "the sentinel reply", outboundTo("chat-2", "sentinel reply"))

	if n := out.count(outboundContent("duplicate final text")); n != 0 {
		t.Errorf("the user turn's final text was published %d time(s) although the message tool had "+
			"already answered in that turn: the cron turn wiped its sent-state", n)
	}
	if n := out.count(outboundContent("answer sent by tool")); n != 1 {
		t.Errorf("tool-sent answer delivered %d times, want 1", n)
	}
}

// turnIDCapturingExecutor is the agent loop as the cron tool sees it, recording
// the turn ID the cron tool put in the context.
type turnIDCapturingExecutor struct {
	*AgentLoop
	mu      sync.Mutex
	turnIDs []string
}

func (e *turnIDCapturingExecutor) ProcessDirectWithChannel(
	ctx context.Context,
	content, sessionKey, channel, chatID string,
	noHistory bool,
) (string, error) {
	e.mu.Lock()
	e.turnIDs = append(e.turnIDs, tools.TurnID(ctx))
	e.mu.Unlock()
	return e.AgentLoop.ProcessDirectWithChannel(ctx, content, sessionKey, channel, chatID, noHistory)
}

// The cron tool asks "did the message tool already send to this chat?" after
// the turn has returned. That only works if the turn's message state outlives
// the turn, which it does when the caller names the turn: the real CronTool
// against the real loop, end to end.
func TestCronTurn_MessageToolSuppressionStillWorks(t *testing.T) {
	prov := newRoutedProvider().
		on("CRON_SAME_CHAT", func(_ context.Context, call int, _ []providers.Message) (*providers.LLMResponse, error) {
			if call == 1 {
				return toolCallResponse(tools.NameMessage, map[string]any{"content": "cron says hi"})
			}
			return textResponse("cron final same chat")
		}).
		on("CRON_OTHER_CHAT", func(_ context.Context, call int, _ []providers.Message) (*providers.LLMResponse, error) {
			if call == 1 {
				return toolCallResponse(tools.NameMessage, map[string]any{
					"content": "note to someone else",
					"channel": "telegram",
					"chat_id": "other-chat",
				})
			}
			return textResponse("cron final other chat")
		})

	al, msgBus := newTurnTestLoop(t, prov, false)
	executor := &turnIDCapturingExecutor{AgentLoop: al}
	cronService := cron.NewCronService(filepath.Join(t.TempDir(), "cron.json"), nil)
	cronTool, err := tools.NewCronTool(cronService, executor, msgBus, t.TempDir(), true, 0, config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewCronTool: %v", err)
	}

	// drain returns everything published so far. ExecuteJob is synchronous and
	// the bus is buffered, so by the time it returns every message it caused
	// is already in the channel.
	drain := func() []string {
		var contents []string
		for {
			select {
			case m := <-msgBus.OutboundChan():
				contents = append(contents, m.ChatID+": "+m.Content)
			default:
				return contents
			}
		}
	}

	job := &cron.CronJob{ID: "job-same"}
	job.Payload.Channel = "telegram"
	job.Payload.To = "chat-1"
	job.Payload.Message = "CRON_SAME_CHAT report"
	if got, err := cronTool.ExecuteJob(context.Background(), job); err != nil || got != "ok" {
		t.Fatalf("ExecuteJob = %q, %v, want ok", got, err)
	}
	if got, want := drain(), []string{"chat-1: cron says hi"}; !equalStrings(got, want) {
		t.Errorf("job that messaged its own chat published %q, want only %q "+
			"(the final response must be suppressed)", got, want)
	}

	job = &cron.CronJob{ID: "job-other"}
	job.Payload.Channel = "telegram"
	job.Payload.To = "chat-1"
	job.Payload.Message = "CRON_OTHER_CHAT report"
	if got, err := cronTool.ExecuteJob(context.Background(), job); err != nil || got != "ok" {
		t.Fatalf("ExecuteJob = %q, %v, want ok", got, err)
	}
	want := []string{"other-chat: note to someone else", "chat-1: cron final other chat"}
	if got := drain(); !equalStrings(got, want) {
		t.Errorf("job that messaged a different chat published %q, want %q "+
			"(a message elsewhere must not suppress the response)", got, want)
	}

	// Each job ran as a turn of its own, and nothing about either is kept.
	executor.mu.Lock()
	ids := append([]string(nil), executor.turnIDs...)
	executor.mu.Unlock()
	if len(ids) != 2 || ids[0] == "" || ids[1] == "" || ids[0] == ids[1] {
		t.Fatalf("cron turn IDs = %q, want two distinct non-empty IDs", ids)
	}
	mt := defaultMessageTool(t, al)
	for _, id := range ids {
		if mt.HasSentInTurn(id) {
			t.Errorf("message state of cron turn %s was not cleared after PublishResponseIfNeeded", id)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// A turn the loop names itself (nobody asks about it afterwards) cleans up
// after itself; one the caller named is left for the caller.
func TestTurnMessageState_ClearedByWhoeverNamedTheTurn(t *testing.T) {
	prov := newRoutedProvider().
		on("SENDING_TURN", func(_ context.Context, call int, _ []providers.Message) (*providers.LLMResponse, error) {
			if call%2 == 1 {
				return toolCallResponse(tools.NameMessage, map[string]any{"content": "sent"})
			}
			return textResponse("done")
		})
	al, msgBus := newTurnTestLoop(t, prov, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	collectOutbound(ctx, msgBus)
	mt := defaultMessageTool(t, al)

	// Named by the caller: still there after the turn, gone after the caller
	// has made its decision.
	turnID := tools.NewTurnID()
	callerCtx := tools.WithTurnID(ctx, turnID)
	if _, err := al.ProcessDirectWithChannel(callerCtx, "SENDING_TURN", "agent:main:named", "telegram", "chat-1", false); err != nil {
		t.Fatalf("turn: %v", err)
	}
	if !mt.HasSentToInTurn(turnID, "telegram", "chat-1") {
		t.Fatal("the loop cleared the message state of a turn its caller named, before the caller could ask")
	}
	al.PublishResponseIfNeeded(callerCtx, "telegram", "chat-1", "agent:main:named", "done")
	if mt.HasSentInTurn(turnID) {
		t.Error("PublishResponseIfNeeded did not clear the turn's message state")
	}

	// Named by the loop: a tool observes the ID mid-turn; nothing is left
	// under it afterwards.
	var seen atomic.Value
	al.RegisterTool(&funcTool{name: "note_turn", fn: func(ctx context.Context, _ map[string]any) *tools.ToolResult {
		seen.Store(tools.TurnID(ctx))
		return tools.SilentResult("noted")
	}})
	prov.on("OWNED_TURN", func(_ context.Context, call int, _ []providers.Message) (*providers.LLMResponse, error) {
		switch call {
		case 1:
			return toolCallResponse("note_turn", map[string]any{})
		case 2:
			return toolCallResponse(tools.NameMessage, map[string]any{"content": "sent"})
		default:
			return textResponse("done")
		}
	})
	if _, err := al.ProcessDirectWithChannel(ctx, "OWNED_TURN", "agent:main:owned", "telegram", "chat-1", false); err != nil {
		t.Fatalf("turn: %v", err)
	}
	ownedID, _ := seen.Load().(string)
	if ownedID == "" {
		t.Fatal("a turn started without a turn ID was not given one")
	}
	if mt.HasSentInTurn(ownedID) {
		t.Error("the loop did not clear the message state of a turn it named itself")
	}
}

// ====================== cancellation ======================

func TestCancelTurnsForSession_StopsRootTurn(t *testing.T) {
	const session = "agent:main:cancel-me"
	inLLM := newSignal()

	prov := newRoutedProvider().
		on("HANG_TURN", func(ctx context.Context, _ int, _ []providers.Message) (*providers.LLMResponse, error) {
			inLLM.fire()
			return blockUntilCancelled(ctx)
		}).
		on("NEXT_TURN", func(context.Context, int, []providers.Message) (*providers.LLMResponse, error) {
			return textResponse("next ok")
		})
	al, _ := newTurnTestLoop(t, prov, false)

	// The caller's context is never cancelled: only CancelTurnsForSession
	// can be what stops the turn.
	turn := startTurn(context.Background(), al, "HANG_TURN", session)
	inLLM.wait(t, "the turn to block in the LLM")

	if n := al.CancelTurnsForSession("agent:main:some-other-session"); n != 0 {
		t.Fatalf("cancelling an idle session signalled %d turns, want 0", n)
	}

	cancelledAt := time.Now()
	if n := al.CancelTurnsForSession(session); n != 1 {
		t.Fatalf("CancelTurnsForSession signalled %d turns, want 1", n)
	}
	res := awaitTurn(t, "the cancelled turn", turn)
	if elapsed := time.Since(cancelledAt); elapsed > 100*time.Millisecond {
		t.Errorf("turn returned %v after the cancel, want within 100ms", elapsed)
	}
	if !errors.Is(res.err, context.Canceled) {
		t.Fatalf("cancelled turn returned err = %v, want context.Canceled", res.err)
	}

	// The cancelled turn let go of everything: the registry, and the session
	// lock - a second turn on the same session runs.
	if info := al.GetActiveTurn(session); info != nil {
		t.Errorf("cancelled turn is still registered: %+v", info)
	}
	if refs := sessionLockRefs(al, session); refs != 0 {
		t.Errorf("session lock still has %d holder(s)/waiter(s) after the turn returned", refs)
	}
	next := awaitTurn(t, "a second turn on the same session", startTurn(context.Background(), al, "NEXT_TURN", session))
	if next.err != nil || next.response != "next ok" {
		t.Fatalf("second turn on the session = (%q, %v), want (\"next ok\", nil)", next.response, next.err)
	}
	if n := al.sessionLocks.size(); n != 0 {
		t.Errorf("session lock map has %d entries with no turn running, want 0", n)
	}
}

// A sub-turn's context is built from context.Background, so that it can
// outlive its parent's normal return. Cancelling the parent's context does not
// reach it; CancelAllTurns must, through the turn tree.
func TestCancelAllTurns_ReachesSubTurns(t *testing.T) {
	t.Run("synchronous child", func(t *testing.T) {
		childInLLM := newSignal()
		childErr := make(chan error, 1)

		prov := newRoutedProvider().
			on("PARENT_TURN", func(_ context.Context, call int, _ []providers.Message) (*providers.LLMResponse, error) {
				if call == 1 {
					return toolCallResponse("spawn_sync_child", map[string]any{})
				}
				return textResponse("parent finished - it should have been cancelled")
			}).
			on("CHILD_TURN", func(ctx context.Context, _ int, _ []providers.Message) (*providers.LLMResponse, error) {
				childInLLM.fire()
				<-ctx.Done()
				childErr <- ctx.Err()
				return nil, ctx.Err()
			})
		al, _ := newTurnTestLoop(t, prov, false)
		al.RegisterTool(&funcTool{name: "spawn_sync_child", fn: func(ctx context.Context, _ map[string]any) *tools.ToolResult {
			res, err := SpawnSubTurn(ctx, SubTurnConfig{Model: "char-model", SystemPrompt: "CHILD_TURN work"})
			if err != nil {
				return tools.ErrorResult(err.Error())
			}
			return res
		}})

		turn := startTurn(context.Background(), al, "PARENT_TURN", "agent:main:cancel-all-sync")
		childInLLM.wait(t, "the sub-turn to block in the LLM")

		if n := al.CancelAllTurns("test"); n != 1 {
			t.Fatalf("CancelAllTurns signalled %d root turns, want 1", n)
		}

		select {
		case err := <-childErr:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("sub-turn's context ended with %v, want context.Canceled", err)
			}
		case <-time.After(testWait):
			t.Fatal("CancelAllTurns did not reach the sub-turn: it is still blocked in the LLM")
		}
		res := awaitTurn(t, "the cancelled parent turn", turn)
		if !errors.Is(res.err, context.Canceled) {
			t.Errorf("parent turn returned (%q, %v), want context.Canceled", res.response, res.err)
		}
	})

	// The same through the real spawn tool, whose child runs in the background
	// on a context that has been detached from the parent's cancellation.
	t.Run("async child from the spawn tool", func(t *testing.T) {
		childInLLM := newSignal()
		parentInSecondCall := newSignal()
		childErr := make(chan error, 1)

		prov := newRoutedProvider().
			on("PARENT_TURN", func(ctx context.Context, call int, _ []providers.Message) (*providers.LLMResponse, error) {
				if call == 1 {
					return toolCallResponse(tools.NameSpawn, map[string]any{"task": "background job"})
				}
				parentInSecondCall.fire()
				return blockUntilCancelled(ctx)
			}).
			on("spawned subagent", func(ctx context.Context, _ int, _ []providers.Message) (*providers.LLMResponse, error) {
				childInLLM.fire()
				<-ctx.Done()
				childErr <- ctx.Err()
				return nil, ctx.Err()
			})
		al, msgBus := newTurnTestLoop(t, prov, true)

		turn := startTurn(context.Background(), al, "PARENT_TURN", "agent:main:cancel-all-async")
		childInLLM.wait(t, "the spawned sub-turn to block in the LLM")
		parentInSecondCall.wait(t, "the parent to block in its next LLM call")

		if n := al.CancelAllTurns("test"); n != 1 {
			t.Fatalf("CancelAllTurns signalled %d root turns, want 1", n)
		}

		select {
		case err := <-childErr:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("spawned sub-turn's context ended with %v, want context.Canceled", err)
			}
		case <-time.After(testWait):
			t.Fatal("CancelAllTurns did not reach the spawned sub-turn")
		}
		res := awaitTurn(t, "the cancelled parent turn", turn)
		if !errors.Is(res.err, context.Canceled) {
			t.Errorf("parent turn returned (%q, %v), want context.Canceled", res.response, res.err)
		}

		// The spawn's goroutine reports its failure as a system message; that
		// is the last thing it does, so waiting for it leaves nothing of this
		// test running into the next one.
		select {
		case msg := <-msgBus.InboundChan():
			if msg.Channel != "system" || !strings.Contains(msg.Content, "context canceled") {
				t.Errorf("spawn reported %+v, want a system message about the cancellation", msg)
			}
		case <-time.After(testWait):
			t.Fatal("the cancelled spawn never reported back")
		}
	})

	// The case a stop command meets most often: the turn that spawned the
	// subagent answered and returned long ago, and the subagent is still
	// working. The parent is no longer registered anywhere, so the child has
	// to be found on its own.
	orphan := func(t *testing.T, session string, cancel func(al *AgentLoop) int) {
		childInLLM := newSignal()
		childErr := make(chan error, 1)

		prov := newRoutedProvider().
			on("PARENT_TURN", func(ctx context.Context, call int, _ []providers.Message) (*providers.LLMResponse, error) {
				if call == 1 {
					return toolCallResponse(tools.NameSpawn, map[string]any{"task": "background job"})
				}
				// Answer only once the child is busy, so that it is mid-call
				// - not between iterations - when the parent returns.
				select {
				case <-childInLLM.fired():
					return textResponse("parent done")
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}).
			on("spawned subagent", func(ctx context.Context, _ int, _ []providers.Message) (*providers.LLMResponse, error) {
				childInLLM.fire()
				<-ctx.Done()
				childErr <- ctx.Err()
				return nil, ctx.Err()
			})
		al, msgBus := newTurnTestLoop(t, prov, true)

		res := awaitTurn(t, "the parent turn", startTurn(context.Background(), al, "PARENT_TURN", session))
		if res.err != nil || res.response != "parent done" {
			t.Fatalf("parent turn = (%q, %v), want (\"parent done\", nil)", res.response, res.err)
		}
		if info := al.GetActiveTurn(session); info != nil {
			t.Fatalf("the parent turn is still registered after returning: %+v", info)
		}
		select {
		case err := <-childErr:
			t.Fatalf("the sub-turn was cancelled (%v) by its parent's normal return", err)
		default:
		}

		if n := al.CancelTurnsForSession("agent:main:someone-else"); n != 0 {
			t.Fatalf("cancelling another session stopped %d turn(s), want 0", n)
		}
		if n := cancel(al); n != 1 {
			t.Errorf("the cancel reported %d turn(s) stopped, want 1: the returned turn whose sub-turn was running", n)
		}
		select {
		case err := <-childErr:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("sub-turn's context ended with %v, want context.Canceled", err)
			}
		case <-time.After(testWait):
			t.Fatal("the cancel did not reach a sub-turn whose parent had already returned")
		}

		// As above: wait for the spawn's goroutine to report and finish.
		select {
		case <-msgBus.InboundChan():
		case <-time.After(testWait):
			t.Fatal("the cancelled spawn never reported back")
		}
		if n := cancel(al); n != 0 {
			t.Errorf("a second cancel reported %d turn(s) stopped with nothing left running, want 0", n)
		}
	}
	t.Run("async child that outlived its parent, CancelAllTurns", func(t *testing.T) {
		orphan(t, "agent:main:orphan-all", func(al *AgentLoop) int { return al.CancelAllTurns("test") })
	})
	t.Run("async child that outlived its parent, CancelTurnsForSession", func(t *testing.T) {
		const session = "agent:main:orphan-session"
		orphan(t, session, func(al *AgentLoop) int { return al.CancelTurnsForSession(session) })
	})
}

// An async spawn is started during a turn and is meant to outlive it. The turn
// cancels its context when it returns; the spawn must not take that as its own
// cancellation. Here the spawn is still waiting for a concurrency slot when
// its parent returns normally - the point at which it used to depend on the
// parent's context - and must go on to run once a slot frees up.
func TestSpawnAsync_SurvivesParentNormalReturn(t *testing.T) {
	var parentTS atomic.Pointer[turnState]

	prov := newRoutedProvider().
		on("PARENT_TURN", func(_ context.Context, call int, _ []providers.Message) (*providers.LLMResponse, error) {
			switch call {
			case 1:
				return toolCallResponse("occupy_slots", map[string]any{})
			case 2:
				return toolCallResponse(tools.NameSpawn, map[string]any{"task": "background job"})
			default:
				return textResponse("parent done")
			}
		}).
		on("spawned subagent", func(context.Context, int, []providers.Message) (*providers.LLMResponse, error) {
			return textResponse("child done")
		})
	al, _ := newTurnTestLoop(t, prov, true)

	// Fill every sub-turn slot of the turn, so the spawn that follows has to
	// wait - and is still waiting when the parent returns.
	al.RegisterTool(&funcTool{name: "occupy_slots", fn: func(ctx context.Context, _ map[string]any) *tools.ToolResult {
		ts := TurnStateFromContext(ctx)
		for i := 0; i < cap(ts.concurrencySem); i++ {
			ts.concurrencySem <- struct{}{}
		}
		parentTS.Store(ts)
		return tools.SilentResult("slots occupied")
	}})

	// Watch sub-turn lifecycle events for this parent's child.
	var (
		evMu    sync.Mutex
		spawned = map[string]string{} // parent turn ID -> child ID
		ended   = map[string]error{}  // child ID -> how it ended
		evSeen  = make(chan struct{}, 1)
	)
	originalEmit := MockEventBus.Emit
	MockEventBus.Emit = func(e any) {
		evMu.Lock()
		switch ev := e.(type) {
		case SubTurnSpawnEvent:
			spawned[ev.ParentID] = ev.ChildID
		case SubTurnEndEvent:
			ended[ev.ChildID] = ev.Err
		}
		evMu.Unlock()
		select {
		case evSeen <- struct{}{}:
		default:
		}
	}
	defer func() { MockEventBus.Emit = originalEmit }()

	res := awaitTurn(t, "the parent turn", startTurn(context.Background(), al, "PARENT_TURN", "agent:main:spawn-survives"))
	if res.err != nil || res.response != "parent done" {
		t.Fatalf("parent turn = (%q, %v), want (\"parent done\", nil)", res.response, res.err)
	}
	ts := parentTS.Load()
	if ts == nil {
		t.Fatal("occupy_slots never ran")
	}
	evMu.Lock()
	_, spawnedEarly := spawned[ts.turnID]
	evMu.Unlock()
	if spawnedEarly {
		t.Fatal("the sub-turn started although every slot was occupied; the test no longer exercises the wait")
	}

	// The parent has returned and its context is cancelled. Free the slots:
	// the waiting spawn must take one and run to a clean end.
	for i := 0; i < cap(ts.concurrencySem); i++ {
		<-ts.concurrencySem
	}

	deadline := time.After(testWait)
	for {
		evMu.Lock()
		childID, started := spawned[ts.turnID]
		err, done := ended[childID]
		evMu.Unlock()
		if started && done {
			if err != nil {
				t.Fatalf("spawned sub-turn ended with %v, want a clean finish", err)
			}
			break
		}
		select {
		case <-evSeen:
		case <-deadline:
			t.Fatalf("the spawn did not survive its parent's normal return (started=%v, ended=%v): "+
				"it was cancelled along with the parent's context", started, done)
		}
	}
}

// ====================== rollback ======================

// A hard abort undoes the aborted turn and nothing else. The snapshot it rolls
// back to used to be measured on the wrong session key (always 0 on a real
// store), so the abort truncated the session to nothing; a store that ignores
// its key hid that, hence the real store here.
func TestHardAbort_RollsBackOnlyNewMessages(t *testing.T) {
	seedHistory := func(t *testing.T, al *AgentLoop, session string) {
		t.Helper()
		sessions := al.GetRegistry().GetDefaultAgent().Sessions
		sessions.AddMessage(session, "user", "old question")
		sessions.AddMessage(session, "assistant", "old answer")
		if err := sessions.Save(session); err != nil {
			t.Fatalf("save seeded history: %v", err)
		}
		if n := len(sessions.GetHistory(session)); n != 2 {
			t.Fatalf("seeded history has %d messages, want 2", n)
		}
	}
	assertOnlySeedLeft := func(t *testing.T, al *AgentLoop, session string) {
		t.Helper()
		history := al.GetRegistry().GetDefaultAgent().Sessions.GetHistory(session)
		if len(history) != 2 {
			t.Fatalf("history has %d messages after the abort, want exactly the 2 from before the turn: %+v",
				len(history), history)
		}
		if history[0].Content != "old question" || history[1].Content != "old answer" {
			t.Errorf("pre-existing history was altered by the abort: %+v", history)
		}
	}

	t.Run("aborted while waiting for the LLM", func(t *testing.T) {
		const session = "agent:main:rollback-llm"
		inSecondCall := newSignal()
		prov := newRoutedProvider().
			on("ABORTED_TURN", func(ctx context.Context, call int, _ []providers.Message) (*providers.LLMResponse, error) {
				if call == 1 {
					return toolCallResponse("char_tool", map[string]any{})
				}
				inSecondCall.fire()
				return blockUntilCancelled(ctx)
			})
		al, _ := newTurnTestLoop(t, prov, false)
		al.RegisterTool(&charRecordingTool{name: "char_tool", llmOutput: "tool output"})
		seedHistory(t, al, session)

		turn := startTurn(context.Background(), al, "ABORTED_TURN", session)
		inSecondCall.wait(t, "the turn to block in its second LLM call")

		sessions := al.GetRegistry().GetDefaultAgent().Sessions
		if n := len(sessions.GetHistory(session)); n <= 2 {
			t.Fatalf("the turn has added nothing to the session yet (%d messages); nothing to roll back", n)
		}

		if err := al.HardAbort(session); err != nil {
			t.Fatalf("HardAbort: %v", err)
		}
		res := awaitTurn(t, "the aborted turn", turn)
		if !errors.Is(res.err, context.Canceled) {
			t.Errorf("aborted turn returned err = %v, want context.Canceled", res.err)
		}
		assertOnlySeedLeft(t, al, session)
	})

	// The abort lands while a tool is running. The tool finishes afterwards
	// and its result is appended to the session - after HardAbort has rolled
	// back. The turn rolls back again on its way out.
	t.Run("aborted while a tool is still running", func(t *testing.T) {
		const session = "agent:main:rollback-tool"
		toolRunning := newSignal()
		releaseTool := newSignal()
		prov := newRoutedProvider().
			on("ABORTED_TURN", func(context.Context, int, []providers.Message) (*providers.LLMResponse, error) {
				return toolCallResponse("slow_tool", map[string]any{})
			})
		al, _ := newTurnTestLoop(t, prov, false)
		al.RegisterTool(&funcTool{name: "slow_tool", fn: func(context.Context, map[string]any) *tools.ToolResult {
			toolRunning.fire()
			<-releaseTool.fired() // a tool that does not watch its context
			return tools.SilentResult("late tool output")
		}})
		seedHistory(t, al, session)

		turn := startTurn(context.Background(), al, "ABORTED_TURN", session)
		toolRunning.wait(t, "the tool to start")

		if err := al.HardAbort(session); err != nil {
			t.Fatalf("HardAbort: %v", err)
		}
		releaseTool.fire()
		res := awaitTurn(t, "the aborted turn", turn)
		if !errors.Is(res.err, context.Canceled) {
			t.Errorf("aborted turn returned err = %v, want context.Canceled", res.err)
		}
		assertOnlySeedLeft(t, al, session)
	})

	// An abort can land just after its target has returned. By then the
	// session belongs to whoever runs next, and a rollback to the old turn's
	// snapshot would cut into that turn's messages.
	t.Run("a turn that has already exited is not rolled back", func(t *testing.T) {
		const session = "agent:main:rollback-exited"
		al, _ := newTurnTestLoop(t, newRoutedProvider(), false)
		seedHistory(t, al, session)
		sessions := al.GetRegistry().GetDefaultAgent().Sessions

		finished := &turnState{turnID: "finished", sessionKey: session, session: sessions}
		finished.snapshotHistory() // 2: the seed
		finished.exit()

		sessions.AddMessage(session, "user", "the next turn's question")
		finished.rollbackHistory()
		if n := len(sessions.GetHistory(session)); n != 3 {
			t.Fatalf("history has %d messages, want 3: a turn that had exited rolled back the next turn's message", n)
		}

		// The same turn, had it still been running, is rolled back.
		running := &turnState{turnID: "running", sessionKey: session, session: sessions}
		running.setHistorySnapshot(2)
		running.rollbackHistory()
		assertOnlySeedLeft(t, al, session)
	})

	// A turn still queued for the session lock has written nothing and has no
	// snapshot; aborting it must not touch the session it is waiting for.
	t.Run("aborting a queued turn rolls back only the running one", func(t *testing.T) {
		const session = "agent:main:rollback-queued"
		inLLM := newSignal()
		prov := newRoutedProvider().
			on("RUNNING_TURN", func(ctx context.Context, _ int, _ []providers.Message) (*providers.LLMResponse, error) {
				inLLM.fire()
				return blockUntilCancelled(ctx)
			}).
			on("QUEUED_TURN", func(context.Context, int, []providers.Message) (*providers.LLMResponse, error) {
				return textResponse("queued turn ran - it should have been aborted")
			})
		al, _ := newTurnTestLoop(t, prov, false)
		seedHistory(t, al, session)

		running := startTurn(context.Background(), al, "RUNNING_TURN", session)
		inLLM.wait(t, "the first turn to block in the LLM")
		queued := startTurn(context.Background(), al, "QUEUED_TURN", session)
		waitUntil(t, "the second turn to queue for the session lock", func() bool {
			return sessionLockRefs(al, session) == 2
		})

		if err := al.HardAbort(session); err != nil {
			t.Fatalf("HardAbort: %v", err)
		}
		for name, turn := range map[string]<-chan turnResult{"running": running, "queued": queued} {
			if res := awaitTurn(t, "the "+name+" turn", turn); !errors.Is(res.err, context.Canceled) {
				t.Errorf("%s turn returned (%q, %v), want context.Canceled", name, res.response, res.err)
			}
		}
		assertOnlySeedLeft(t, al, session)
	})
}

// ====================== session lock ======================

func TestSessionLock_SerialisesSameSession_ParallelDifferentSessions(t *testing.T) {
	t.Run("lock", func(t *testing.T) {
		var locks sessionLocks

		// Mutual exclusion on one key: counter has no other protection, so
		// the race detector fails this if two holders ever overlap.
		counter := 0
		var wg sync.WaitGroup
		for i := 0; i < 32; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < 50; j++ {
					_, unlock, err := locks.lock(context.Background(), "same")
					if err != nil {
						t.Errorf("lock: %v", err)
						return
					}
					counter++
					unlock()
				}
			}()
		}
		wg.Wait()
		if counter != 32*50 {
			t.Errorf("counter = %d, want %d", counter, 32*50)
		}

		// A different key is not held up by this one being locked.
		_, unlockA, err := locks.lock(context.Background(), "a")
		if err != nil {
			t.Fatalf("lock a: %v", err)
		}
		bCtx, cancel := context.WithTimeout(context.Background(), testWait)
		defer cancel()
		_, unlockB, err := locks.lock(bCtx, "b")
		if err != nil {
			t.Fatalf("lock b while a is held: %v", err)
		}
		if n := locks.size(); n != 2 {
			t.Errorf("lock map has %d entries with two sessions held, want 2", n)
		}
		unlockB()
		unlockA()
		unlockA() // unlocking twice is harmless

		// Entries are dropped when nobody holds or waits: the map does not
		// grow with the number of sessions ever seen.
		if n := locks.size(); n != 0 {
			t.Errorf("lock map has %d entries with nothing held, want 0", n)
		}
	})

	t.Run("turns", func(t *testing.T) {
		const sessionA, sessionB = "agent:main:lock-a", "agent:main:lock-b"
		firstInLLM := newSignal()
		releaseFirst := newSignal()
		var secondSawFirstReply atomic.Bool

		prov := newRoutedProvider().
			on("FIRST_ON_A", func(ctx context.Context, _ int, _ []providers.Message) (*providers.LLMResponse, error) {
				firstInLLM.fire()
				select {
				case <-releaseFirst.fired():
					return textResponse("first reply")
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}).
			on("SECOND_ON_A", func(_ context.Context, _ int, msgs []providers.Message) (*providers.LLMResponse, error) {
				secondSawFirstReply.Store(anyMessageContains(msgs, "first reply"))
				return textResponse("second reply")
			}).
			on("ONLY_ON_B", func(context.Context, int, []providers.Message) (*providers.LLMResponse, error) {
				return textResponse("b reply")
			})
		al, _ := newTurnTestLoop(t, prov, false)

		first := startTurn(context.Background(), al, "FIRST_ON_A", sessionA)
		firstInLLM.wait(t, "the first turn on A to block in the LLM")

		second := startTurn(context.Background(), al, "SECOND_ON_A", sessionA)
		waitUntil(t, "the second turn on A to queue for the session lock", func() bool {
			return sessionLockRefs(al, sessionA) == 2
		})

		// A different session is not held up: its turn runs to completion
		// while A's first turn still holds A's lock.
		b := awaitTurn(t, "the turn on B", startTurn(context.Background(), al, "ONLY_ON_B", sessionB))
		if b.err != nil || b.response != "b reply" {
			t.Fatalf("turn on B = (%q, %v), want (\"b reply\", nil)", b.response, b.err)
		}

		// The second turn on A has not started.
		if n := prov.callCount("SECOND_ON_A"); n != 0 {
			t.Fatalf("the second turn on A reached the LLM %d time(s) while the first still held the session", n)
		}
		select {
		case res := <-second:
			t.Fatalf("the second turn on A returned (%q, %v) while the first still held the session", res.response, res.err)
		default:
		}

		releaseFirst.fire()
		if res := awaitTurn(t, "the first turn on A", first); res.err != nil || res.response != "first reply" {
			t.Fatalf("first turn on A = (%q, %v)", res.response, res.err)
		}
		if res := awaitTurn(t, "the second turn on A", second); res.err != nil || res.response != "second reply" {
			t.Fatalf("second turn on A = (%q, %v)", res.response, res.err)
		}
		// It ran strictly after the first: the first's saved reply was in the
		// history it loaded.
		if !secondSawFirstReply.Load() {
			t.Error("the second turn on A did not see the first turn's reply in its history: the turns interleaved")
		}
		if n := al.sessionLocks.size(); n != 0 {
			t.Errorf("session lock map has %d entries with no turn running, want 0", n)
		}
	})
}

func TestSessionLock_CancelWhileWaiting(t *testing.T) {
	t.Run("lock", func(t *testing.T) {
		var locks sessionLocks
		_, unlockHolder, err := locks.lock(context.Background(), "s")
		if err != nil {
			t.Fatalf("lock: %v", err)
		}

		waiterCtx, cancelWaiter := context.WithCancel(context.Background())
		waiterErr := make(chan error, 1)
		go func() {
			_, unlock, err := locks.lock(waiterCtx, "s")
			if err == nil {
				unlock()
			}
			waiterErr <- err
		}()
		waitUntil(t, "the waiter to queue", func() bool {
			locks.mu.Lock()
			defer locks.mu.Unlock()
			return locks.entries["s"] != nil && locks.entries["s"].refs == 2
		})

		cancelWaiter()
		select {
		case err := <-waiterErr:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled waiter returned %v, want context.Canceled", err)
			}
		case <-time.After(testWait):
			t.Fatal("a waiter whose context was cancelled is still waiting for the lock")
		}

		// The waiter left without the lock and without a trace: the holder
		// still holds it, alone.
		locks.mu.Lock()
		refs := locks.entries["s"].refs
		locks.mu.Unlock()
		if refs != 1 {
			t.Errorf("lock has %d holder(s)/waiter(s) after the waiter gave up, want 1", refs)
		}

		// A context that is already cancelled never acquires, even when the
		// lock is free.
		unlockHolder()
		dead, cancelDead := context.WithCancel(context.Background())
		cancelDead()
		if _, _, err := locks.lock(dead, "s"); !errors.Is(err, context.Canceled) {
			t.Errorf("lock with a cancelled context returned %v, want context.Canceled", err)
		}
		if n := locks.size(); n != 0 {
			t.Errorf("lock map has %d entries with nothing held, want 0", n)
		}
	})

	// The same for a real turn: cancelled while queued behind another turn,
	// it returns at once, never runs, and leaves the running turn alone.
	t.Run("turn", func(t *testing.T) {
		const session = "agent:main:cancel-queued"
		holderInLLM := newSignal()
		prov := newRoutedProvider().
			on("HOLDER_TURN", func(ctx context.Context, _ int, _ []providers.Message) (*providers.LLMResponse, error) {
				holderInLLM.fire()
				return blockUntilCancelled(ctx)
			}).
			on("QUEUED_TURN", func(context.Context, int, []providers.Message) (*providers.LLMResponse, error) {
				return textResponse("queued turn ran - it should have been cancelled first")
			})
		al, _ := newTurnTestLoop(t, prov, false)

		holder := startTurn(context.Background(), al, "HOLDER_TURN", session)
		holderInLLM.wait(t, "the holding turn to block in the LLM")

		queuedCtx, cancelQueued := context.WithCancel(context.Background())
		defer cancelQueued()
		queued := startTurn(queuedCtx, al, "QUEUED_TURN", session)
		waitUntil(t, "the second turn to queue for the session lock", func() bool {
			return sessionLockRefs(al, session) == 2
		})

		cancelQueued()
		res := awaitTurn(t, "the cancelled queued turn", queued)
		if !errors.Is(res.err, context.Canceled) {
			t.Fatalf("queued turn returned (%q, %v), want context.Canceled", res.response, res.err)
		}
		if n := prov.callCount("QUEUED_TURN"); n != 0 {
			t.Errorf("the cancelled queued turn reached the LLM %d time(s)", n)
		}
		if refs := sessionLockRefs(al, session); refs != 1 {
			t.Errorf("session lock has %d holder(s)/waiter(s) after the queued turn left, want 1", refs)
		}
		select {
		case res := <-holder:
			t.Fatalf("the holding turn ended (%q, %v) when only the queued turn was cancelled", res.response, res.err)
		default:
		}

		al.CancelTurnsForSession(session)
		awaitTurn(t, "the holding turn", holder)
	})
}

// Work that already runs under a session's lock can reach code that takes the
// same lock. The context records what it holds, so that is a pass-through
// rather than a deadlock against itself.
//
// Note what does and does not go through that mechanism. A turn started from
// inside another turn, on the same context chain, is not a root turn at all -
// the context carries the outer turn's state - and takes no lock in the first
// place (third subtest). The held-lock record is what makes the other two
// safe: taking the lock again from a tool, and starting a root turn from
// inside WithSessionLockContext.
func TestSessionLock_ReentrantSameTurnChain(t *testing.T) {
	t.Run("lock", func(t *testing.T) {
		var locks sessionLocks
		outerCtx, unlockOuter, err := locks.lock(context.Background(), "s")
		if err != nil {
			t.Fatalf("outer lock: %v", err)
		}
		if !holdsSessionLock(outerCtx, "s") {
			t.Fatal("the context returned by lock does not record the lock as held")
		}
		if holdsSessionLock(outerCtx, "other") || holdsSessionLock(context.Background(), "s") {
			t.Fatal("a lock is reported held by a context that never acquired it")
		}

		// Same chain, same key: immediate. The timeout only turns a
		// regression into a failure instead of a hung test.
		innerCtx, cancel := context.WithTimeout(outerCtx, testWait)
		defer cancel()
		_, unlockInner, err := locks.lock(innerCtx, "s")
		if err != nil {
			t.Fatalf("re-entrant lock on the holder's own context chain: %v", err)
		}

		// The inner unlock gives nothing back: the outer holder still holds.
		unlockInner()
		strangerCtx, cancelStranger := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancelStranger()
		if _, _, err := locks.lock(strangerCtx, "s"); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("a different context chain got the lock (err = %v) after only the inner unlock", err)
		}

		// A different key is a real acquisition even on this chain.
		_, unlockOther, err := locks.lock(outerCtx, "other")
		if err != nil {
			t.Fatalf("lock of another key from a holder's context: %v", err)
		}
		unlockOther()

		// Once released, the old context is no longer a holder: it has to
		// queue like anyone else, and what it then acquires is a real lock.
		unlockOuter()
		if holdsSessionLock(outerCtx, "s") {
			t.Fatal("a context still counts as holding a lock that has been released")
		}
		_, unlockAgain, err := locks.lock(outerCtx, "s")
		if err != nil {
			t.Fatalf("lock after release: %v", err)
		}
		if n := locks.size(); n != 1 {
			t.Errorf("lock map has %d entries, want 1: a stale context skipped the lock instead of taking it", n)
		}
		unlockAgain()
		if n := locks.size(); n != 0 {
			t.Errorf("lock map has %d entries with nothing held, want 0", n)
		}
	})

	t.Run("tool takes its own session's lock", func(t *testing.T) {
		const session = "agent:main:reentrant-tool"
		prov := newRoutedProvider().
			on("OUTER_TURN", func(_ context.Context, call int, _ []providers.Message) (*providers.LLMResponse, error) {
				if call == 1 {
					return toolCallResponse("relock", map[string]any{})
				}
				return textResponse("outer done")
			})
		al, _ := newTurnTestLoop(t, prov, false)
		var ranUnderLock atomic.Bool
		al.RegisterTool(&funcTool{name: "relock", fn: func(ctx context.Context, _ map[string]any) *tools.ToolResult {
			err := al.WithSessionLockContext(ctx, session, func(context.Context) error {
				ranUnderLock.Store(true)
				return nil
			})
			if err != nil {
				return tools.ErrorResult(err.Error())
			}
			return tools.SilentResult("relocked")
		}})

		res := awaitTurn(t, "a turn whose tool re-takes the session lock (deadlock?)",
			startTurn(context.Background(), al, "OUTER_TURN", session))
		if res.err != nil || res.response != "outer done" {
			t.Fatalf("turn = (%q, %v), want (\"outer done\", nil)", res.response, res.err)
		}
		if !ranUnderLock.Load() {
			t.Error("the tool's WithSessionLockContext callback never ran")
		}
	})

	t.Run("root turn started under WithSessionLockContext", func(t *testing.T) {
		const session = "agent:main:reentrant-root"
		prov := newRoutedProvider().
			on("INNER_TURN", func(context.Context, int, []providers.Message) (*providers.LLMResponse, error) {
				return textResponse("inner done")
			})
		al, _ := newTurnTestLoop(t, prov, false)

		done := make(chan turnResult, 1)
		go func() {
			var res turnResult
			res.err = al.WithSessionLockContext(context.Background(), session, func(ctx context.Context) error {
				// A root turn: nothing in ctx but the held lock.
				response, err := al.ProcessDirectWithChannel(ctx, "INNER_TURN", session, "telegram", "chat-1", false)
				res.response = response
				return err
			})
			done <- res
		}()
		res := awaitTurn(t, "a root turn started while its session lock is held by its own caller (deadlock?)", done)
		if res.err != nil || res.response != "inner done" {
			t.Fatalf("turn = (%q, %v), want (\"inner done\", nil)", res.response, res.err)
		}
		if n := al.sessionLocks.size(); n != 0 {
			t.Errorf("session lock map has %d entries afterwards, want 0", n)
		}
	})

	t.Run("turn started by a tool of a turn on the same session", func(t *testing.T) {
		const session = "agent:main:reentrant-nested"
		prov := newRoutedProvider().
			on("OUTER_TURN", func(_ context.Context, call int, _ []providers.Message) (*providers.LLMResponse, error) {
				if call == 1 {
					return toolCallResponse("nested_turn", map[string]any{})
				}
				return textResponse("outer done")
			}).
			on("NESTED_TURN", func(context.Context, int, []providers.Message) (*providers.LLMResponse, error) {
				return textResponse("nested done")
			})
		al, _ := newTurnTestLoop(t, prov, false)
		var nested atomic.Value
		al.RegisterTool(&funcTool{name: "nested_turn", fn: func(ctx context.Context, _ map[string]any) *tools.ToolResult {
			response, err := al.ProcessDirectWithChannel(ctx, "NESTED_TURN", session, "telegram", "chat-1", false)
			if err != nil {
				return tools.ErrorResult(err.Error())
			}
			nested.Store(response)
			return tools.SilentResult(response)
		}})

		res := awaitTurn(t, "a turn whose tool starts another turn on the same session (deadlock?)",
			startTurn(context.Background(), al, "OUTER_TURN", session))
		if res.err != nil || res.response != "outer done" {
			t.Fatalf("outer turn = (%q, %v), want (\"outer done\", nil)", res.response, res.err)
		}
		if got, _ := nested.Load().(string); got != "nested done" {
			t.Errorf("nested turn returned %q, want \"nested done\"", got)
		}
	})
}

// A sub-turn runs on its own ephemeral session under its own key and takes no
// session lock, so it cannot deadlock against the parent that is blocked
// waiting for it. A synchronous one may even re-enter the parent's lock - it
// is still working on the parent's behalf while the parent waits. An async one
// may not: it can outlive the parent's turn.
func TestSubTurn_NoDeadlockWithParentLock(t *testing.T) {
	const session = "agent:main:parent-with-child"

	type probe struct {
		holdsParentLock bool
		turnID          string
	}
	var (
		parentTurnID atomic.Value
		syncProbe    atomic.Pointer[probe]
		asyncProbe   atomic.Pointer[probe]
		asyncProbed  = newSignal()
		asyncDone    = newSignal()
	)

	prov := newRoutedProvider().
		on("PARENT_TURN", func(ctx context.Context, call int, _ []providers.Message) (*providers.LLMResponse, error) {
			switch call {
			case 1:
				return toolCallResponse("spawn_sync_child", map[string]any{})
			case 2:
				return toolCallResponse("spawn_async_child", map[string]any{})
			default:
				// Keep the parent's turn (and its lock) alive until the
				// async child has looked at its context.
				select {
				case <-asyncProbed.fired():
					return textResponse("parent done")
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
		}).
		on("CHILD_INLINE", func(_ context.Context, call int, _ []providers.Message) (*providers.LLMResponse, error) {
			if call == 1 {
				return toolCallResponse("probe_sync", map[string]any{})
			}
			return textResponse("sync child done")
		}).
		on("CHILD_BACKGROUND", func(_ context.Context, call int, _ []providers.Message) (*providers.LLMResponse, error) {
			if call == 1 {
				return toolCallResponse("probe_async", map[string]any{})
			}
			return textResponse("async child done")
		})
	al, _ := newTurnTestLoop(t, prov, false)

	al.RegisterTool(&funcTool{name: "spawn_sync_child", fn: func(ctx context.Context, _ map[string]any) *tools.ToolResult {
		parentTurnID.Store(tools.TurnID(ctx))
		res, err := SpawnSubTurn(ctx, SubTurnConfig{Model: "char-model", SystemPrompt: "CHILD_INLINE work"})
		if err != nil {
			return tools.ErrorResult(err.Error())
		}
		return res
	}})
	al.RegisterTool(&funcTool{name: "spawn_async_child", fn: func(ctx context.Context, _ map[string]any) *tools.ToolResult {
		spawnCtx := context.WithoutCancel(ctx) // as the spawn tool does
		go func() {
			defer asyncDone.fire()
			_, _ = SpawnSubTurn(spawnCtx, SubTurnConfig{Model: "char-model", SystemPrompt: "CHILD_BACKGROUND work", Async: true})
		}()
		return tools.SilentResult("spawned")
	}})
	al.RegisterTool(&funcTool{name: "probe_sync", fn: func(ctx context.Context, _ map[string]any) *tools.ToolResult {
		syncProbe.Store(&probe{holdsParentLock: holdsSessionLock(ctx, session), turnID: tools.TurnID(ctx)})
		// Take the parent's session lock from inside the child. If the child
		// were treated as a stranger this would wait for the parent, which is
		// waiting for the child.
		if err := al.WithSessionLockContext(ctx, session, func(context.Context) error { return nil }); err != nil {
			return tools.ErrorResult(err.Error())
		}
		return tools.SilentResult("probed")
	}})
	al.RegisterTool(&funcTool{name: "probe_async", fn: func(ctx context.Context, _ map[string]any) *tools.ToolResult {
		asyncProbe.Store(&probe{holdsParentLock: holdsSessionLock(ctx, session), turnID: tools.TurnID(ctx)})
		asyncProbed.fire()
		return tools.SilentResult("probed")
	}})

	res := awaitTurn(t, "a turn with sub-turns (deadlock against the parent's session lock?)",
		startTurn(context.Background(), al, "PARENT_TURN", session))
	// The async child's result is appended to the parent's reply if it arrives
	// before the parent's final poll, so only the start of the reply is fixed.
	if res.err != nil || !strings.HasPrefix(res.response, "parent done") {
		t.Fatalf("parent turn = (%q, %v), want a reply starting \"parent done\" and no error", res.response, res.err)
	}
	asyncDone.wait(t, "the async sub-turn to finish")

	wantTurnID, _ := parentTurnID.Load().(string)
	if wantTurnID == "" {
		t.Fatal("the parent turn's tools saw no turn ID")
	}

	sp := syncProbe.Load()
	if sp == nil {
		t.Fatal("the synchronous sub-turn never ran its tool")
	}
	if !sp.holdsParentLock {
		t.Error("a synchronous sub-turn does not carry its parent's session lock: re-entering it would deadlock")
	}
	if sp.turnID != wantTurnID {
		t.Errorf("synchronous sub-turn has turn ID %q, want its parent's %q", sp.turnID, wantTurnID)
	}

	ap := asyncProbe.Load()
	if ap == nil {
		t.Fatal("the async sub-turn never ran its tool")
	}
	if ap.holdsParentLock {
		t.Error("an async sub-turn claims its parent's session lock, which it can outlive")
	}
	if ap.turnID != wantTurnID {
		t.Errorf("async sub-turn has turn ID %q, want its parent's %q", ap.turnID, wantTurnID)
	}

	// The children worked on their own stores: nothing of theirs is in the
	// parent's session.
	for _, m := range al.GetRegistry().GetDefaultAgent().Sessions.GetHistory(session) {
		if strings.Contains(m.Content, "CHILD_INLINE work") || strings.Contains(m.Content, "CHILD_BACKGROUND work") {
			t.Errorf("a sub-turn's prompt was written to the parent's session: %+v", m)
		}
	}
	if n := al.sessionLocks.size(); n != 0 {
		t.Errorf("session lock map has %d entries with no turn running, want 0", n)
	}
}

// ====================== turn registry ======================

// Root turns are registered by turn ID. When they were keyed by session key,
// a second turn on a session replaced the first one's entry, and the first one
// finishing removed the second's - after which neither could be found, let
// alone cancelled.
func TestActiveTurnStates_TwoTurnsSameSessionDoNotCollide(t *testing.T) {
	t.Run("registry", func(t *testing.T) {
		var reg turnRegistry
		first := &turnState{turnID: "t1", sessionKey: "heartbeat"}
		second := &turnState{turnID: "t2", sessionKey: "heartbeat"}
		other := &turnState{turnID: "t3", sessionKey: "elsewhere"}
		reg.register(first)
		reg.register(second)
		reg.register(other)

		if got := reg.forSession("heartbeat"); len(got) != 2 || got[0] != first || got[1] != second {
			t.Fatalf("forSession(heartbeat) = %v, want [first second]", got)
		}
		if got, orphaned := reg.cancelTargets("", true); len(got) != 3 || len(orphaned) != 0 {
			t.Fatalf("cancelTargets(all) = %d registered, %d orphaned; want 3, 0", len(got), len(orphaned))
		}
		if got, _ := reg.cancelTargets("elsewhere", false); len(got) != 1 || got[0] != other {
			t.Fatalf("cancelTargets(elsewhere) = %v, want [other]", got)
		}

		// A sub-turn is found through its root. While the root is registered
		// it is not listed a second time; once the root has returned, it is
		// what stands for the sub-turns it left running - once, however many
		// there are, and only for its own session.
		child := &turnState{turnID: "c1", parentTurnState: other}
		grandchild := &turnState{turnID: "c2", parentTurnState: child}
		reg.addSubTurn(child)
		reg.addSubTurn(grandchild)
		if _, orphaned := reg.cancelTargets("", true); len(orphaned) != 0 {
			t.Fatalf("a registered root was also listed as orphaned: %v", orphaned)
		}
		reg.unregister(other)
		if got, orphaned := reg.cancelTargets("elsewhere", false); len(got) != 0 || len(orphaned) != 1 || orphaned[0] != other {
			t.Fatalf("cancelTargets(elsewhere) after the root returned = %v registered, %v orphaned; want none, [other]",
				got, orphaned)
		}
		if _, orphaned := reg.cancelTargets("heartbeat", false); len(orphaned) != 0 {
			t.Fatalf("another session's cancel picked up sub-turns that are not its own: %v", orphaned)
		}
		reg.removeSubTurn(child)
		reg.removeSubTurn(grandchild)
		if _, orphaned := reg.cancelTargets("", true); len(orphaned) != 0 || len(reg.subTurns) != 0 {
			t.Fatalf("sub-turns that returned are still tracked: %v", orphaned)
		}
		reg.register(other)

		// Neither is running yet: the first registered is reported. Once one
		// holds the session, that one is.
		if got := reg.activeForSession("heartbeat"); got != first {
			t.Errorf("activeForSession = %v, want the first registered", got.turnID)
		}
		second.setHistorySnapshot(0)
		if got := reg.activeForSession("heartbeat"); got != second {
			t.Errorf("activeForSession = %v, want the turn that has started", got.turnID)
		}

		reg.unregister(first)
		reg.unregister(first) // twice is harmless
		if got := reg.forSession("heartbeat"); len(got) != 1 || got[0] != second {
			t.Fatalf("after the first turn left, forSession(heartbeat) = %v, want [second]", got)
		}
		reg.unregister(second)
		reg.unregister(other)
		if len(reg.byID) != 0 || len(reg.bySession) != 0 {
			t.Errorf("registry not empty after every turn left: %d by ID, %d sessions", len(reg.byID), len(reg.bySession))
		}

		// A caller-supplied ID reused for two overlapping turns: the second
		// is told apart rather than replacing the first.
		a := &turnState{turnID: "dup", sessionKey: "s"}
		b := &turnState{turnID: "dup", sessionKey: "s"}
		reg.register(a)
		reg.register(b)
		if a.turnID != "dup" || b.turnID == "dup" || b.turnID == "" {
			t.Errorf("turn IDs after registering a duplicate: %q and %q", a.turnID, b.turnID)
		}
		reg.unregister(a)
		if got := reg.forSession("s"); len(got) != 1 || got[0] != b {
			t.Errorf("unregistering one turn of a duplicated ID removed the wrong entry: %v", got)
		}
	})

	t.Run("turns", func(t *testing.T) {
		const session = "agent:main:two-turns"
		firstInLLM := newSignal()
		prov := newRoutedProvider().
			on("FIRST_TURN", func(ctx context.Context, _ int, _ []providers.Message) (*providers.LLMResponse, error) {
				firstInLLM.fire()
				return blockUntilCancelled(ctx)
			}).
			on("LATER_TURN", func(context.Context, int, []providers.Message) (*providers.LLMResponse, error) {
				return textResponse("a later turn ran while the first still held the session")
			})
		al, _ := newTurnTestLoop(t, prov, false)

		first := startTurn(context.Background(), al, "FIRST_TURN", session)
		firstInLLM.wait(t, "the first turn to block in the LLM")
		running := al.GetActiveTurn(session)
		if running == nil {
			t.Fatal("the running turn is not registered")
		}

		secondCtx, cancelSecond := context.WithCancel(context.Background())
		defer cancelSecond()
		second := startTurn(secondCtx, al, "LATER_TURN", session)
		waitUntil(t, "the second turn to queue for the session lock", func() bool {
			return sessionLockRefs(al, session) == 2
		})

		// Both are registered, as separate turns; the session's active turn
		// is still the one that is running.
		turns := al.activeTurnStates.forSession(session)
		if len(turns) != 2 || turns[0].turnID == turns[1].turnID {
			t.Fatalf("session has %d registered turn(s), want 2 distinct ones", len(turns))
		}
		if got := al.GetActiveTurn(session); got == nil || got.TurnID != running.TurnID {
			t.Fatalf("GetActiveTurn = %+v, want the running turn %s", got, running.TurnID)
		}

		// The second one leaving takes only itself out.
		cancelSecond()
		if res := awaitTurn(t, "the cancelled second turn", second); !errors.Is(res.err, context.Canceled) {
			t.Fatalf("second turn returned (%q, %v), want context.Canceled", res.response, res.err)
		}
		if got := al.GetActiveTurn(session); got == nil || got.TurnID != running.TurnID {
			t.Fatalf("after the second turn left, GetActiveTurn = %+v, want the running turn %s: "+
				"one turn's exit removed another's entry", got, running.TurnID)
		}

		// A running turn and a queued one are both reachable by session.
		third := startTurn(context.Background(), al, "LATER_TURN", session)
		waitUntil(t, "the third turn to queue for the session lock", func() bool {
			return sessionLockRefs(al, session) == 2
		})
		if n := al.CancelTurnsForSession(session); n != 2 {
			t.Errorf("CancelTurnsForSession signalled %d turns, want 2 (one running, one queued)", n)
		}
		for name, turn := range map[string]<-chan turnResult{"first": first, "third": third} {
			if res := awaitTurn(t, "the "+name+" turn", turn); !errors.Is(res.err, context.Canceled) {
				t.Errorf("%s turn returned (%q, %v), want context.Canceled", name, res.response, res.err)
			}
		}
		if n := prov.callCount("LATER_TURN"); n != 0 {
			t.Errorf("a queued turn reached the LLM %d time(s) before the running one finished", n)
		}
		if got := al.GetActiveTurn(session); got != nil {
			t.Errorf("a turn is still registered after every turn returned: %+v", got)
		}
		if n := al.sessionLocks.size(); n != 0 {
			t.Errorf("session lock map has %d entries with no turn running, want 0", n)
		}
	})
}

// WithSessionLock is the session lock for writers that are not turns: it
// waits for a running turn on the session, and a turn waits for it.
func TestWithSessionLock_ExcludesTurnsOnTheSession(t *testing.T) {
	const session = "agent:main:external-writer"
	prov := newRoutedProvider().
		on("WAITING_TURN", func(context.Context, int, []providers.Message) (*providers.LLMResponse, error) {
			return textResponse("turn done")
		})
	al, _ := newTurnTestLoop(t, prov, false)

	inside := newSignal()
	release := newSignal()
	writerDone := make(chan error, 1)
	go func() {
		writerDone <- al.WithSessionLock(session, func() error {
			inside.fire()
			<-release.fired()
			return nil
		})
	}()
	inside.wait(t, "the writer to take the session lock")

	turn := startTurn(context.Background(), al, "WAITING_TURN", session)
	waitUntil(t, "the turn to queue behind the writer", func() bool {
		return sessionLockRefs(al, session) == 2
	})
	if n := prov.callCount("WAITING_TURN"); n != 0 {
		t.Fatalf("the turn reached the LLM %d time(s) while a writer held its session", n)
	}

	release.fire()
	select {
	case err := <-writerDone:
		if err != nil {
			t.Fatalf("WithSessionLock: %v", err)
		}
	case <-time.After(testWait):
		t.Fatal("WithSessionLock did not return")
	}
	if res := awaitTurn(t, "the turn", turn); res.err != nil || res.response != "turn done" {
		t.Fatalf("turn = (%q, %v), want (\"turn done\", nil)", res.response, res.err)
	}

	// fn's error is passed through, and a cancelled context gives up waiting
	// without running fn.
	wantErr := errors.New("writer failed")
	if err := al.WithSessionLock(session, func() error { return wantErr }); !errors.Is(err, wantErr) {
		t.Errorf("WithSessionLock returned %v, want fn's error", err)
	}
	dead, cancel := context.WithCancel(context.Background())
	cancel()
	ran := false
	err := al.WithSessionLockContext(dead, session, func(context.Context) error {
		ran = true
		return nil
	})
	if !errors.Is(err, context.Canceled) || ran {
		t.Errorf("WithSessionLockContext on a cancelled context: err = %v, fn ran = %v", err, ran)
	}
}
