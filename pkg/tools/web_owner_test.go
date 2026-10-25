package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/identity"
	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/policy"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// armWebGate arms condition (c) of the web owner route for one test.
func armWebGate(t *testing.T, ok bool) {
	t.Helper()
	config.SetWebLauncherGate(func(string) (bool, string) { return ok, "gate says no" })
	t.Cleanup(func() { config.SetWebLauncherGate(nil) })
}

// webTurn is a root turn from the dashboard chat: the pico channel, the sender
// the pico channel gives every connection, and (when auth) the tag the agent loop
// sets for a token-authenticated connection.
func webTurn(auth bool) context.Context {
	ctx := WithToolSender(WithToolOrigin(context.Background(), OriginRemote), "pico:pico-user")
	if auth {
		ctx = WithWebAuth(ctx)
	}
	return WithToolContext(ctx, config.WebChannel, "pico:abc")
}

func TestCheckOwnerTurn_WebRoute(t *testing.T) {
	off := false
	cases := []struct {
		name string
		gate bool
		cfg  func() *config.Config
		ctx  func() context.Context
		want ownerVerdict
	}{
		{"all conditions hold", true, func() *config.Config { return approvalsCfg(true) }, func() context.Context { return webTurn(true) }, ownerYes},
		{"(a) no owner configured", true, func() *config.Config { return config.DefaultConfig() }, func() context.Context { return webTurn(true) }, ownerNoOwner},
		{"(b) connection not token-authenticated", true, func() *config.Config { return approvalsCfg(true) }, func() context.Context { return webTurn(false) }, ownerOther},
		{"(c) launcher authentication not established", false, func() *config.Config { return approvalsCfg(true) }, func() context.Context { return webTurn(true) }, ownerOther},
		{"web_is_owner false", true, func() *config.Config {
			c := approvalsCfg(true)
			c.Proactive.Owner.WebIsOwner = &off
			return c
		}, func() context.Context { return webTurn(true) }, ownerOther},
		{"a sub-turn of a web turn", true, func() *config.Config { return approvalsCfg(true) }, func() context.Context {
			return WithToolContext(WithSubTurn(webTurn(true)), "", "")
		}, ownerSubTurn},
		{"a sub-turn that kept the channel", true, func() *config.Config { return approvalsCfg(true) }, func() context.Context {
			return WithSubTurn(webTurn(true))
		}, ownerSubTurn},
		{"the tag on another channel", true, func() *config.Config { return approvalsCfg(true) }, func() context.Context {
			return WithToolContext(webTurn(true), "telegram", "999")
		}, ownerOther},
		{"a heartbeat carrying the tag", true, func() *config.Config { return approvalsCfg(true) }, func() context.Context {
			return WithToolContext(WithWebAuth(WithToolOrigin(context.Background(), OriginHeartbeat)), config.WebChannel, "x")
		}, ownerBackground},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			armWebGate(t, tc.gate)
			by, v := checkOwnerTurn(tc.ctx(), tc.cfg(), true)
			if v != tc.want {
				t.Fatalf("verdict = %v, want %v", v, tc.want)
			}
			if v == ownerYes && by != identity.WebOwner {
				t.Errorf("author = %q, want %q", by, identity.WebOwner)
			}
			if v != ownerYes && by != "" {
				t.Errorf("author = %q on a refusal", by)
			}
		})
	}
}

func TestCheckOwnerTurn_WebRouteNeedsAnArmedGate(t *testing.T) {
	config.SetWebLauncherGate(nil)
	if _, v := checkOwnerTurn(webTurn(true), approvalsCfg(true), true); v != ownerOther {
		t.Fatalf("an unarmed gate must fail closed, got %v", v)
	}
}

func TestCheckOwnerTurn_ConfiguredRouteUnchanged(t *testing.T) {
	for _, gate := range []bool{true, false} {
		armWebGate(t, gate)
		ctx := WithToolContext(WithToolSender(WithToolOrigin(context.Background(), OriginRemote), proposeOwner), "telegram", "1001")
		by, v := checkOwnerTurn(ctx, approvalsCfg(true), true)
		if v != ownerYes || by != proposeOwner {
			t.Errorf("gate=%v: configured owner got (%q, %v)", gate, by, v)
		}
	}
}

// Every consumer of the shared rule gets the same answer from the web chat.
func TestWebRoute_ReachesEveryConsumer(t *testing.T) {
	armWebGate(t, true)
	cfg := approvalsCfg(true)
	ctx := webTurn(true)

	if !isOwnerRootTurn(ctx, cfg) {
		t.Error("memory gate: web turn is not the owner")
	}
	if isOwnerRootTurn(WithSubTurn(ctx), cfg) {
		t.Error("memory gate: a sub-turn of a web turn must not be the owner")
	}
	SetProposalConfig(cfg)
	t.Cleanup(func() { SetProposalConfig(nil) })
	if !proposeOrderOpen(ctx) {
		t.Error("propose_order is not open to the web chat")
	}
	if proposeOrderOpen(webTurn(false)) {
		t.Error("propose_order is open to an unauthenticated connection")
	}
	kind, actor := turnIdentity(ctx)
	if kind != proactive.TurnUser || actor != policy.ActorOwner {
		t.Errorf("policy identity = (%v, %v), want (user, owner): the dial's owner column, live policy and the kill exemption read this", kind, actor)
	}
	if _, actor := turnIdentity(webTurn(false)); actor != policy.ActorOther {
		t.Errorf("an unauthenticated connection has actor %v", actor)
	}
	if ownerOnlyApplies(ctx) {
		t.Error("the memory file would refuse the web owner")
	}
	if !ownerOnlyApplies(webTurn(false)) {
		t.Error("the memory file would admit an unauthenticated connection")
	}
}

func TestRemember_WebOwnerWritesStampedAsWeb(t *testing.T) {
	armWebGate(t, true)
	r := newRememberRig(t, true, true)
	tn := turn{name: "web", ctx: webTurn(true), channel: config.WebChannel}
	if res := r.remember(tn, "I prefer short answers"); res.IsError {
		t.Fatalf("remember: %s", res.ForLLM)
	}
	want := "## Notes\n- [preference] I prefer short answers <!-- by:owner-web at:2026-10-19 -->\n"
	if got := r.memoryFile(); got != want {
		t.Fatalf("MEMORY.md = %q, want %q", got, want)
	}
	if n := len(r.prop.all()); n != 0 {
		t.Errorf("the web owner's turn created %d proposals", n)
	}
	// Not authenticated: a suggestion, nothing written.
	r2 := newRememberRig(t, true, true)
	if res := r2.remember(turn{name: "plain", ctx: webTurn(false), channel: config.WebChannel}, "x y"); res.IsError {
		t.Fatalf("remember: %s", res.ForLLM)
	}
	if r2.memoryFile() != "" || len(r2.prop.all()) != 1 {
		t.Errorf("an unauthenticated connection wrote memory (%q) or made %d suggestions", r2.memoryFile(), len(r2.prop.all()))
	}
}

func TestUpdateProfile_WebOwnerIsRecordedAsWeb(t *testing.T) {
	armWebGate(t, true)
	store := profileTestStore(t)
	tool := NewUpdateProfileTool(store, approvalsCfg(true))
	if res := tool.Execute(webTurn(true), map[string]any{"language": "th"}); res.IsError {
		t.Fatal(res.ForLLM)
	}
	v, ok, _ := store.Latest(context.Background())
	if !ok || v.UpdatedBy != identity.WebOwner {
		t.Fatalf("updated_by = %q, want web", v.UpdatedBy)
	}
	res := tool.Execute(webTurn(false), map[string]any{"language": "en"})
	if !res.IsError || !strings.Contains(res.ForLLM, "not from the configured owner") {
		t.Fatalf("an unauthenticated connection changed the profile: %q", res.ForLLM)
	}
}

func TestCreateIntent_WebOwnerActivatesAndIsRecordedAsWeb(t *testing.T) {
	armWebGate(t, true)
	store := intentTestStore(t)
	tool := NewCreateIntentTool(store, intentTestConfig(true))
	res := tool.Execute(webTurn(true), watchArgs("SOL/USDT below 120", true))
	if res.IsError || !strings.HasPrefix(res.ForLLM, "Active watch:") {
		t.Fatalf("web owner + user_requested: %q", res.ForLLM)
	}
	rows, _ := store.ListIntents(context.Background(), intent.IntentFilter{})
	if len(rows) != 1 || rows[0].Provenance.SenderID != identity.WebOwner ||
		rows[0].Provenance.TurnKind != proactive.TurnUser || rows[0].Provenance.Trust != proactive.TrustUserAuthored {
		t.Fatalf("provenance = %+v", rows)
	}
	res = tool.Execute(webTurn(false), watchArgs("SOL/USDT below 110", true))
	if !strings.HasPrefix(res.ForLLM, "Suggested (not active) watch:") {
		t.Fatalf("an unauthenticated connection activated an intent: %q", res.ForLLM)
	}
}

// Live policy and the kill gate see the web chat as the owner's own root turn: it
// acts (also under the kill switch and after reading the web), and the same turn
// from an unauthenticated connection is a stranger's - stopped by gate K.
func TestLive_WebOwnerActsAndIsKillExempt(t *testing.T) {
	armWebGate(t, true)
	l := newLiveRig(t, nil)
	onPico := func(ctx context.Context, name string, args map[string]any) *ToolResult {
		return l.reg.ExecuteWithContext(ctx, name, args, config.WebChannel, "c", nil)
	}

	if res := onPico(webTurn(true), NameCreateOrder, buyOrder("BTC/USDT", 1)); res.IsError {
		t.Fatalf("the web owner's order was refused: %s", res.ForLLM)
	}
	l.killed.Store(true)
	if res := onPico(webTurn(true), NameCreateOrder, buyOrder("ETH/USDT", 2)); res.IsError {
		t.Fatalf("the web owner's order under the kill switch (K exempts the owner's own turn): %s", res.ForLLM)
	}
	if l.trade.count() != 2 {
		t.Fatalf("%d orders ran, want 2", l.trade.count())
	}

	res := onPico(webTurn(false), NameCreateOrder, buyOrder("SOL/USDT", 3))
	if !res.IsError || !strings.Contains(res.ForLLM, "Policy refused this call: K:") || l.trade.count() != 2 {
		t.Fatalf("an unauthenticated pico connection under the kill switch: error=%v runs=%d %q", res.IsError, l.trade.count(), res.ForLLM)
	}
	for _, d := range l.rows(t) {
		if d.Tool == NameCreateOrder && d.Mode == policy.ModeAct && !d.Enforced {
			t.Errorf("a row was not enforced: %+v", d)
		}
	}
}

// With the launcher's authentication not established, the same turn is a stranger's
// even with the connection authenticated.
func TestLive_WebChatWithoutLauncherAuthIsAStranger(t *testing.T) {
	armWebGate(t, false)
	l := newLiveRig(t, nil)
	l.killed.Store(true)
	res := l.reg.ExecuteWithContext(webTurn(true), NameCreateOrder, buyOrder("BTC/USDT", 1), config.WebChannel, "c", nil)
	if !res.IsError || l.trade.count() != 0 {
		t.Fatalf("error=%v runs=%d %q", res.IsError, l.trade.count(), res.ForLLM)
	}
}
