package policy

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// RowResult is what a tool's own policy row says about a call.
type RowResult int

// The row results.
const (
	// RowPass: the row has nothing against the call.
	RowPass RowResult = iota
	// RowBlocked: the row refuses the call (a blocked origin, paper mode).
	RowBlocked
	// RowHold: the row holds the call until the operator approves it.
	RowHold
)

// Mandates is the read-only mandate lookup. Covering reports the id of an ACTIVE
// mandate that covers the request, if there is one. A nil Mandates covers nothing.
type Mandates interface {
	Covering(ctx context.Context, r ActionRequest) (id string, ok bool)
}

// Valuer prices an order in USD. It may call the network, so the engine bounds
// it with a timeout; ok is false when the value cannot be established.
type Valuer interface {
	NotionalUSD(ctx context.Context, r ActionRequest) (usd float64, ok bool)
}

// Exposure reports what share of the portfolio the order's symbol would hold
// once the order (worth usd) is placed, as a percentage.
type Exposure interface {
	ShareAfter(ctx context.Context, r ActionRequest, usd float64) (pct float64, ok bool)
}

// Deps is what the engine reads. Everything is optional and fails closed: no
// ceiling denies, no configuration denies a money action, an order that cannot
// be valued is refused while a USD cap is set.
type Deps struct {
	// Cfg returns the running configuration; it is read on every call so that a
	// reload takes effect.
	Cfg func() *config.Config
	// Killed reports the kill switch; nil means never killed, as everywhere else.
	Killed func() bool
	// MayCall is today's class ceiling (pkg/tools turnMayCall). The engine calls
	// it and does not re-implement it.
	MayCall func(ctx context.Context, tool string) bool
	// Row is today's per-tool policy row (blocked origin, paper mode, required
	// approval), without its side effects.
	Row func(ctx context.Context, tool string) (RowResult, string)

	Mandates  Mandates
	Valuer    Valuer
	Exposure  Exposure
	LossCheck func(limitUSD float64, accountKey string) error
	Now       func() time.Time
}

// Brake windows and tolerances.
const (
	rateWindow    = 10 * time.Minute
	dupWindow     = 60 * time.Second
	dupTolerance  = 0.01
	valueTimeout  = 3 * time.Second
	maxRecentKept = 1024
)

// Engine decides. Its only state is the memory of recent placing orders that
// the rate and duplicate brakes read. It is safe for concurrent use.
type Engine struct {
	d Deps

	mu     sync.Mutex
	recent []stamp
}

type stamp struct {
	at     time.Time
	symbol string
	side   string
	amount float64
}

// New builds an engine.
func New(d Deps) *Engine {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.LossCheck == nil {
		d.LossCheck = broker.GlobalLossTracker.CheckDailyLossFor
	}
	return &Engine{d: d}
}

// Ceiling answers whether the turn ctx describes may call the tool at all. It is
// today's ceiling (the same predicate ExecuteWithContext enforces), asked through
// the engine. A ceiling depends on the context - origin, sub-turn, DCA job scope,
// whether the policy is armed - so it is keyed on the context, not on a turn kind
// alone.
func (e *Engine) Ceiling(ctx context.Context, tool string) bool {
	return e.d.MayCall != nil && e.d.MayCall(ctx, tool)
}

type column int

const (
	colUser    column = iota // the owner's own conversation
	colTrusted               // an autonomous turn whose inputs were all exchange data or better
	colTainted               // everything else: tainted, untrusted, another person, unknown
)

// Authorize decides the request. The gates run in order and the first that
// restricts decides: a later gate never overrides an earlier one.
//
//	L  today's ceiling and per-tool rows (kept so the engine is never looser)
//	K  kill switch (not for the owner's own root conversation turn)
//	A  provenance
//	B  hard limits and brakes
//	C  tier x turn x trust: the autonomy dial
//	D  utility (Horvitz thresholds, MR-503): not built. It would sit after C and
//	   could only degrade act -> ask -> notify, never loosen.
func (e *Engine) Authorize(ctx context.Context, r ActionRequest) Verdict {
	covered := e.covered(ctx, r)
	v := Verdict{Tier: tierOf(r, covered)}

	verdictOf := func(gate string, m Mode, why string) Verdict {
		v.Mode = m
		v.Reasons = []string{gate + ": " + why}
		return v
	}
	proposal := func(gate string, m Mode, why string) Verdict {
		// A proposal tool's call is the question itself, so "ask" lets it through.
		if r.IsProposal && m == ModeAsk {
			return verdictOf(gate, ModeAct, why+" (the proposal is the question)")
		}
		return verdictOf(gate, m, why)
	}

	if m, why := e.legacy(ctx, r); m != ModeAct {
		return verdictOf("L", m, why)
	}
	// The kill switch stops the assistant acting on its own; it does not take the
	// owner's hands off. So it does not apply to the owner's own root conversation
	// turn, and applies to every other turn: non-owner, sub-turn, autonomous, untagged.
	if columnOf(r) != colUser && e.killed() && r.Class.movesMoney() && !(r.ReduceOnly || r.Protective) {
		return verdictOf("K", ModeDeny, "kill switch engaged and the action is not protective")
	}
	if m, why := provenance(r, v.Tier); m != ModeAct {
		return verdictOf("A", m, why)
	}
	if m, why := e.limits(ctx, r); m != ModeAct {
		return verdictOf("B", m, why)
	}
	m, why := dial(r, v.Tier, covered)
	return proposal("C", m, why)
}

func (e *Engine) killed() bool { return e.d.Killed != nil && e.d.Killed() }

// legacy is gate L: the ceiling and the tool's own policy row.
func (e *Engine) legacy(ctx context.Context, r ActionRequest) (Mode, string) {
	if !e.Ceiling(ctx, r.Tool) {
		return ModeDeny, "the turn's ceiling does not allow this tool"
	}
	if e.d.Row != nil {
		switch res, msg := e.d.Row(ctx, r.Tool); res {
		case RowBlocked:
			return ModeDeny, "the tool's policy row refuses this call: " + msg
		case RowHold:
			return ModeAsk, "the tool requires operator approval"
		}
	}
	return ModeAct, ""
}

func (e *Engine) covered(ctx context.Context, r ActionRequest) bool {
	if e.d.Mandates == nil || r.Class != ClassTrade || r.ReduceOnly || r.Protective {
		return false
	}
	_, ok := e.d.Mandates.Covering(ctx, r)
	return ok
}

// tierOf computes the tier from class, direction and mandate.
func tierOf(r ActionRequest, covered bool) Tier {
	switch r.Class {
	case ClassRead, ClassNotify, ClassLocalState:
		return T0Read
	case ClassFunds:
		return T4Funds
	case ClassTrade, ClassPropose:
		switch {
		case r.ReduceOnly || r.Protective:
			return T1Protective
		case covered:
			return T2Mandated
		}
	}
	// Discretionary trades, and every class whose worst case is not reading:
	// automation, system, and a tool with no class.
	return T3Discretionary
}

// provenance is gate A: an action that moves money (or proposes to) on the
// strength of text nobody vouched for may at most be asked about.
func provenance(r ActionRequest, t Tier) (Mode, string) {
	if !r.Class.movesMoney() || r.Trust >= proactive.TrustExchange {
		return ModeAct, ""
	}
	why := fmt.Sprintf("trust %s is below exchange on a %s action (%s)", r.Trust, r.Class, t)
	if r.IsProposal {
		// A proposal built on text nobody vouched for is not shown to the owner as a
		// card at any tier: it is at most mentioned. (A reduce-only one would be an
		// "ask" below, but a card is exactly what a forged "sell all BTC now" wants.)
		return ModeNotifyOnly, why
	}
	switch t {
	case T1Protective:
		return ModeAsk, why
	case T4Funds:
		return ModeDeny, why
	}
	return ModeNotifyOnly, why
}

func columnOf(r ActionRequest) column {
	switch {
	case r.Actor == ActorOwner && r.TurnKind == proactive.TurnUser:
		return colUser
	case r.Actor == ActorNone && r.Trust >= proactive.TrustExchange:
		return colTrusted
	}
	return colTainted
}

// dial is gate C, the default autonomy dial. T1 on an autonomous turn is always
// ask: there is no automatic protective recovery.
func dial(r ActionRequest, t Tier, covered bool) (Mode, string) {
	col := columnOf(r)
	m := ModeAct
	switch {
	case t == T0Read || col == colUser:
	case t == T4Funds:
		m = ModeDeny
	case t == T1Protective:
		m = ModeAsk
	case t == T2Mandated && col == colTrusted:
		if !covered {
			m = ModeAsk
		}
	case col == colTrusted: // T3
		m = ModeAsk
	default: // T2, T3 on a tainted, untrusted or foreign turn
		m = ModeNotifyOnly
	}
	return m, fmt.Sprintf("dial %s, column %d", t, col)
}

// limits is gate B: permission, the USD cap and the brakes. Every brake is off
// unless configured, and reduce-only and protective actions are exempt from all
// of them: a safety brake never blocks an exit.
func (e *Engine) limits(ctx context.Context, r ActionRequest) (Mode, string) {
	if !r.Class.movesMoney() {
		return ModeAct, ""
	}
	cfg := e.cfg()
	if cfg == nil {
		return ModeDeny, "no configuration to check the limits against"
	}
	if r.Provider != "" {
		scope := config.ScopeTrade
		if r.Class == ClassFunds {
			scope = config.ScopeTransfer
		}
		// A denied permission decides at once: the brakes below may value the order
		// over the network and build a provider the call would never have reached.
		if err := broker.CheckPermission(cfg, r.Provider, r.Account, scope); err != nil {
			return ModeDeny, "permission: the account lacks the " + string(scope) + " permission"
		}
	}
	if r.ReduceOnly || r.Protective {
		return ModeAct, ""
	}
	if fails := e.brakes(ctx, cfg, r); len(fails) > 0 {
		return ModeDeny, strings.Join(fails, "; ")
	}
	return ModeAct, ""
}

func (e *Engine) cfg() *config.Config {
	if e.d.Cfg == nil {
		return nil
	}
	return e.d.Cfg()
}

// brakes are the checks that apply to a risk-increasing action only.
func (e *Engine) brakes(ctx context.Context, cfg *config.Config, r ActionRequest) []string {
	tr := cfg.TradingRisk
	var fails []string
	if r.Futures {
		if err := broker.CheckLeverage(cfg, r.Tool, broker.RiskIncrease); err != nil {
			fails = append(fails, "leverage: trading_risk.allow_leverage is off")
		}
	}
	if r.Provider != "" && tr.DailyLossLimitUSD > 0 && (r.Class == ClassTrade || r.Class == ClassPropose) {
		if err := e.d.LossCheck(tr.DailyLossLimitUSD, broker.AccountKey(cfg, r.Provider, r.Account)); err != nil {
			fails = append(fails, "loss guard: the daily loss limit does not allow adding risk")
		}
	}
	o := r.Order
	if o == nil {
		return fails
	}
	if len(tr.SymbolAllowlist) > 0 && !allowed(tr.SymbolAllowlist, o.Symbol) {
		fails = append(fails, "allowlist: the symbol is not in trading_risk.symbol_allowlist")
	}

	needUSD := tr.MaxOrderValueUSD > 0 || tr.MaxSymbolSharePct > 0
	var usd float64
	var known bool
	if needUSD {
		usd, known = e.value(ctx, r)
	}
	if tr.MaxOrderValueUSD > 0 {
		switch {
		case !known:
			fails = append(fails, "cap: the order cannot be valued in USD while max_order_value_usd is set")
		case broker.CheckRiskNotional(cfg, usd) != nil:
			fails = append(fails, "cap: the order exceeds max_order_value_usd")
		}
	}
	if tr.MaxSymbolSharePct > 0 {
		pct, ok := math.NaN(), false
		if known && e.d.Exposure != nil {
			pct, ok = e.d.Exposure.ShareAfter(ctx, r, usd)
		}
		switch {
		case !ok || math.IsNaN(pct) || math.IsInf(pct, 0):
			fails = append(fails, "share: the symbol's share of the portfolio cannot be measured")
		case pct > tr.MaxSymbolSharePct:
			fails = append(fails, "share: the symbol would exceed max_symbol_share_pct")
		}
	}
	if o.Placing {
		fails = append(fails, e.recentFails(tr.MaxOrdersPer10Min, o)...)
	}
	return fails
}

func allowed(list []string, symbol string) bool {
	for _, s := range list {
		if strings.EqualFold(strings.TrimSpace(s), strings.TrimSpace(symbol)) {
			return true
		}
	}
	return false
}

func (e *Engine) value(ctx context.Context, r ActionRequest) (float64, bool) {
	finite := func(v float64) bool { return v >= 0 && !math.IsNaN(v) && !math.IsInf(v, 0) }
	if r.Order.USDKnown {
		return r.Order.USD, finite(r.Order.USD)
	}
	if e.d.Valuer == nil {
		return 0, false
	}
	ctx, cancel := context.WithTimeout(ctx, valueTimeout)
	defer cancel()
	usd, ok := e.d.Valuer.NotionalUSD(ctx, r)
	return usd, ok && finite(usd)
}

// recentFails runs the order-rate throttle (limit 0 = off) and the duplicate
// guard against the orders this engine has been told were placed.
func (e *Engine) recentFails(limit int, o *Order) []string {
	now := e.d.Now()
	e.mu.Lock()
	defer e.mu.Unlock()
	e.trimLocked(now)
	var fails []string
	if limit > 0 && len(e.recent) >= limit {
		fails = append(fails, fmt.Sprintf("rate: %d orders in the last %d minutes (max_orders_per_10min)", len(e.recent), int(rateWindow.Minutes())))
	}
	for _, s := range e.recent {
		if now.Sub(s.at) <= dupWindow && strings.EqualFold(s.symbol, o.Symbol) && strings.EqualFold(s.side, o.Side) &&
			s.amount > 0 && math.Abs(o.Amount-s.amount)/s.amount <= dupTolerance {
			fails = append(fails, "duplicate: the same symbol, side and amount (within 1%) was placed in the last 60 seconds")
			break
		}
	}
	return fails
}

func (e *Engine) trimLocked(now time.Time) {
	keep := e.recent[:0]
	for _, s := range e.recent {
		if now.Sub(s.at) <= rateWindow {
			keep = append(keep, s)
		}
	}
	e.recent = keep
}

// Note tells the engine that a call went on to place the order r describes, so
// that the rate and duplicate brakes see it. The caller notes only placements it
// knows were allowed; reduce-only and protective orders are never counted.
func (e *Engine) Note(r ActionRequest) {
	o := r.Order
	if o == nil || !o.Placing || r.ReduceOnly || r.Protective {
		return
	}
	now := e.d.Now()
	e.mu.Lock()
	defer e.mu.Unlock()
	e.trimLocked(now)
	if len(e.recent) >= maxRecentKept {
		e.recent = e.recent[1:]
	}
	e.recent = append(e.recent, stamp{at: now, symbol: o.Symbol, side: o.Side, amount: o.Amount})
}
