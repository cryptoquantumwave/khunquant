package agent

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/providers"
	"github.com/cryptoquantumwave/khunquant/pkg/routing"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

// scriptProvider is a provider whose answer is computed by fn from the request,
// safe for the concurrent turns -race tests run. It records every request.
type scriptProvider struct {
	fn func(ctx context.Context, call int, msgs []providers.Message, model string) (*providers.LLMResponse, error)

	mu     sync.Mutex
	calls  int
	msgs   [][]providers.Message
	defs   [][]providers.ToolDefinition
	models []string
}

func (p *scriptProvider) Chat(
	ctx context.Context,
	messages []providers.Message,
	defs []providers.ToolDefinition,
	model string,
	_ map[string]any,
) (*providers.LLMResponse, error) {
	p.mu.Lock()
	n := p.calls
	p.calls++
	p.msgs = append(p.msgs, append([]providers.Message(nil), messages...))
	p.defs = append(p.defs, append([]providers.ToolDefinition(nil), defs...))
	p.models = append(p.models, model)
	p.mu.Unlock()
	return p.fn(ctx, n, messages, model)
}

func (p *scriptProvider) GetDefaultModel() string { return "char-model" }

func (p *scriptProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// script answers call i with responses[i], then "done".
func script(responses ...providers.LLMResponse) *scriptProvider {
	return &scriptProvider{fn: func(_ context.Context, n int, _ []providers.Message, _ string) (*providers.LLMResponse, error) {
		if n < len(responses) {
			r := responses[n]
			return &r, nil
		}
		return &providers.LLMResponse{Content: "done"}, nil
	}}
}

func respondCall(id string, args map[string]any) providers.ToolCall {
	return providers.ToolCall{ID: id, Type: "function", Name: tools.NameRespondProactive, Arguments: args}
}

func callsResponse(calls ...providers.ToolCall) providers.LLMResponse {
	return providers.LLMResponse{ToolCalls: calls}
}

func respondResponse(args map[string]any) providers.LLMResponse {
	return callsResponse(respondCall("r1", args))
}

// proactiveLoop is an agent loop whose owner is a Telegram DM.
func proactiveLoop(t *testing.T, prov providers.LLMProvider, mutate ...func(*config.Config)) *AgentLoop {
	t.Helper()
	al, _ := ownerLoop(t, prov, mutate...)
	return al
}

// readTool registers a read-class tool (get_ticker) that returns result.
func readTool(al *AgentLoop, name string, result func(ctx context.Context) *tools.ToolResult) {
	al.RegisterTool(&funcTool{name: name, fn: func(ctx context.Context, _ map[string]any) *tools.ToolResult {
		return result(ctx)
	}})
}

func cand(id string, sev proactive.Severity, protective bool) proactive.Candidate {
	return proactive.Candidate{
		ID: id, TriggerID: "price_cross:" + id, Severity: sev, Protective: protective,
		Subject: "BTC/USDT", Title: "BTC crossed a level " + id, Trust: proactive.TrustSystem,
	}
}

func proactiveReq(cands ...proactive.Candidate) ProactiveTurnRequest {
	return ProactiveTurnRequest{
		Kind: proactive.TurnProactive, Prompt: "Evidence is shown to the owner separately.",
		Candidates: cands, Deadline: 20 * time.Second,
	}
}

func mustRun(t *testing.T, al *AgentLoop, req ProactiveTurnRequest) ProactiveTurnResult {
	t.Helper()
	res, err := al.RunProactiveTurn(context.Background(), req)
	if err != nil {
		t.Fatalf("RunProactiveTurn: %v", err)
	}
	return res
}

func ownerSessionKeyFor(cfg *config.Config) string {
	return routing.NewRouteResolver(cfg).ResolveRoute(routing.RouteInput{
		Channel: "telegram", Peer: &routing.RoutePeer{Kind: "direct", ID: "424242"},
	}).SessionKey
}

// ====================== the eight spec tests ======================

func TestProactiveTurn_IsolatedSession_DoesNotTouchMain(t *testing.T) {
	prov := script(respondResponse(map[string]any{"decision": "silent"}))
	al, cfg := ownerLoop(t, prov)
	sessions := al.GetRegistry().GetDefaultAgent().Sessions
	ownerKey := ownerSessionKeyFor(cfg)
	const mainKey = "agent:main:main"

	for _, key := range []string{ownerKey, mainKey} {
		sessions.AddMessage(key, "user", "hello "+key)
		sessions.AddMessage(key, "assistant", "hi back")
	}
	beforeOwner := sessions.GetHistory(ownerKey)
	beforeMain := sessions.GetHistory(mainKey)

	mustRun(t, al, proactiveReq(cand("c1", proactive.SeverityNotice, false)))

	if got := sessions.GetHistory(ownerKey); !reflect.DeepEqual(got, beforeOwner) {
		t.Errorf("owner session changed:\n got %+v\nwant %+v", got, beforeOwner)
	}
	if got := sessions.GetHistory(mainKey); !reflect.DeepEqual(got, beforeMain) {
		t.Errorf("main session changed:\n got %+v\nwant %+v", got, beforeMain)
	}
	// The turn's own messages went to its own session, and nowhere else.
	const proactiveKey = "agent:main:proactive"
	if h := sessions.GetHistory(proactiveKey); len(h) == 0 {
		t.Error("the proactive session holds nothing: the turn wrote elsewhere")
	}
	// Owner, main and proactive: the turn created no other session. (The store
	// lists sanitised file names, so count rather than compare keys.)
	if n := len(sessions.ListSessions()); n != 3 {
		t.Errorf("%d sessions, want 3: %v", n, sessions.ListSessions())
	}
	// The prompt the model saw carried no history of either session.
	for _, m := range prov.msgs[0] {
		if strings.Contains(m.Content, "hello "+ownerKey) || strings.Contains(m.Content, "hello "+mainKey) {
			t.Error("a session's history reached the proactive turn")
		}
	}
}

func TestProactiveTurn_HistoryTruncatedToTwenty(t *testing.T) {
	prov := script()
	prov.fn = func(_ context.Context, _ int, msgs []providers.Message, _ string) (*providers.LLMResponse, error) {
		if msgs[len(msgs)-1].Role == "tool" {
			return &providers.LLMResponse{Content: "done"}, nil
		}
		r := respondResponse(map[string]any{"decision": "silent"})
		return &r, nil
	}
	al := proactiveLoop(t, prov)
	for i := 0; i < 15; i++ {
		mustRun(t, al, proactiveReq())
	}
	if n := len(al.GetRegistry().GetDefaultAgent().Sessions.GetHistory("agent:main:proactive")); n > 20 || n == 0 {
		t.Errorf("proactive history = %d messages, want 1..20", n)
	}
}

func TestProactiveTurn_DecisionCollected(t *testing.T) {
	prov := script(
		providers.LLMResponse{
			ToolCalls: []providers.ToolCall{respondCall("r1", map[string]any{
				"decision": "notify", "severity": "warning",
				"text":      "  BTC moved through a level you watch; consider reviewing the hedge.  ",
				"dedup_key": "BTC:Cross Level", "candidate_ids": []any{"c1", "bogus"},
			})},
			Usage: &providers.UsageInfo{PromptTokens: 120, CompletionTokens: 15},
		},
		providers.LLMResponse{Content: "done", Usage: &providers.UsageInfo{PromptTokens: 150, CompletionTokens: 5}},
	)
	al := proactiveLoop(t, prov)
	res := mustRun(t, al, proactiveReq(cand("c1", proactive.SeverityNotice, false)))

	want := []proactive.Decision{{
		Kind: proactive.DecideNotify, Severity: proactive.SeverityWarning,
		Text:         "BTC moved through a level you watch; consider reviewing the hedge.",
		CandidateIDs: []string{"c1"}, DedupKey: "btc:cross_level",
	}}
	if !reflect.DeepEqual(res.Decisions, want) {
		t.Fatalf("decisions = %+v\nwant %+v", res.Decisions, want)
	}
	if res.ContractViolated {
		t.Error("ContractViolated on a turn that used the tool")
	}
	if res.FinalText != "done" {
		t.Errorf("FinalText = %q", res.FinalText)
	}
	if res.InputTokens != 270 || res.OutputTokens != 20 {
		t.Errorf("tokens = %d in / %d out, want 270 / 20", res.InputTokens, res.OutputTokens)
	}
	if res.Model != "char-model" {
		t.Errorf("Model = %q", res.Model)
	}
}

func TestProactiveTurn_NoToolCall_TextBecomesNotify_ContractViolated(t *testing.T) {
	long := strings.Repeat("ก", 2500)
	for name, text := range map[string]string{"short": "BTC is moving, take a look.", "long": long} {
		t.Run(name, func(t *testing.T) {
			al := proactiveLoop(t, script(providers.LLMResponse{Content: text}))
			res := mustRun(t, al, proactiveReq(cand("c1", proactive.SeverityWarning, true)))

			if !res.ContractViolated {
				t.Error("ContractViolated = false")
			}
			if len(res.Decisions) != 1 {
				t.Fatalf("decisions = %+v", res.Decisions)
			}
			d := res.Decisions[0]
			if d.Kind != proactive.DecideNotify || d.Severity != proactive.SeverityNotice {
				t.Errorf("decision = %+v, want a notice-level notify", d)
			}
			if n := len([]rune(d.Text)); n == 0 || n > tools.MaxProactiveTextRunes {
				t.Errorf("decision text has %d runes, want 1..%d", n, tools.MaxProactiveTextRunes)
			}
			if name == "short" && d.Text != text {
				t.Errorf("text = %q", d.Text)
			}
			if res.FinalText != text {
				t.Error("FinalText is not the model's text")
			}
		})
	}
}

func TestProactiveTurn_HEARTBEAT_OK_IsSilent(t *testing.T) {
	for _, final := range []string{"HEARTBEAT_OK", "  HEARTBEAT_OK\n", ""} {
		al := proactiveLoop(t, script(providers.LLMResponse{Content: final}))
		res := mustRun(t, al, proactiveReq())
		if res.ContractViolated {
			t.Errorf("%q: ContractViolated", final)
		}
		if len(res.Decisions) != 1 || res.Decisions[0].Kind != proactive.DecideSilent {
			t.Errorf("%q: decisions = %+v, want one silent", final, res.Decisions)
		}
	}
	// "HEARTBEAT_OK" with anything else is prose, not the all-clear.
	al := proactiveLoop(t, script(providers.LLMResponse{Content: "HEARTBEAT_OK but BTC dropped"}))
	if res := mustRun(t, al, proactiveReq()); !res.ContractViolated || res.Decisions[0].Kind != proactive.DecideNotify {
		t.Errorf("mixed text was treated as the all-clear: %+v", res)
	}
}

func TestProactiveTurn_SeverityCapped(t *testing.T) {
	cases := []struct {
		name  string
		cands []proactive.Candidate
		ask   string
		want  proactive.Severity
	}{
		{"info candidate", []proactive.Candidate{cand("c1", proactive.SeverityInfo, false)}, "critical", proactive.SeverityNotice},
		{"notice candidate", []proactive.Candidate{cand("c1", proactive.SeverityNotice, false)}, "critical", proactive.SeverityWarning},
		{"warning, not protective", []proactive.Candidate{cand("c1", proactive.SeverityWarning, false)}, "critical", proactive.SeverityWarning},
		{"protective", []proactive.Candidate{cand("c1", proactive.SeverityWarning, true)}, "critical", proactive.SeverityCritical},
		{"no candidates", nil, "critical", proactive.SeverityWarning},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			al := proactiveLoop(t, script(respondResponse(map[string]any{
				"decision": "notify", "severity": tc.ask, "text": "heads up",
			})))
			res := mustRun(t, al, proactiveReq(tc.cands...))
			if got := res.Decisions[0].Severity; got != tc.want {
				t.Errorf("severity = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestProactiveTurn_Deadline(t *testing.T) {
	prov := &scriptProvider{fn: func(ctx context.Context, n int, _ []providers.Message, _ string) (*providers.LLMResponse, error) {
		if n == 0 {
			r := respondResponse(map[string]any{"decision": "notify", "text": "an early decision"})
			return &r, nil
		}
		<-ctx.Done() // the model hangs
		return nil, ctx.Err()
	}}
	al := proactiveLoop(t, prov)

	start := time.Now()
	req := proactiveReq(cand("c1", proactive.SeverityNotice, false))
	req.Deadline = 300 * time.Millisecond
	res, err := al.RunProactiveTurn(context.Background(), req)
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want one wrapping context.DeadlineExceeded", err)
	}
	if len(res.Decisions) != 0 {
		t.Errorf("a timed-out turn returned decisions: %+v", res.Decisions)
	}
	if elapsed > 5*time.Second {
		t.Errorf("returned after %s: the retry backoff outlived the deadline", elapsed)
	}
}

func TestProactiveTurn_ProviderErrorIsReturned(t *testing.T) {
	boom := errors.New("provider down")
	al := proactiveLoop(t, &scriptProvider{fn: func(context.Context, int, []providers.Message, string) (*providers.LLMResponse, error) {
		return nil, boom
	}})
	res, err := al.RunProactiveTurn(context.Background(), proactiveReq())
	if err == nil || len(res.Decisions) != 0 {
		t.Fatalf("err = %v, decisions = %+v", err, res.Decisions)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Error("a provider error was reported as a deadline")
	}
}

func TestProactiveTurn_UsesLightModelWhenConfigured(t *testing.T) {
	done := func(model *string) *scriptProvider {
		return &scriptProvider{fn: func(_ context.Context, _ int, msgs []providers.Message, m string) (*providers.LLMResponse, error) {
			*model = m
			if msgs[len(msgs)-1].Role == "tool" {
				return &providers.LLMResponse{Content: "done"}, nil
			}
			r := respondResponse(map[string]any{"decision": "silent"})
			return &r, nil
		}}
	}
	var mainModel, lightModel string
	mainProv, lightProv := done(&mainModel), done(&lightModel)
	al := proactiveLoop(t, mainProv)
	agent := al.GetRegistry().GetDefaultAgent()
	agent.LightCandidates = []providers.FallbackCandidate{{Provider: "openai", Model: "light-model"}}
	agent.LightProvider = lightProv
	// A router that would send every message to the light model: a turn that
	// does not ask for it must still stay on the normal model.
	agent.Router = routing.New(routing.RouterConfig{LightModel: "light-model", Threshold: 0.99})

	req := proactiveReq(cand("c1", proactive.SeverityNotice, false))
	req.Model = proactive.ModelTierLight
	res := mustRun(t, al, req)
	if res.Model != "light-model" {
		t.Errorf("Model = %q, want light-model", res.Model)
	}
	if lightProv.callCount() != 2 || mainProv.callCount() != 0 {
		t.Errorf("light calls %d, main calls %d: want the light model for every iteration", lightProv.callCount(), mainProv.callCount())
	}
	if lightModel != "light-model" {
		t.Errorf("the provider was asked for model %q", lightModel)
	}

	// The zero tier and an explicit primary are the normal model, whatever the
	// router would say and although a light model exists.
	for _, tier := range []proactive.ModelTier{"", proactive.ModelTierPrimary} {
		before := mainProv.callCount()
		req.Model = tier
		res = mustRun(t, al, req)
		if res.Model != "char-model" || mainProv.callCount() != before+2 || lightProv.callCount() != 2 {
			t.Errorf("tier %q: Model %q, main %d, light %d", tier, res.Model, mainProv.callCount(), lightProv.callCount())
		}
	}

	// No light model configured: the request is served by the normal one.
	agent.LightCandidates, agent.LightProvider, agent.Router = nil, nil, nil
	req.Model = proactive.ModelTierLight
	before := mainProv.callCount()
	if res = mustRun(t, al, req); res.Model != "char-model" || mainProv.callCount() != before+2 {
		t.Errorf("no light model: Model %q, main calls %d (was %d)", res.Model, mainProv.callCount(), before)
	}
}

// An unknown tier is refused before anything runs: it is not quietly served by
// either model.
func TestProactiveTurn_UnknownModelTierIsAnError(t *testing.T) {
	prov := script(respondResponse(map[string]any{"decision": "silent"}))
	al := proactiveLoop(t, prov)

	req := proactiveReq(cand("c1", proactive.SeverityNotice, false))
	req.Model = "turbo"
	_, err := al.RunProactiveTurn(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "turbo") {
		t.Fatalf("err = %v, want it to name the unknown tier", err)
	}
	if prov.callCount() != 0 {
		t.Errorf("a turn ran on an unknown tier (%d provider calls)", prov.callCount())
	}
}

// A user turn is routed exactly as before: ModelOverride is not set for it.
func TestUserTurn_RoutingUnchangedByModelOverride(t *testing.T) {
	var model string
	prov := &scriptProvider{fn: func(_ context.Context, _ int, _ []providers.Message, m string) (*providers.LLMResponse, error) {
		model = m
		return &providers.LLMResponse{Content: "ok"}, nil
	}}
	light := &scriptProvider{fn: prov.fn}
	al := proactiveLoop(t, prov)
	agent := al.GetRegistry().GetDefaultAgent()
	agent.LightCandidates = []providers.FallbackCandidate{{Provider: "openai", Model: "light-model"}}
	agent.LightProvider = light
	agent.Router = routing.New(routing.RouterConfig{LightModel: "light-model", Threshold: 0.99})

	if _, err := al.ProcessDirect(context.Background(), "hi", "sess-routed"); err != nil {
		t.Fatal(err)
	}
	if model != "light-model" || light.callCount() != 1 || prov.callCount() != 0 {
		t.Errorf("a routed user turn: model %q, light %d, main %d", model, light.callCount(), prov.callCount())
	}
}

func TestProactiveTurn_MessageToolUnavailable(t *testing.T) {
	prov := script(
		callsResponse(
			providers.ToolCall{ID: "m1", Type: "function", Name: tools.NameMessage, Arguments: map[string]any{"value": "hi"}},
			providers.ToolCall{ID: "w1", Type: "function", Name: tools.NameWriteFile, Arguments: map[string]any{"value": "x"}},
		),
		respondResponse(map[string]any{"decision": "silent"}),
	)
	al := proactiveLoop(t, prov)
	recs := registerRecording(al, tools.NameMessage, tools.NameWriteFile, tools.NameGetTicker)

	res := mustRun(t, al, proactiveReq())

	for name, rec := range recs {
		if n := len(rec.calls()); n != 0 {
			t.Errorf("%s executed %d time(s) in a proactive turn", name, n)
		}
	}
	defs := defNames(prov.defs[0])
	for _, denied := range []string{tools.NameMessage, tools.NameWriteFile, tools.NameSendFile} {
		if hasName(defs, denied) {
			t.Errorf("a proactive turn was shown %s", denied)
		}
	}
	if !hasName(defs, tools.NameGetTicker) || !hasName(defs, tools.NameRespondProactive) {
		t.Errorf("a proactive turn lost a read tool or respond_proactive: %v", defs)
	}
	if !anyMessageContains(prov.msgs[1], "not permitted in an autonomous (proactive) turn") {
		t.Errorf("the model was not told the call was refused")
	}
	if len(res.Decisions) != 1 || res.Decisions[0].Kind != proactive.DecideSilent {
		t.Errorf("decisions = %+v", res.Decisions)
	}

	// respond_proactive lived on the turn's clone only.
	if _, shared := al.GetRegistry().GetDefaultAgent().Tools.Get(tools.NameRespondProactive); shared {
		t.Error("respond_proactive leaked into the shared registry")
	}
	user := script(providers.LLMResponse{Content: "ok"})
	al2 := proactiveLoop(t, user)
	if _, err := al2.ProcessDirect(context.Background(), "hi", "after"); err != nil {
		t.Fatal(err)
	}
	if hasName(defNames(user.defs[0]), tools.NameRespondProactive) {
		t.Error("a user turn was shown respond_proactive")
	}
}

// ====================== delivery ======================

type asyncReadTool struct{ name string }

func (a *asyncReadTool) Name() string        { return a.name }
func (a *asyncReadTool) Description() string { return "async read" }
func (a *asyncReadTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

func (a *asyncReadTool) Execute(context.Context, map[string]any) *tools.ToolResult {
	return tools.SilentResult("sync")
}

func (a *asyncReadTool) ExecuteAsync(ctx context.Context, _ map[string]any, cb tools.AsyncCallback) *tools.ToolResult {
	cb(ctx, &tools.ToolResult{ForLLM: "async done", ForUser: "ASYNC-FOR-USER"})
	cb(ctx, &tools.ToolResult{ForLLM: "async failed", Err: errors.New("task failed")})
	return tools.AsyncResult("started")
}

// A proactive turn returns decisions and publishes nothing: every path in the
// loop that sends a tool's output, media, reasoning, interim text, retry notice
// or async result to the bus is exercised, on the owner's real (non-internal)
// channel, with a model that ends in free text.
func TestProactiveTurn_PublishesNothing(t *testing.T) {
	var step int
	var mu sync.Mutex
	prov := &scriptProvider{fn: func(_ context.Context, _ int, _ []providers.Message, _ string) (*providers.LLMResponse, error) {
		mu.Lock()
		defer mu.Unlock()
		step++
		switch step {
		case 1: // a context-length error: the loop tells the chat it is compressing
			return nil, errors.New("context_length_exceeded: too many tokens")
		case 2:
			return &providers.LLMResponse{
				Content:          "interim words before the tools",
				ReasoningContent: "private reasoning",
				ToolCalls: []providers.ToolCall{
					{ID: "t1", Type: "function", Name: tools.NameGetTicker, Arguments: map[string]any{}},
					{ID: "t2", Type: "function", Name: tools.NameGetMarkets, Arguments: map[string]any{}},
					{ID: "t3", Type: "function", Name: tools.NameGetTickers, Arguments: map[string]any{}},
				},
			}, nil
		}
		return &providers.LLMResponse{Content: "A closing note for the owner.", ReasoningContent: "more reasoning"}, nil
	}}
	al := proactiveLoop(t, prov)
	readTool(al, tools.NameGetTicker, func(context.Context) *tools.ToolResult {
		return &tools.ToolResult{ForLLM: "BTC 100", ForUser: "TOOL-FOR-USER: BTC 100"}
	})
	readTool(al, tools.NameGetMarkets, func(context.Context) *tools.ToolResult {
		return &tools.ToolResult{
			ForLLM: "chart", ForUser: "MEDIA-FOR-USER",
			Media: []string{"media://chart-1"}, ResponseHandled: true,
		}
	})
	al.RegisterTool(&asyncReadTool{name: tools.NameGetTickers})

	res := mustRun(t, al, proactiveReq(cand("c1", proactive.SeverityNotice, false)))
	time.Sleep(100 * time.Millisecond) // let any detached publisher run

	if n := len(al.bus.OutboundChan()); n != 0 {
		var got []string
		for len(al.bus.OutboundChan()) > 0 {
			got = append(got, (<-al.bus.OutboundChan()).Content)
		}
		t.Errorf("%d outbound message(s) published: %q", n, got)
	}
	if n := len(al.bus.OutboundMediaChan()); n != 0 {
		t.Errorf("%d outbound media message(s) published", n)
	}
	if n := len(al.bus.InboundChan()); n != 0 {
		t.Errorf("%d inbound (async follow-up) message(s) published", n)
	}
	if step < 3 {
		t.Fatalf("the script ended at step %d: the tool paths were not exercised", step)
	}
	// The text still comes back to the caller, as data.
	if len(res.Decisions) != 1 || !res.ContractViolated || res.Decisions[0].Text != "A closing note for the owner." {
		t.Errorf("result = %+v", res)
	}
	// And the turn left the "last active chat" alone.
	if got := al.State().GetLastChannel(); got != "" {
		t.Errorf("the proactive turn recorded %q as the last active channel", got)
	}
}

func TestProactiveTurn_ToolsSeeOwnerRouteOrInternalChannel(t *testing.T) {
	type seen struct{ channel, chatID, origin, sender string }
	run := func(t *testing.T, mutate ...func(*config.Config)) seen {
		t.Helper()
		var got seen
		var mu sync.Mutex
		prov := script(callsResponse(providers.ToolCall{ID: "t", Type: "function", Name: tools.NameGetTicker, Arguments: map[string]any{}}))
		al := proactiveLoop(t, prov, mutate...)
		readTool(al, tools.NameGetTicker, func(ctx context.Context) *tools.ToolResult {
			mu.Lock()
			got = seen{tools.ToolChannel(ctx), tools.ToolChatID(ctx), tools.ToolOrigin(ctx), tools.ToolSender(ctx)}
			mu.Unlock()
			return tools.SilentResult("x")
		})
		mustRun(t, al, proactiveReq())
		mu.Lock()
		defer mu.Unlock()
		return got
	}

	if got := run(t); got != (seen{"telegram", "424242", "proactive", "telegram:424242"}) {
		t.Errorf("owner configured: %+v", got)
	}
	noOwner := func(c *config.Config) { c.Proactive = config.ProactiveConfig{} }
	if got := run(t, noOwner); got != (seen{"system", "proactive", "proactive", ""}) {
		t.Errorf("no owner: %+v", got)
	}
}

// RunProactiveTurn starts the ceiling itself: a context that already carries a
// person's origin does not leak that origin's permissions into the turn.
func TestProactiveTurn_OverridesAnInheritedOrigin(t *testing.T) {
	prov := script(callsResponse(providers.ToolCall{ID: "o", Type: "function", Name: tools.NameCreateOrder, Arguments: map[string]any{"value": "1"}}))
	al := proactiveLoop(t, prov)
	rec := registerRecording(al, tools.NameCreateOrder)[tools.NameCreateOrder]

	ctx := tools.WithToolOrigin(context.Background(), tools.OriginRemote)
	if _, err := al.RunProactiveTurn(ctx, proactiveReq()); err != nil {
		t.Fatal(err)
	}
	if n := len(rec.calls()); n != 0 {
		t.Fatalf("create_order ran %d time(s) in a proactive turn started from a remote context", n)
	}
	if !anyMessageContains(prov.msgs[1], "not permitted in an autonomous (proactive) turn") {
		t.Error("the refusal does not name the proactive origin")
	}
}

func TestProactiveTurn_RejectsOtherKindsAndNesting(t *testing.T) {
	al := proactiveLoop(t, script())
	for _, kind := range []proactive.TurnKind{proactive.TurnUser, proactive.TurnSubTurn, proactive.TurnCronLegacy,
		proactive.TurnReflection, proactive.TurnApprovalFollowUp, "", "other"} {
		if _, err := al.RunProactiveTurn(context.Background(), ProactiveTurnRequest{Kind: kind}); err == nil {
			t.Errorf("kind %q accepted", kind)
		}
	}
	nested := withTurnState(context.Background(), &turnState{})
	if _, err := al.RunProactiveTurn(nested, proactiveReq()); err == nil {
		t.Error("a proactive turn started inside another turn")
	}
}

func TestProactiveTurn_MaxIterationsWithoutDecisionIsAnError(t *testing.T) {
	forever := &scriptProvider{fn: func(context.Context, int, []providers.Message, string) (*providers.LLMResponse, error) {
		return &providers.LLMResponse{ToolCalls: []providers.ToolCall{
			{ID: fmt.Sprintf("t%d", time.Now().UnixNano()), Type: "function", Name: tools.NameGetTicker, Arguments: map[string]any{}},
		}}, nil
	}}
	al := proactiveLoop(t, forever, func(c *config.Config) { c.Agents.Defaults.MaxToolIterations = 2 })
	readTool(al, tools.NameGetTicker, func(context.Context) *tools.ToolResult { return tools.SilentResult("x") })

	res, err := al.RunProactiveTurn(context.Background(), proactiveReq())
	if err == nil || !strings.Contains(err.Error(), "max_tool_iterations") {
		t.Fatalf("err = %v", err)
	}
	if len(res.Decisions) != 0 {
		t.Errorf("decisions = %+v", res.Decisions)
	}
}

func TestProactiveTurn_DecisionCapKeepsEightAndWarns(t *testing.T) {
	var calls []providers.ToolCall
	for i := 0; i < 11; i++ {
		calls = append(calls, respondCall(fmt.Sprintf("r%d", i), map[string]any{
			"decision": "notify", "text": fmt.Sprintf("topic %d", i),
		}))
	}
	al := proactiveLoop(t, script(callsResponse(calls...)))
	res := mustRun(t, al, proactiveReq())
	if len(res.Decisions) != proactive.MaxDecisionsPerTurn {
		t.Errorf("kept %d decisions, want %d", len(res.Decisions), proactive.MaxDecisionsPerTurn)
	}
}

// ====================== concurrency ======================

func TestProactiveTurn_ConcurrentTurnsDoNotShareDecisions(t *testing.T) {
	prov := &scriptProvider{fn: func(_ context.Context, _ int, msgs []providers.Message, _ string) (*providers.LLMResponse, error) {
		last := msgs[len(msgs)-1]
		if last.Role == "tool" {
			return &providers.LLMResponse{Content: "done"}, nil
		}
		// Each turn declares a decision about the candidate in its own request.
		var id string
		for _, want := range []string{"CAND-A", "CAND-B"} {
			if strings.Contains(last.Content, want) {
				id = want
			}
		}
		r := respondResponse(map[string]any{
			"decision": "notify", "text": "about " + id, "candidate_ids": []any{id},
		})
		return &r, nil
	}}
	al := proactiveLoop(t, prov)

	var wg sync.WaitGroup
	results := make([]ProactiveTurnResult, 8)
	errs := make([]error, 8)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := "CAND-A"
			if i%2 == 1 {
				id = "CAND-B"
			}
			results[i], errs[i] = al.RunProactiveTurn(context.Background(), proactiveReq(cand(id, proactive.SeverityNotice, false)))
		}()
	}
	wg.Wait()

	for i, res := range results {
		id := "CAND-A"
		if i%2 == 1 {
			id = "CAND-B"
		}
		if errs[i] != nil {
			t.Errorf("turn %d: %v", i, errs[i])
			continue
		}
		if len(res.Decisions) != 1 || res.Decisions[0].Text != "about "+id ||
			!reflect.DeepEqual(res.Decisions[0].CandidateIDs, []string{id}) {
			t.Errorf("turn %d got %+v, want one decision about %s", i, res.Decisions, id)
		}
	}
}

// An operator's require_approval on a read tool would make a person's turn send
// an approval request to the chat. A proactive turn must neither send it nor
// wait for it: the call is refused and the bus stays empty.
func TestProactiveTurn_ApprovalRequiredReadToolPublishesNothing(t *testing.T) {
	prov := script(
		callsResponse(providers.ToolCall{ID: "w", Type: "function", Name: tools.NameGetTicker, Arguments: map[string]any{}}),
		respondResponse(map[string]any{"decision": "silent"}),
	)
	al := proactiveLoop(t, prov, func(c *config.Config) {
		c.Tools.Policies = map[string]config.ToolPolicyConfig{tools.NameGetTicker: {RequireApproval: true}}
	})
	recs := registerRecording(al, tools.NameGetTicker)

	mustRun(t, al, proactiveReq())
	time.Sleep(50 * time.Millisecond)

	if n := len(recs[tools.NameGetTicker].calls()); n != 0 {
		t.Errorf("get_ticker ran %d time(s) without approval", n)
	}
	if n := len(al.bus.OutboundChan()); n != 0 {
		t.Errorf("%d outbound message(s) published (an approval request?)", n)
	}
	if !anyMessageContains(prov.msgs[1], "requires operator approval") {
		t.Error("the model was not told why the call was refused")
	}
}
