package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/providers"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex/sqlitextest"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

const intentMarker = "ZXQ-SUGGESTION-MARKER-9917"

// intentLoopConfig is the profile test config with the intent ledger on or off.
func intentLoopConfig(workspace string, on bool) *config.Config {
	cfg := profileTestConfig(workspace, true)
	cfg.Proactive.Enabled = on
	cfg.Proactive.Intents.Enabled = on
	return cfg
}

func newIntentLoop(t *testing.T, provider providers.LLMProvider, on bool) (*AgentLoop, string) {
	t.Helper()
	workspace := t.TempDir()
	t.Cleanup(func() { _ = sqlitex.CloseShared() })
	al := NewAgentLoop(intentLoopConfig(workspace, on), bus.NewMessageBus(), provider)
	t.Cleanup(al.Close)
	return al, workspace
}

func toolCall(id, name string, args map[string]any) providers.LLMResponse {
	return providers.LLMResponse{ToolCalls: []providers.ToolCall{{ID: id, Type: "function", Name: name, Arguments: args}}}
}

func watchCallArgs(title string, requested bool) map[string]any {
	return map[string]any{
		"type": "watch", "title": title, "user_requested": requested,
		"trigger": map[string]any{"kind": "price_cross", "params": map[string]any{"symbol": "SOL/USDT", "below": 120}},
	}
}

func allIntents(t *testing.T, workspace string) []intent.Intent {
	t.Helper()
	rows, err := profileStoreFor(t, workspace).ListIntents(context.Background(), intent.IntentFilter{})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// The acceptance criterion: the owner says it; the model calls create_intent;
// the result is one active watch, visible in list_intents and in the very next
// prompt's digest (inside the financial context's cache window).
func TestIntentAcceptance_OwnerSaysItOneActiveWatch(t *testing.T) {
	provider := &charScriptedProvider{responses: []providers.LLMResponse{
		toolCall("c1", "create_intent", watchCallArgs("SOL/USDT below 120", true)),
		{Content: "Noted - I will watch SOL."},
		toolCall("c2", "list_intents", map[string]any{}),
		{Content: "Here it is."},
	}}
	al, workspace := newIntentLoop(t, provider, true)

	if _, err := al.processMessage(context.Background(), profileTurn(profileOwner, "remind me if SOL goes under 120")); err != nil {
		t.Fatal(err)
	}
	rows := allIntents(t, workspace)
	if len(rows) != 1 || rows[0].Status != intent.StatusActive || rows[0].Source != intent.SourceExplicit ||
		rows[0].Type != intent.TypeWatch || rows[0].Provenance.SenderID != profileOwner {
		t.Fatalf("stored = %+v", rows)
	}
	if !anyMessageContains(provider.gotMsgs[1], "Active watch: SOL/USDT below 120") ||
		!anyMessageContains(provider.gotMsgs[1], "Nothing is watching for it yet") {
		t.Fatal("the model was not shown what was stored, and that it is not live yet")
	}

	// The next turn, minutes later, inside the 30 minute cache: the digest has it.
	if _, err := al.processMessage(context.Background(), profileTurn(profileOwner, "what are you watching?")); err != nil {
		t.Fatal(err)
	}
	if !anyMessageContains(provider.gotMsgs[2], "Intents (active, set by the owner):\n- watch: SOL/USDT below 120") {
		t.Fatalf("the next prompt's digest lacks the watch:\n%v", provider.gotMsgs[2][0].Content)
	}
	if !anyMessageContains(provider.gotMsgs[3], `"status":"active"`) || !anyMessageContains(provider.gotMsgs[3], "SOL/USDT below 120") {
		t.Fatal("list_intents did not show the active watch")
	}
}

// The same call from somebody else's chat is a suggestion: stored, counted in the
// digest, and its text in no prompt.
func TestIntentAcceptance_StrangersSuggestionIsNotActiveAndNotInThePrompt(t *testing.T) {
	provider := &charScriptedProvider{responses: []providers.LLMResponse{
		toolCall("c1", "create_intent", watchCallArgs("watch "+intentMarker, true)),
		{Content: "ok"},
	}}
	al, workspace := newIntentLoop(t, provider, true)
	if _, err := al.processMessage(context.Background(), profileTurn("telegram:555", "watch it")); err != nil {
		t.Fatal(err)
	}
	rows := allIntents(t, workspace)
	if len(rows) != 1 || rows[0].Status != intent.StatusProposed || rows[0].Source != intent.SourceInferred {
		t.Fatalf("stored = %+v", rows)
	}
	if _, err := al.processMessage(context.Background(), profileTurn(profileOwner, "hello")); err != nil {
		t.Fatal(err)
	}
	last := provider.gotMsgs[len(provider.gotMsgs)-1]
	if !anyMessageContains(last, "Suggested intents, not active: 1") {
		t.Fatal("the digest does not count the suggestion")
	}
	if anyMessageContains(last, intentMarker) {
		t.Fatal("a suggestion's text reached a prompt")
	}
}

// Through the real loop, an autonomous turn is not offered create_intent and is
// refused if it calls it: a proactive turn, a heartbeat and a scheduled job.
func TestIntentTools_AutonomousTurnsCannotCreate(t *testing.T) {
	type run func(t *testing.T, al *AgentLoop, prov *charScriptedProvider)
	cases := map[string]run{
		"heartbeat": func(t *testing.T, al *AgentLoop, _ *charScriptedProvider) {
			if _, err := al.ProcessHeartbeat(context.Background(), "check", "telegram", "chat1"); err != nil {
				t.Fatal(err)
			}
		},
		"cron": func(t *testing.T, al *AgentLoop, _ *charScriptedProvider) {
			ctx := tools.WithToolOrigin(context.Background(), tools.OriginCron)
			if _, err := al.ProcessDirectWithChannel(ctx, "scheduled job", "cron-sess", "telegram", "chat1", false); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, do := range cases {
		t.Run(name, func(t *testing.T) {
			prov := &charScriptedProvider{responses: []providers.LLMResponse{
				toolCall("c1", "create_intent", watchCallArgs("sneaky "+intentMarker, true)),
				toolCall("c2", "update_intent", map[string]any{"id": "x", "action": "confirm"}),
				{Content: "done"},
			}}
			al, workspace := newIntentLoop(t, prov, true)
			do(t, al, prov)

			if hasName(defNames(prov.gotTools[0]), "create_intent") {
				t.Errorf("%s: create_intent is in the turn's definitions: %v", name, defNames(prov.gotTools[0]))
			}
			if !hasName(defNames(prov.gotTools[0]), "list_intents") {
				t.Errorf("%s: list_intents (read class) should be offered: %v", name, defNames(prov.gotTools[0]))
			}
			if !anyMessageContains(prov.gotMsgs[1], refusalText) {
				t.Errorf("%s: the create_intent call was not refused by the ceiling", name)
			}
			if !anyMessageContains(prov.gotMsgs[2], "not available in background or scheduled turns") {
				t.Errorf("%s: update_intent was not refused by the owner rule", name)
			}
			if rows := allIntents(t, workspace); len(rows) != 0 {
				t.Fatalf("%s: %d intents stored", name, len(rows))
			}
		})
	}

	t.Run("proactive", func(t *testing.T) {
		prov := script(
			toolCall("c1", "create_intent", watchCallArgs("sneaky "+intentMarker, true)),
			respondResponse(map[string]any{"decision": "silent"}),
		)
		workspace := t.TempDir()
		t.Cleanup(func() { _ = sqlitex.CloseShared() })
		al := NewAgentLoop(intentLoopConfig(workspace, true), bus.NewMessageBus(), prov)
		t.Cleanup(al.Close)

		req := proactiveReq()
		req.Tools = []string{AllProactiveTools}
		mustRun(t, al, req)

		prov.mu.Lock()
		defer prov.mu.Unlock()
		names := defNames(prov.defs[0])
		if hasName(names, "create_intent") || hasName(names, "update_intent") {
			t.Errorf("a proactive turn is offered an intent writer: %v", names)
		}
		if !hasName(names, "list_intents") {
			t.Errorf("a proactive turn should be offered list_intents: %v", names)
		}
		if len(prov.msgs) < 2 {
			t.Fatalf("provider saw %d calls", len(prov.msgs))
		}
		denied := false
		for _, m := range prov.msgs[1] {
			if strings.Contains(m.Content, "not permitted in a proactive") || strings.Contains(m.Content, "not permitted") {
				denied = true
			}
		}
		if !denied {
			t.Error("create_intent in a proactive turn was not refused by the ceiling")
		}
		if rows := allIntents(t, workspace); len(rows) != 0 {
			t.Fatalf("%d intents stored by a proactive turn", len(rows))
		}
	})
}

// A sub-turn the agent loop starts (spawn, subagent, delegate) carries the owner's
// origin and sender, and its task was written by a model. What it creates is a
// suggestion, whatever it claims, and it cannot confirm one.
func TestIntentTools_SubTurnOfTheOwnersTurnOnlySuggests(t *testing.T) {
	prov := &charScriptedProvider{responses: []providers.LLMResponse{
		toolCall("c1", "create_intent", watchCallArgs("from a sub-turn", true)),
		{Content: "done"},
	}}
	al, workspace := newIntentLoop(t, prov, true)

	parent := &turnState{
		ctx:            context.Background(),
		turnID:         "parent-owner",
		pendingResults: make(chan *tools.ToolResult, 1),
		session:        &ephemeralSessionStore{},
	}
	ctx := tools.WithToolOrigin(context.Background(), tools.OriginRemote)
	ctx = tools.WithToolSender(ctx, profileOwner)
	ctx = tools.WithToolContext(ctx, "telegram", "chat1")
	if _, err := spawnSubTurn(ctx, al, parent, SubTurnConfig{Model: "char-model"}); err != nil {
		t.Fatalf("spawnSubTurn: %v", err)
	}
	rows := allIntents(t, workspace)
	if len(rows) != 1 {
		t.Fatalf("%d intents stored by the sub-turn, want 1", len(rows))
	}
	if rows[0].Status != intent.StatusProposed || rows[0].Source != intent.SourceInferred ||
		rows[0].Provenance.TurnKind != proactive.TurnSubTurn {
		t.Fatalf("a sub-turn of the owner's turn stored %s/%s (%s)", rows[0].Status, rows[0].Source, rows[0].Provenance.TurnKind)
	}
	if !anyMessageContains(prov.gotMsgs[1], "Suggested (not active) watch: from a sub-turn") {
		t.Fatal("the sub-turn's model was not told it stored a suggestion")
	}
}

// A suggestion's text is in no prompt mode: not the full chat prompt, not the
// light one a sub-turn gets, not the proactive one. Active titles are, and the
// suggestion count is.
func TestIntentDigest_SuggestionTextIsInNoPromptMode(t *testing.T) {
	al, workspace := newIntentLoop(t, &charScriptedProvider{}, true)
	store := profileStoreFor(t, workspace)
	ctx := context.Background()

	owner := intent.Provenance{SenderID: profileOwner, Trust: proactive.TrustUserAuthored, TurnKind: proactive.TurnUser}
	model := intent.Provenance{Trust: proactive.TrustUntrusted, TurnKind: proactive.TurnProactive}
	if _, err := store.CreateIntent(ctx, intent.Intent{Type: intent.TypeGoal, Title: "keep crypto under 30%",
		Status: intent.StatusActive, Source: intent.SourceExplicit, Provenance: owner}); err != nil {
		t.Fatal(err)
	}
	for i, ty := range []intent.IntentType{intent.TypeGoal, intent.TypeWatch, intent.TypeStandingOrder, intent.TypeReminder, intent.TypePreference} {
		if _, err := store.CreateIntent(ctx, intent.Intent{Type: ty, Title: fmt.Sprintf("%s %d %s", ty, i, intentMarker),
			Params: []byte(`{"note":"` + intentMarker + `"}`), Provenance: model}); err != nil {
			t.Fatal(err)
		}
	}

	cb := al.GetRegistry().GetDefaultAgent().ContextBuilder
	for _, mode := range []PromptMode{PromptFull, PromptLight, PromptProactive} {
		msgs := cb.BuildMessagesWithOptions(nil, "", "hello", nil, "telegram", "c1", BuildOptions{Mode: mode})
		if anyMessageContains(msgs, intentMarker) {
			t.Errorf("mode %d: a suggestion's text is in the prompt", mode)
		}
		if !anyMessageContains(msgs, "- goal: keep crypto under 30%") {
			t.Errorf("mode %d: the active goal is missing from the prompt", mode)
		}
		if !anyMessageContains(msgs, "Suggested intents, not active: 5") {
			t.Errorf("mode %d: the suggestion count is missing", mode)
		}
	}
}

// With the default config nothing exists: no tool, no contributor, no header, and
// the financial context is byte-identical to one built without the feature.
func TestIntents_AbsentByDefault(t *testing.T) {
	al, workspace := newIntentLoop(t, &charScriptedProvider{}, false)
	tl := al.GetRegistry().GetDefaultAgent().Tools
	for _, name := range []string{"create_intent", "list_intents", "update_intent"} {
		if _, ok := tl.Get(name); ok {
			t.Errorf("%s is registered with the default config", name)
		}
	}
	collector := al.GetRegistry().GetDefaultAgent().financialCollector
	if collector == nil {
		t.Fatal("no financial collector")
	}
	for _, c := range collector.contributors {
		if c.Name() == "intents" {
			t.Fatal("the intents contributor is attached with the default config")
		}
	}

	// Even with intents in the ledger, a prompt built with the feature off has no
	// trace of them: the tables exist (migration v2 ran when the store opened) but
	// nothing reads them.
	store := profileStoreFor(t, workspace)
	if _, err := store.CreateIntent(context.Background(), intent.Intent{Type: intent.TypeGoal, Title: intentMarker + " active",
		Status: intent.StatusActive, Source: intent.SourceExplicit,
		Provenance: intent.Provenance{SenderID: profileOwner, Trust: proactive.TrustUserAuthored, TurnKind: proactive.TurnUser}}); err != nil {
		t.Fatal(err)
	}
	cb := al.GetRegistry().GetDefaultAgent().ContextBuilder
	for _, mode := range []PromptMode{PromptFull, PromptLight, PromptProactive} {
		msgs := cb.BuildMessagesWithOptions(nil, "", "hello", nil, "telegram", "c1", BuildOptions{Mode: mode})
		if anyMessageContains(msgs, intentMarker) || anyMessageContains(msgs, "Intents (") {
			t.Errorf("mode %d: the prompt shows intents with the feature off", mode)
		}
	}
}

// Enabled with an empty ledger the digest is zero bytes: the financial context is
// the same string as with the feature off, with and without other sections.
func TestIntentDigest_EmptyIsZeroBytes(t *testing.T) {
	ctx := context.Background()
	store, err := intent.NewStore(ctx, sqlitextest.Open(t))
	if err != nil {
		t.Fatal(err)
	}
	c := &intentsContributor{store: store}
	for _, compact := range []bool{false, true} {
		if got, err := c.LiveSection(ctx, compact); err != nil || got != "" {
			t.Fatalf("empty ledger, compact=%v: %q, %v", compact, got, err)
		}
	}
	if got, _ := c.Section(ctx); got != "" {
		t.Fatalf("Section = %q", got)
	}

	other := &dcaStub{section: "Active DCA (1): plan"}
	with := NewFinancialContextCollector([]FinancialContributor{other, c}, time.Minute)
	without := NewFinancialContextCollector([]FinancialContributor{other}, time.Minute)
	if a, b := with.GetSummary(), without.GetSummary(); a != b || a == "" {
		t.Fatalf("an empty digest changed the context:\n%q\n%q", a, b)
	}
	only := NewFinancialContextCollector([]FinancialContributor{c}, time.Minute)
	if got := only.GetSummary(); got != "" {
		t.Fatalf("an empty digest alone produced %q (a header with nothing under it)", got)
	}
}

type dcaStub struct{ section string }

func (d *dcaStub) Name() string                            { return "stub" }
func (d *dcaStub) Section(context.Context) (string, error) { return d.section, nil }
func (d *dcaStub) Close() error                            { return nil }

func TestIntentDigestContributor(t *testing.T) {
	ctx := context.Background()
	store, err := intent.NewStore(ctx, sqlitextest.Open(t))
	if err != nil {
		t.Fatal(err)
	}
	owner := intent.Provenance{SenderID: profileOwner, Trust: proactive.TrustUserAuthored, TurnKind: proactive.TurnUser}
	model := intent.Provenance{Trust: proactive.TrustUntrusted, TurnKind: proactive.TurnProactive}
	add := func(ty intent.IntentType, title string, active bool) intent.Intent {
		in := intent.Intent{Type: ty, Title: title, Provenance: model}
		if active {
			in.Status, in.Source, in.Provenance = intent.StatusActive, intent.SourceExplicit, owner
		}
		got, err := store.CreateIntent(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	c := &intentsContributor{store: store}

	add(intent.TypeWatch, "SOL/USDT below 120", true)
	add(intent.TypeGoal, "keep crypto under 30%", true)
	add(intent.TypeStandingOrder, "never buy leveraged tokens", true)
	add(intent.TypeReminder, "review SET positions", true) // not in the digest
	add(intent.TypePreference, "short answers", true)      // not in the digest
	add(intent.TypeWatch, "suggested watch "+intentMarker, false)
	add(intent.TypeGoal, "suggested goal "+intentMarker, false)

	got, err := c.Section(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := "Intents (active, set by the owner):\n" +
		"- goal: keep crypto under 30%\n" +
		"- standing order: never buy leveraged tokens\n" +
		"- watch: SOL/USDT below 120\n" +
		"Suggested intents, not active: 2 (list_intents shows them)"
	if got != want {
		t.Fatalf("digest:\n%s\nwant:\n%s", got, want)
	}
	if strings.Contains(got, intentMarker) || strings.Contains(got, "review SET") || strings.Contains(got, "short answers") {
		t.Fatal("the digest holds text it must not")
	}

	// Only suggestions: just the count, no header about active intents.
	only, _ := intent.NewStore(ctx, sqlitextest.Open(t))
	if _, err := only.CreateIntent(ctx, intent.Intent{Type: intent.TypeGoal, Title: "x", Provenance: model}); err != nil {
		t.Fatal(err)
	}
	got, _ = (&intentsContributor{store: only}).Section(ctx)
	if got != "Suggested intents, not active: 1 (list_intents shows them)" {
		t.Fatalf("only suggestions: %q", got)
	}

	// A cancelled intent leaves the digest at once, without any cache to wait out.
	collector := NewFinancialContextCollector([]FinancialContributor{c}, time.Hour)
	if s := collector.GetSummary(); !strings.Contains(s, "keep crypto under 30%") {
		t.Fatalf("summary = %q", s)
	}
	rows, _ := store.ListIntents(ctx, intent.IntentFilter{Types: []intent.IntentType{intent.TypeGoal}, Statuses: []intent.Status{intent.StatusActive}})
	if err := store.Transition(ctx, rows[0].ID, intent.StatusCancelled, owner, "test"); err != nil {
		t.Fatal(err)
	}
	if s := collector.GetSummary(); strings.Contains(s, "keep crypto under 30%") {
		t.Fatalf("a cancelled goal is still in the digest inside the cache window: %q", s)
	}
}

// The digest keeps to its cap in bytes, in a chat turn and in a compact one, with
// plenty of long titles, and says how many it left out.
func TestIntentDigest_CapsAndTruncation(t *testing.T) {
	var active []intent.Intent
	for i := 0; i < 40; i++ {
		active = append(active, intent.Intent{
			Type: intent.TypeGoal, Title: fmt.Sprintf("%02d %s", i, strings.Repeat("ก", 115)),
		})
	}
	for _, limit := range []int{intentDigestCap, intentDigestCompactCap} {
		got := renderIntentDigest(active, 7, limit)
		if len(got) > limit || len(got) < limit-500 {
			t.Errorf("limit %d: digest is %d bytes", limit, len(got))
		}
		if !strings.Contains(got, "... and ") || !strings.HasSuffix(got, "Suggested intents, not active: 7 (list_intents shows them)") {
			t.Errorf("limit %d: no 'more' line or no suggestion count:\n%s", limit, got)
		}
		if !strings.HasPrefix(got, "Intents (active, set by the owner):\n- goal: 00 ") {
			t.Errorf("limit %d: wrong start:\n%s", limit, got)
		}
	}
	// A title with control characters is flattened to one line.
	override := string(rune(0x202e))
	got := renderIntentDigest([]intent.Intent{{Type: intent.TypeGoal, Title: "a\nb" + override + "c"}}, 0, 1200)
	if strings.Count(got, "\n") != 1 || strings.Contains(got, override) {
		t.Errorf("title not flattened: %q", got)
	}
	if renderIntentDigest(nil, 0, 1200) != "" {
		t.Error("nothing to show must be empty")
	}
}

// A config reload switches the tools and the digest on and off without a restart.
func TestIntents_FollowConfigReload(t *testing.T) {
	provider := &charScriptedProvider{}
	al, workspace := newIntentLoop(t, provider, false)
	has := func() (tool bool, digest bool) {
		a := al.GetRegistry().GetDefaultAgent()
		_, tool = a.Tools.Get("create_intent")
		for _, c := range a.financialCollector.contributors {
			digest = digest || c.Name() == "intents"
		}
		return
	}
	if tool, digest := has(); tool || digest {
		t.Fatalf("off: tool=%v digest=%v", tool, digest)
	}

	if err := al.ReloadProviderAndConfig(context.Background(), provider, intentLoopConfig(workspace, true)); err != nil {
		t.Fatal(err)
	}
	if tool, digest := has(); !tool || !digest {
		t.Fatalf("after switching on: tool=%v digest=%v", tool, digest)
	}
	for _, name := range []string{"list_intents", "update_intent"} {
		if _, ok := al.GetRegistry().GetDefaultAgent().Tools.Get(name); !ok {
			t.Errorf("%s missing after switching on", name)
		}
	}

	if err := al.ReloadProviderAndConfig(context.Background(), provider, intentLoopConfig(workspace, false)); err != nil {
		t.Fatal(err)
	}
	if tool, digest := has(); tool || digest {
		t.Fatalf("after switching off: tool=%v digest=%v", tool, digest)
	}

	// proactive.intents on but proactive.enabled off is off too.
	cfg := intentLoopConfig(workspace, true)
	cfg.Proactive.Enabled = false
	if err := al.ReloadProviderAndConfig(context.Background(), provider, cfg); err != nil {
		t.Fatal(err)
	}
	if tool, digest := has(); tool || digest {
		t.Fatalf("intents without proactive.enabled: tool=%v digest=%v", tool, digest)
	}
}

// Each tool can be switched off on its own.
func TestIntentTools_RespectEnableSwitches(t *testing.T) {
	workspace := t.TempDir()
	t.Cleanup(func() { _ = sqlitex.CloseShared() })
	cfg := intentLoopConfig(workspace, true)
	cfg.Tools.UpdateIntent.Enabled = false
	al := NewAgentLoop(cfg, bus.NewMessageBus(), &charScriptedProvider{})
	t.Cleanup(al.Close)
	tl := al.GetRegistry().GetDefaultAgent().Tools
	if _, ok := tl.Get("update_intent"); ok {
		t.Error("update_intent registered although disabled")
	}
	for _, name := range []string{"create_intent", "list_intents"} {
		if _, ok := tl.Get(name); !ok {
			t.Errorf("%s missing", name)
		}
	}
}
