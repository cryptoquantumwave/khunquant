package tools

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

const (
	gateOwner = "telegram:1001"
	gateOther = "telegram:2002"
)

func gateCfg(withOwner bool) *config.Config {
	cfg := config.DefaultConfig()
	cfg.Proactive.Enabled = true
	if withOwner {
		cfg.Proactive.Owner = config.OwnerConfig{Channel: "telegram", ChatID: "1001", SenderID: gateOwner}
	}
	return cfg
}

func gateRegistry(t *testing.T, cfg *config.Config) *ToolRegistry {
	t.Helper()
	armPolicy(t, false, defaultToolPolicies())
	SetProposalConfig(cfg)
	t.Cleanup(func() { SetProposalConfig(nil) })
	reg := NewToolRegistry()
	for _, n := range []string{NameCreateOrder, NameTransferFunds, NameFuturesEmergencyFlatten, NameExecuteDCAOrder,
		NameGetTotalValue, NameFuturesGetPositions, NameGetProfile, NameGetTicker, NameGetOHLCV} {
		reg.Register(&stubTool{name: n})
	}
	return reg
}

func personTurn(sender, channel, chat string) context.Context {
	ctx := WithToolOrigin(context.Background(), OriginRemote)
	ctx = WithToolSender(ctx, sender)
	return WithToolContext(ctx, channel, chat)
}

func TestNonOwnerTurnGate(t *testing.T) {
	trade, funds, flatten := NameCreateOrder, NameTransferFunds, NameFuturesEmergencyFlatten
	ownerRead, ownerRead2, publicRead := NameGetTotalValue, NameFuturesGetPositions, NameGetTicker

	web := WithWebAuth(WithToolContext(WithToolOrigin(context.Background(), OriginRemote), config.WebChannel, "web"))
	dcaJob := WithAutomationScope(WithToolOrigin(context.Background(), OriginCron), AutomationScope{JobName: "dca:7:weekly", DCAPlanID: 7})
	hb := WithToolOrigin(context.Background(), OriginHeartbeat)
	pro := WithProactiveTurn(WithToolOrigin(context.Background(), OriginProactive), proactive.TurnProactive)

	// allowed: tool name -> whether the turn may call it (and is shown it).
	type want = map[string]bool
	all := want{trade: true, funds: true, flatten: true, ownerRead: true, ownerRead2: true, publicRead: true}
	refusedPerson := want{trade: false, funds: false, flatten: false, ownerRead: false, ownerRead2: false, publicRead: true}

	cases := []struct {
		name    string
		owner   bool
		ctx     context.Context
		want    want
		webOnly bool
	}{
		{"owner root", true, personTurn(gateOwner, "telegram", "1001"), all, false},
		{"owner sub-turn", true, WithSubTurn(personTurn(gateOwner, "telegram", "1001")), all, false},
		{"owner on the web launcher", true, web, all, true},
		{"non-owner DM", true, personTurn(gateOther, "telegram", "2002"), refusedPerson, false},
		{"non-owner group", true, personTurn(gateOther, "telegram", "-100group"), refusedPerson, false},
		{"non-owner sub-turn", true, WithSubTurn(personTurn(gateOther, "telegram", "2002")), refusedPerson, false},
		{"non-owner sub-turn with no channel", true, WithSubTurn(WithToolSender(WithToolOrigin(context.Background(), OriginRemote), gateOther)), refusedPerson, false},
		{"remote turn with no sender", true, WithToolContext(WithToolOrigin(context.Background(), OriginRemote), "telegram", "9"), refusedPerson, false},
		{"unauthenticated web chat", true, WithToolContext(WithToolOrigin(context.Background(), OriginRemote), config.WebChannel, "web"), refusedPerson, true},
		{"no owner configured, any person", false, personTurn(gateOther, "telegram", "2002"), all, false},
		// Background turns are not persons: they keep what they have today.
		// An untagged turn fails closed on the money rows today (rowGate); the gate adds nothing.
		{"untagged", true, context.Background(), want{trade: false, funds: false, flatten: true, ownerRead: true, ownerRead2: true, publicRead: true}, false},
		{"heartbeat", true, hb, want{ownerRead: true, ownerRead2: true, publicRead: true, trade: false, funds: false, flatten: false}, false},
		{"proactive", true, pro, want{ownerRead: true, ownerRead2: true, publicRead: true, trade: false, funds: false, flatten: false}, false},
		{"dca job", true, dcaJob, want{NameExecuteDCAOrder: true, ownerRead: true, publicRead: true, trade: false, funds: false}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := gateCfg(tc.owner)
			if tc.webOnly {
				config.SetWebLauncherGate(func(string) (bool, string) { return true, "" })
				t.Cleanup(func() { config.SetWebLauncherGate(nil) })
			}
			reg := gateRegistry(t, cfg)
			names := make([]string, 0, len(tc.want))
			for n := range tc.want {
				names = append(names, n)
			}
			sort.Strings(names)
			ctx := tc.ctx
			shown := map[string]bool{}
			for _, d := range reg.ToProviderDefsFor(WithToolContext(ctx, ToolChannel(ctx), "x")) {
				shown[d.Function.Name] = true
			}
			for _, n := range names {
				res := reg.ExecuteWithContext(ctx, n, map[string]any{}, ToolChannel(ctx), "chat", nil)
				ran := !res.IsError
				if ran != tc.want[n] {
					t.Errorf("%s: ran=%v want %v (%s)", n, ran, tc.want[n], res.ForLLM)
				}
				if !ran && strings.Contains(res.ForLLM, "ask the owner") && shown[n] {
					t.Errorf("%s is refused to a person but still offered in the definitions", n)
				}
				if tc.want[n] && !shown[n] {
					t.Errorf("%s is allowed but hidden from the definitions", n)
				}
			}
		})
	}
}

func TestNonOwnerRefusalNamesNoFigures(t *testing.T) {
	res := nonOwnerRefusal(NameCreateOrder)
	if !res.IsError || !strings.Contains(res.ForLLM, "ask the owner") {
		t.Fatalf("refusal = %+v", res)
	}
	for _, r := range res.ForLLM {
		if r >= '0' && r <= '9' {
			t.Fatalf("refusal carries a figure: %s", res.ForLLM)
		}
	}
}

// Every read-class tool is placed on purpose, in exactly one of the two sets; an
// unplaced one counts as owner-data (refused to a non-owner).
func TestEveryReadToolIsPlacedForOwner(t *testing.T) {
	var names []string
	for name, class := range toolClasses {
		if class == ClassRead {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		switch pub, own := publicReadTools[name], ownerDataReadTools[name]; {
		case pub && own:
			t.Errorf("%s is in both sets", name)
		case !pub && !own:
			t.Errorf("read tool %q is in neither set: decide whether it reads the owner's data (ownerDataReadTools) or public data (publicReadTools)", name)
		}
	}
	for _, set := range []map[string]bool{publicReadTools, ownerDataReadTools} {
		for name := range set {
			if ClassOf(name) != ClassRead {
				t.Errorf("%q is placed but is not a read-class tool", name)
			}
		}
	}
	if !ownerDataToolRefused("a_new_read_tool_nobody_placed") && ClassOf("a_new_read_tool_nobody_placed") == ClassRead {
		t.Error("an unplaced read tool must count as owner-data")
	}
	toolClasses["zz_unplaced_read"] = ClassRead
	defer delete(toolClasses, "zz_unplaced_read")
	if !ownerDataToolRefused("zz_unplaced_read") {
		t.Error("an unplaced read-class tool is not refused (fail closed)")
	}
}
