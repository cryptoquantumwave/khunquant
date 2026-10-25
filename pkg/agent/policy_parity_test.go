package agent

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/policy"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

const parityOwner = "telegram:1001"

type parityCase struct {
	name string
	// ownerless runs the case with no owner configured.
	ownerless bool
	// build returns the context of one call at the given trust, carrying a taint
	// when holder is set (a turn a person typed has none, an autonomous one has it
	// from its start; a candidate-driven proactive turn always has it, in its
	// proposal scope). It is called once per call: the registry's note of a call
	// lowers the turn's taint, and a call must not inherit another's.
	build func(trust proactive.Trust, holder bool) context.Context
}

func parityCases() []parityCase {
	remote := func(sender string) context.Context {
		ctx := tools.WithToolOrigin(context.Background(), tools.OriginRemote)
		ctx = tools.WithToolSender(ctx, sender)
		return tools.WithToolContext(ctx, "telegram", "chat")
	}
	with := func(ctx context.Context, trust proactive.Trust, holder bool) context.Context {
		if !holder {
			return ctx
		}
		return proactive.WithTaint(ctx, proactive.NewTaint(trust))
	}
	scoped := func(kind proactive.TurnKind, trust proactive.Trust, deliver bool) context.Context {
		ctx := tools.WithProactiveTurn(context.Background(), kind)
		cands := []proactive.Candidate{{ID: "c1", Trust: trust}}
		return tools.WithProposalScope(ctx, tools.NewProposalScope(cands, deliver, "01IV", false))
	}
	kind := func(k proactive.TurnKind) func(proactive.Trust, bool) context.Context {
		return func(tr proactive.Trust, h bool) context.Context {
			return with(tools.WithProactiveTurn(context.Background(), k), tr, h)
		}
	}
	origin := func(o string) func(proactive.Trust, bool) context.Context {
		return func(tr proactive.Trust, h bool) context.Context {
			return with(tools.WithToolOrigin(context.Background(), o), tr, h)
		}
	}
	return []parityCase{
		{name: "owner user root", build: func(tr proactive.Trust, h bool) context.Context { return with(remote(parityOwner), tr, h) }},
		{name: "owner at the console", build: func(tr proactive.Trust, h bool) context.Context {
			ctx := tools.WithToolContext(tools.WithToolOrigin(context.Background(), tools.OriginInternal), "cli", "direct")
			return with(ctx, tr, h)
		}},
		{name: "owner sub-turn", build: func(tr proactive.Trust, h bool) context.Context {
			return with(tools.WithSubTurn(remote(parityOwner)), tr, h)
		}},
		{name: "non-owner user", build: func(tr proactive.Trust, h bool) context.Context { return with(remote("telegram:999"), tr, h) }},
		{name: "no owner configured", ownerless: true, build: func(tr proactive.Trust, h bool) context.Context {
			return with(remote(parityOwner), tr, h)
		}},
		{name: "cron", build: origin(tools.OriginCron)},
		{name: "cron dca job", build: func(tr proactive.Trust, h bool) context.Context {
			ctx := tools.WithToolOrigin(context.Background(), tools.OriginCron)
			return with(tools.WithAutomationScope(ctx, tools.AutomationScope{JobName: "dca:7:weekly", DCAPlanID: 7}), tr, h)
		}},
		{name: "heartbeat", build: origin(tools.OriginHeartbeat)},
		{name: "governed heartbeat", build: kind(proactive.TurnHeartbeatLegacy)},
		{name: "sweep", build: kind(proactive.TurnHeartbeatLegacy)},
		{name: "proactive reflection", build: kind(proactive.TurnReflection)},
		{name: "proactive candidate-driven, delivered now", build: func(tr proactive.Trust, _ bool) context.Context {
			return scoped(proactive.TurnProactive, tr, true)
		}},
		{name: "proactive candidate-driven, not delivered", build: func(tr proactive.Trust, _ bool) context.Context {
			return scoped(proactive.TurnProactive, tr, false)
		}},
		{name: "system channel", build: func(tr proactive.Trust, h bool) context.Context {
			ctx := tools.WithToolOrigin(context.Background(), tools.OriginInternal)
			return with(tools.WithToolContext(ctx, "system", "x"), tr, h)
		}},
		{name: "untagged", build: func(tr proactive.Trust, h bool) context.Context { return with(context.Background(), tr, h) }},
	}
}

// TestPolicyEngine_NeverLooserThanToday is the safety property of the shadow
// release: for every tool a gateway can register (the real registry built with every
// tool on, every classified name, and MCP-style names nothing classifies), in every
// kind of turn, at every trust, with and without the registry's note of the call,
// with the kill switch on and off, approvals on and off, a tool requiring operator
// approval, paper mode on and off and the new brakes on and off, the engine acts
// ONLY where today's checks let the call through. Every other verdict (ask,
// notify_only, deny) is not more permissive than a refusal.
func TestPolicyEngine_NeverLooserThanToday(t *testing.T) {
	cfg := &config.Config{Agents: config.AgentsConfig{
		Defaults: config.AgentDefaults{Workspace: t.TempDir(), Model: "char-model", MaxTokens: 4096, MaxToolIterations: 10},
	}}
	enableEveryTool(cfg)
	cfg.Proactive.Enabled = true
	cfg.Proactive.Intents.Enabled = true
	al := NewAgentLoop(cfg, bus.NewMessageBus(), &charScriptedProvider{})
	t.Cleanup(al.Close)

	seen := map[string]bool{}
	var names []string
	add := func(n string) {
		if !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	registered := al.GetRegistry().GetDefaultAgent().Tools.List()
	for _, n := range registered {
		add(n)
	}
	for _, n := range tools.ClassifiedNames() {
		add(n)
	}
	for _, n := range []string{"mcp_github_create_issue", "mcp_fs_write", "some_unregistered_tool", ""} {
		add(n)
	}
	sort.Strings(names)
	if len(registered) < 60 || len(names) < len(registered) {
		t.Fatalf("the universe is too small: %d registered, %d names", len(registered), len(names))
	}

	// No provider is named, so no order is valued over the network and no account is looked up.
	args := func(name string) map[string]any {
		switch name {
		case tools.NameCreateOrder, tools.NameProposeOrder:
			return map[string]any{"symbol": "BTC/USDT", "side": "buy", "amount": 1.0, "price": 10.0, "confirm": true}
		case tools.NameExecuteDCAOrder:
			return map[string]any{"plan_id": 7.0}
		}
		return nil
	}

	prevNotifier := tools.SetApprovalNotifier(func(context.Context, string, string, string) error { return nil })
	t.Cleanup(func() {
		tools.SetApprovalNotifier(prevNotifier)
		tools.SetRequiredApprovals(nil)
		tools.SetProposalConfig(nil)
		tools.SetPolicyConfig(false)
	})

	trusts := []proactive.Trust{proactive.TrustSystem, proactive.TrustExchange, proactive.TrustUntrusted}
	bools := []bool{false, true}
	var checked, bothAct, stricter int
	var liveAct, liveStricter, liveHeld, liveOwnerActs int
	var failures []string
	for _, ownerless := range bools {
		for _, approvals := range bools {
			for _, paper := range bools {
				for _, brakes := range bools {
					for _, hold := range bools {
						c := config.DefaultConfig()
						c.Proactive.Enabled = true
						if !ownerless {
							c.Proactive.Owner = config.OwnerConfig{Channel: "telegram", ChatID: "1001", SenderID: parityOwner}
						}
						c.Proactive.Approvals = config.ApprovalsConfig{Enabled: approvals, ExpiryMinutes: 10}
						c.TradingRisk.PaperTradingMode = paper
						if brakes {
							c.TradingRisk.MaxOrderValueUSD = 1000
							c.TradingRisk.MaxOrdersPer10Min = 1
							c.TradingRisk.MaxSymbolSharePct = 10
							c.TradingRisk.SymbolAllowlist = []string{"ETH/USDT"}
						}
						tools.SetPolicyConfig(paper)
						tools.SetProposalConfig(c)
						if hold {
							tools.SetRequiredApprovals([]string{tools.NameCreateOrder})
						} else {
							tools.SetRequiredApprovals(nil)
						}
						for _, killed := range bools {
							isKilled := killed
							eng := tools.NewPolicyEngine(func() *config.Config { return c }, func() bool { return isKilled }, nil, nil)
							// The same engine with the owner's autonomy dial, whose setting rotates
							// over the cases: it may only tighten, and never touches the owner's turn.
							dialSetting := "default"
							dialEng := tools.NewPolicyEngine(func() *config.Config { return c }, func() bool { return isKilled }, nil, nil)
							dialEng.SetOwnerDial(func(context.Context, policy.Tier) (string, error) { return dialSetting, nil })
							// Live counts an allowed order, so each live probe gets an engine of its own.
							liveEng := func() *policy.Engine {
								return tools.NewPolicyEngine(func() *config.Config { return c }, func() bool { return isKilled }, nil, nil)
							}
							for _, tc := range parityCases() {
								if tc.ownerless && !ownerless {
									continue
								}
								for _, trust := range trusts {
									for _, holder := range bools {
										for _, note := range bools {
											for _, name := range names {
												ctx := tc.build(trust, holder)
												if note {
													tools.NoteToolCalls(ctx, name) // as the registry does, just before the check
												}
												legacy, v := tools.PolicyProbe(ctx, eng, name, args(name))
												checked++
												dialSetting = []string{"ask", "notify", "off"}[checked%3]
												_, dv := tools.PolicyProbe(ctx, dialEng, name, args(name))
												if dv.Mode.Rank() < v.Mode.Rank() && len(failures) < 20 {
													failures = append(failures, fmt.Sprintf("DIAL-LOOSER: %s / tool %q / dial %s: %s -> %s (%v)", tc.name, name, dialSetting, v.Mode, dv.Mode, dv.Reasons))
												}
												if (tc.name == "owner at the console" || (tc.name == "owner user root" && !ownerless)) && dv.Mode != v.Mode && len(failures) < 20 {
													failures = append(failures, fmt.Sprintf("DIAL-OWNER: %s / tool %q / dial %s changed the owner's own turn: %s -> %s", tc.name, name, dialSetting, v.Mode, dv.Mode))
												}
												// The kill switch does not take the owner's hands off: it never
												// decides the owner's own root turn or the console.
												isOwner := tc.name == "owner at the console" || (tc.name == "owner user root" && !ownerless)
												if isOwner && len(v.Reasons) > 0 && strings.HasPrefix(v.Reasons[0], "K:") && len(failures) < 20 {
													failures = append(failures, fmt.Sprintf("KILL: %s / kill %v / tool %q: the kill switch decided the owner's own turn", tc.name, killed, name))
												}
												// Live: the outcome is today's refusal OR the engine's, never an
												// allow that either refused. (A held tool whose approval was granted
												// is today's decision: the engine's L-ask for it is answered.)
												liveLegacy, liveOK, lv := tools.PolicyProbeLive(ctx, liveEng(), name, args(name))
												held := v.Mode == policy.ModeAsk && len(v.Reasons) > 0 && v.Reasons[0] == "L: the tool requires operator approval"
												switch {
												case liveLegacy != legacy:
													failures = append(failures, fmt.Sprintf("LIVE-PROBE: %s / tool %q: legacy %v vs %v", tc.name, name, legacy, liveLegacy))
												case liveOK && !legacy:
													failures = append(failures, fmt.Sprintf("LIVE-LOOSER: %s / trust %s / tool %q: today refuses, live allows (%v)", tc.name, trust, name, lv.Reasons))
												case liveOK && v.Mode != policy.ModeAct && !held && tools.ClassOf(name) != tools.ClassUnknown && moneyClass(name):
													failures = append(failures, fmt.Sprintf("LIVE-LOOSER-THAN-ENGINE: %s / trust %s / tool %q: the engine says %s %v, live allows", tc.name, trust, name, v.Mode, v.Reasons))
												case liveOK:
													liveAct++
													if held && v.Mode != policy.ModeAct {
														liveHeld++
													}
												case legacy:
													liveStricter++
												}
												// The owner keeps their hands: the owner's own root turn (no taint
												// holder) is refused by nothing in live but gate B's limits the owner
												// configured, and exits never.
												if isOwner && !holder && !brakes && legacy {
													if !liveOK && len(failures) < 20 {
														failures = append(failures, fmt.Sprintf("OWNER-REFUSED: %s / kill %v / tool %q: %s %v", tc.name, killed, name, lv.Mode, lv.Reasons))
													}
													if liveOK {
														liveOwnerActs++
													}
												}
												switch {
												case !legacy && v.Mode == policy.ModeAct:
													if len(failures) < 20 {
														failures = append(failures, fmt.Sprintf(
															"LOOSER: %s / trust %s / holder %v / note %v / kill %v / approvals %v / paper %v / brakes %v / hold %v / tool %q: today refuses, the engine acts (%v)",
															tc.name, trust, holder, note, killed, approvals, paper, brakes, hold, name, v.Reasons))
													}
												case legacy && v.Mode == policy.ModeAct:
													bothAct++
												case legacy:
													stricter++
												}
											}
										}
									}
								}
							}
						}
					}
				}
			}
		}
	}
	for _, f := range failures {
		t.Error(f)
	}
	if bothAct == 0 || stricter == 0 {
		t.Fatalf("the matrix is vacuous: %d calls both allow, %d the engine is stricter on", bothAct, stricter)
	}
	if liveAct == 0 || liveStricter == 0 || liveOwnerActs == 0 {
		t.Fatalf("the live matrix is vacuous: live acts on %d, stricter on %d, owner acts on %d", liveAct, liveStricter, liveOwnerActs)
	}
	t.Logf("live: acts where both act on %d calls (%d of them held tools whose approval was granted), refuses %d calls today allows, owner root turn acts on %d",
		liveAct, liveHeld, liveStricter, liveOwnerActs)
	t.Logf("checked %d cases over %d tool names: both act on %d, the engine is stricter on %d allowed calls, looser on %d",
		checked, len(names), bothAct, stricter, len(failures))
}

// moneyClass is whether the live hook is asked about the tool at all (the classes
// that move money); for every other class live is exactly today's decision.
func moneyClass(name string) bool {
	switch tools.ClassOf(name) {
	case tools.ClassTrade, tools.ClassFunds, tools.ClassPropose:
		return true
	}
	return false
}
