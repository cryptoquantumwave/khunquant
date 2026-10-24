package tools

import (
	"context"
	"math"
	"strings"
	"sync/atomic"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/exchanges"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/policy"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// shadowRecorder is the policy engine's shadow wiring, armed by the gateway on
// every build and reload and only while proactive is on and proactive.policy.mode
// is shadow or live. Nil (the default, and mode off) means checkToolPolicy never
// builds a request and never calls the engine.
var shadowRecorder atomic.Pointer[policy.Recorder]

// SetPolicyShadow arms the shadow recorder; nil disarms it.
func SetPolicyShadow(rc *policy.Recorder) { shadowRecorder.Store(rc) }

// NewPolicyEngine builds the policy engine over today's checks: the class ceiling
// (turnMayCall), the per-tool rows (rowGate), the kill flag and the running
// configuration. mandates and exposure may be nil (nothing is covered; a symbol's
// share cannot be measured).
func NewPolicyEngine(cfg func() *config.Config, killed func() bool, mandates policy.Mandates, exposure policy.Exposure) *policy.Engine {
	return policy.New(policy.Deps{
		Cfg:    cfg,
		Killed: killed,
		MayCall: func(ctx context.Context, tool string) bool {
			return turnMayCall(ctx, tool)
		},
		Row: func(ctx context.Context, tool string) (policy.RowResult, string) {
			switch kind, msg := rowGate(ctx, tool); kind {
			case rowBlocked:
				return policy.RowBlocked, msg
			case rowHold:
				return policy.RowHold, ""
			}
			return policy.RowPass, ""
		},
		Mandates: mandates,
		Valuer:   orderValuer{cfg: cfg},
		Exposure: exposure,
	})
}

// observeShadow asks the engine about a call whose outcome the existing checks
// have already decided, and records both. It never changes the outcome: a panic
// anywhere in it is recovered and logged.
func observeShadow(ctx context.Context, name string, args map[string]any, legacyAllowed bool) {
	rc := shadowRecorder.Load()
	if rc == nil {
		return
	}
	defer func() {
		if p := recover(); p != nil {
			logger.ErrorCF("policy", "The policy shadow path panicked; the call is unaffected",
				map[string]any{"tool": name, "panic": p})
		}
	}()
	rc.Observe(ctx, buildActionRequest(ctx, name, args), legacyAllowed)
}

// PolicyProbe asks the existing enforcement and the engine about a call without
// running the tool, and returns whether the enforcement allowed it and what the
// engine decided. It exists so that a test over every registered tool can compare
// the two; it records nothing. It does not note the call for the turn's taint (the
// registry does that just before the check): a test that wants the registry's
// behaviour calls NoteToolCalls first.
func PolicyProbe(ctx context.Context, e *policy.Engine, name string, args map[string]any) (legacyAllowed bool, v policy.Verdict) {
	legacyAllowed = enforceToolPolicy(ctx, name, args, false) == nil
	return legacyAllowed, e.Authorize(ctx, buildActionRequest(ctx, name, args))
}

// turnIdentity says what kind of turn ctx is and who, if anyone, is behind it.
// Anything it cannot establish is autonomous or foreign, never the owner.
func turnIdentity(ctx context.Context) (proactive.TurnKind, policy.Actor) {
	origin := ToolOrigin(ctx)
	switch {
	case IsSubTurn(ctx):
		return proactive.TurnSubTurn, policy.ActorNone
	case origin == OriginProactive:
		return ProactiveTurnKind(ctx), policy.ActorNone
	case origin == OriginHeartbeat:
		return proactive.TurnHeartbeatLegacy, policy.ActorNone
	case origin == OriginCron:
		return proactive.TurnCronLegacy, policy.ActorNone
	case origin == "":
		return "", policy.ActorNone
	}
	if _, v := checkOwnerTurn(ctx, armedConfig.Load(), true); v == ownerYes {
		return proactive.TurnUser, policy.ActorOwner
	}
	return proactive.TurnUser, policy.ActorOther
}

// turnTrust is the turn's provenance floor: its taint when it has one, else what
// the turn is. A turn a person typed is user-authored; an autonomous turn with no
// taint holder has no tracked provenance, which is untrusted.
func turnTrust(ctx context.Context, actor policy.Actor) proactive.Trust {
	if t := proactive.TaintFrom(ctx); t != nil {
		return t.Trust()
	}
	if actor != policy.ActorNone {
		return proactive.TrustUserAuthored
	}
	return proactive.TrustUntrusted
}

// buildActionRequest turns what a call already has - its name, arguments and
// context - into the engine's request. It does no I/O.
func buildActionRequest(ctx context.Context, name string, args map[string]any) policy.ActionRequest {
	class := ClassOf(name)
	if name == NameRemember || name == NameProposeReflection {
		class = ClassLocalState // the engine is about money; see applies
	}
	r := policy.ActionRequest{Tool: name, Class: policy.Class(class.String()), IsProposal: name == NameProposeOrder}
	r.TurnKind, r.Actor = turnIdentity(ctx)
	r.Trust = turnTrust(ctx, r.Actor)
	if riskReducingTools[name] {
		r.Protective, r.ReduceOnly = true, true
	}
	if class != ClassTrade && class != ClassFunds && class != ClassPropose {
		return r
	}
	r.Provider, r.Account = stringArg(args, "provider"), stringArg(args, "account")
	r.Futures = !r.Protective && (strings.HasPrefix(name, "futures_") || stringArg(args, "market") == "futures")
	switch name {
	case NameCreateOrder:
		params, _ := args["params"].(map[string]any)
		symbol, side := stringArg(args, "symbol"), stringArg(args, "side")
		r.ReduceOnly = !orderAddsRisk(symbol, side, params, false)
		r.Order = orderFacts(args, symbol, side)
		confirm, _ := args["confirm"].(bool)
		r.Order.Placing = confirm
	case NameProposeOrder:
		symbol, side := stringArg(args, "symbol"), stringArg(args, "side")
		reduce, _ := args["reduce_only"].(bool)
		r.ReduceOnly = !orderAddsRisk(symbol, side, nil, reduce)
		r.Futures = r.Futures || strings.Contains(symbol, ":")
		r.Order = orderFacts(args, symbol, side)
		if usd := numberArg(args, "notional_usd"); usd > 0 {
			r.Order.USD, r.Order.USDKnown = usd, true
		}
	case NameExecuteDCAOrder:
		if f, ok := args["plan_id"].(float64); ok && f > 0 && f <= math.MaxInt32 {
			r.PlanID = int64(f)
		}
	}
	return r
}

func orderFacts(args map[string]any, symbol, side string) *policy.Order {
	o := &policy.Order{Symbol: symbol, Side: side, Amount: numberArg(args, "amount")}
	if p, ok := args["price"].(float64); ok {
		o.Price = &p
	}
	return o
}

// orderValuer prices an order in USD for the engine's cap. It reads a ticker only
// when it must: an order with an explicit price on a USD-like quote needs no
// network. The engine bounds the call with a timeout; any error is "unknown".
type orderValuer struct{ cfg func() *config.Config }

func (v orderValuer) NotionalUSD(ctx context.Context, r policy.ActionRequest) (float64, bool) {
	o := r.Order
	cfg := v.cfg()
	if o == nil || cfg == nil || r.Provider == "" || o.Symbol == "" {
		return 0, false
	}
	if o.Price != nil && exchanges.USDLike(broker.SymbolQuote(o.Symbol, r.Provider)) {
		return o.Amount * *o.Price, true
	}
	p, err := broker.CreateProviderForAccount(r.Provider, r.Account, cfg)
	if err != nil {
		return 0, false
	}
	md, ok := p.(broker.MarketDataProvider)
	if !ok {
		return 0, false
	}
	est, err := broker.EstimateNotionalUSDWithFallback(ctx, cfg, md, o.Symbol, o.Side, o.Amount, o.Price)
	if err != nil || !est.USDKnown {
		return 0, false
	}
	return est.USD, true
}
