// Package planhealth is the vocabulary a delta-neutral plan's health is reported in:
// the breach codes, how severe each is, the recommended action for a set of them, the
// label for a score, and the numbers that travel in a perception.plan.health event.
//
// It exists so that the monitor (pkg/deltaneutral), the plan-health source
// (pkg/perception/sources) and the dn_health trigger (pkg/triggers) agree on one
// definition without importing each other: the trigger package may not reach the
// exchange clients that pkg/deltaneutral imports, and the world model carries a plan's
// health only as a map of numbers. Everything here is a pure function of its
// arguments, and the package imports nothing but the standard library.
package planhealth

import (
	"fmt"
	"math"
	"sort"
)

// Kind is the plan family a delta-neutral plan's health is reported under
// (worldmodel.PlanPayload.Kind).
const Kind = "dn"

// Class is how severe a breach code is. Critical codes are protective: the
// condition can deteriorate further under the same code, so they re-alert while
// they persist.
type Class int

const (
	// ClassWarn is a condition worth a note (a target met, a rate below the floor).
	ClassWarn Class = iota
	// ClassDanger is a condition that needs a look.
	ClassDanger
	// ClassCritical is a condition that can end in liquidation, or a blind monitor.
	ClassCritical
)

// The breach codes. The order of the table below is the order of their bits in the
// breach mask that travels in a plan's health (see Health.Encode): append new codes at
// the end, never reorder or remove one.
const (
	CodeDataUnavailable        = "data_unavailable"
	CodeMarginCritical         = "margin_critical"
	CodeLiquidationDistanceLow = "liquidation_distance_low"
	CodeFundingNegative        = "funding_negative"
	CodeFundingReversal        = "funding_reversal"
	CodeMarginDanger           = "margin_danger"
	CodeDeltaDriftHigh         = "delta_drift_high"
	CodeFundingUnavailable     = "funding_unavailable"
	CodeFundingBelowMin        = "funding_below_min"
	CodeExitSpreadTargetMet    = "exit_spread_target_met"
	CodeLegMismatch            = "leg_mismatch"
	CodeRecoveryRequired       = "recovery_required"
	CodeMaxDrawdownBreached    = "max_drawdown_breached"
	CodeProfitTargetMet        = "profit_target_met"
)

type codeInfo struct {
	code  string
	class Class
}

// table is the single definition: bit i of the mask is table[i]. At most 52 codes
// fit a float64 mask exactly.
var table = [...]codeInfo{
	{CodeDataUnavailable, ClassCritical},
	{CodeMarginCritical, ClassCritical},
	{CodeLiquidationDistanceLow, ClassCritical},
	{CodeFundingNegative, ClassDanger},
	{CodeFundingReversal, ClassDanger},
	{CodeMarginDanger, ClassDanger},
	{CodeDeltaDriftHigh, ClassDanger},
	{CodeFundingUnavailable, ClassDanger},
	{CodeFundingBelowMin, ClassWarn},
	{CodeExitSpreadTargetMet, ClassWarn},
	{CodeLegMismatch, ClassDanger},
	{CodeRecoveryRequired, ClassDanger},
	{CodeMaxDrawdownBreached, ClassDanger},
	{CodeProfitTargetMet, ClassWarn},
}

// Codes lists every breach code in bit order.
func Codes() []string {
	out := make([]string, len(table))
	for i, c := range table {
		out[i] = c.code
	}
	return out
}

func index(code string) int {
	for i, c := range table {
		if c.code == code {
			return i
		}
	}
	return -1
}

// ClassOf is the severity class of a code; ok is false for a code not in the table.
func ClassOf(code string) (c Class, ok bool) {
	if i := index(code); i >= 0 {
		return table[i].class, true
	}
	return ClassWarn, false
}

// IsCritical reports whether code is a critical breach code.
func IsCritical(code string) bool {
	c, ok := ClassOf(code)
	return ok && c == ClassCritical
}

// Severity is the worst severity among codes, as the monitor words it: "critical",
// "danger" or "warn" (also for no code, or one it does not know).
func Severity(codes []string) string {
	worst := ClassWarn
	for _, code := range codes {
		if c, ok := ClassOf(code); ok && c > worst {
			worst = c
		}
	}
	switch worst {
	case ClassCritical:
		return "critical"
	case ClassDanger:
		return "danger"
	}
	return "warn"
}

// RecommendedAction is the text recommended for a set of breach codes: the first
// critical code decides among the critical ones, then the groups of danger codes and
// the warnings in turn. Empty for no codes.
func RecommendedAction(codes []string) string {
	if len(codes) == 0 {
		return ""
	}
	for _, code := range codes {
		switch code {
		case CodeDataUnavailable, CodeMarginCritical, CodeLiquidationDistanceLow:
			return ActionText(code)
		}
	}
	switch {
	case hasAny(codes, CodeFundingNegative, CodeFundingUnavailable, CodeMarginDanger):
		return "Plan health has degraded. Review position and consider unwinding or rebalancing."
	case hasAny(codes, CodeLegMismatch):
		return ActionText(CodeLegMismatch)
	case hasAny(codes, CodeDeltaDriftHigh):
		return "Delta drift has exceeded policy threshold. Rebalance positions to restore hedge."
	case hasAny(codes, CodeRecoveryRequired):
		return ActionText(CodeRecoveryRequired)
	case hasAny(codes, CodeMaxDrawdownBreached):
		return ActionText(CodeMaxDrawdownBreached)
	case hasAny(codes, CodeFundingReversal):
		return "Funding rates are reversing. Monitor closely; consider unwinding if reversal sustains."
	case hasAny(codes, CodeFundingBelowMin):
		return "Current funding rate is below policy minimum. Plan profitability is declining."
	case hasAny(codes, CodeExitSpreadTargetMet):
		return "Exit spread target met. Consider unwinding to lock in gains from basis convergence."
	case hasAny(codes, CodeProfitTargetMet):
		return ActionText(CodeProfitTargetMet)
	}
	return "Plan threshold breached. Review metrics and consider appropriate action."
}

// ActionText is the recommended action for a single code, in English. It is advice
// to the owner, never an instruction the system carries out.
func ActionText(code string) string {
	switch code {
	case CodeDataUnavailable:
		return "Data fetch failed for active plan. Check network connectivity and API keys. Manual intervention may be required."
	case CodeMarginCritical:
		return "Margin ratio is critical. Consider reducing position size or unwinding immediately."
	case CodeLiquidationDistanceLow:
		return "Liquidation distance is dangerously low. Reduce position or add margin immediately."
	case CodeLegMismatch:
		return "The spot and futures quantities no longer match. Rebalance a leg to restore the hedge."
	case CodeRecoveryRequired:
		return "The plan needs recovery: an execution did not finish. Review its legs and finish or unwind them by hand."
	case CodeMaxDrawdownBreached:
		return "The plan's loss has passed its drawdown limit. Review it and consider unwinding."
	case CodeProfitTargetMet:
		return "The plan's profit target is met. Consider unwinding to lock in the gain."
	}
	return RecommendedAction([]string{code})
}

func hasAny(codes []string, want ...string) bool {
	for _, c := range codes {
		for _, w := range want {
			if c == w {
				return true
			}
		}
	}
	return false
}

// Health labels, by score. They are the labels of deltaneutral.HealthLabel*.
const (
	LabelExcellent = "excellent"
	LabelHealthy   = "healthy"
	LabelWatch     = "watch"
	LabelDanger    = "danger"
	LabelCritical  = "critical"
)

// LabelForScore is the label of a 0-100 health score.
func LabelForScore(score int) string {
	switch {
	case score >= 90:
		return LabelExcellent
	case score >= 75:
		return LabelHealthy
	case score >= 60:
		return LabelWatch
	case score >= 40:
		return LabelDanger
	}
	return LabelCritical
}

// The keys of the numbers in a perception.plan.health payload. The world model keeps
// at most 16 of them, each name a short token.
const (
	keyMask     = "breach_mask"
	keySilenced = "silenced_mask"
	keyScore    = "score"
	keyLiq      = "liq_distance_pct"
	keyMargin   = "margin_ratio_pct"
	keyDrift    = "delta_drift_pct"
	keyLeg      = "leg_mismatch_pct"
	keyAPY      = "funding_apy_pct"
	keyRate     = "funding_rate"
	keyPnL      = "pnl_usdt"
	keyInterval = "interval_s"
	keyCooldown = "cooldown_s"
	keyDataOK   = "data_ok"
)

// MaxEntries is how many numbers Encode produces at most: within what the world
// model keeps (16).
const MaxEntries = 13

// Health is what one check of a plan reported, in the form a trigger needs it.
type Health struct {
	// Score is the 0-100 health score.
	Score int
	// BreachCodes are the codes in force.
	BreachCodes []string
	// SilencedCodes are the codes in force that the owner has silenced for now (the
	// plan's silences, read with the check): the trigger makes no candidate for them.
	SilencedCodes []string

	LiquidationDistancePct float64
	MarginRatioPct         float64
	DeltaDriftPct          float64
	LegMismatchPct         float64
	FundingAPYPct          float64
	FundingRate            float64
	// PnLUSDT is the plan's profit and loss, when PnLKnown.
	PnLUSDT  float64
	PnLKnown bool

	// IntervalSeconds is how often the plan is checked; CooldownSeconds the plan's
	// alert cooldown. DataOK is false when a leg could not be read.
	IntervalSeconds int
	CooldownSeconds int
	DataOK          bool
}

// Label is the label of the score.
func (h Health) Label() string { return LabelForScore(h.Score) }

// Mask is the bit mask of the breach codes. Codes not in the table are left out.
func Mask(codes []string) uint64 {
	var m uint64
	for _, c := range codes {
		if i := index(c); i >= 0 {
			m |= 1 << uint(i)
		}
	}
	return m
}

// FromMask lists the codes whose bits are set, in table order. Bits beyond the table
// are ignored.
func FromMask(m uint64) []string {
	var out []string
	for i, c := range table {
		if m&(1<<uint(i)) != 0 {
			out = append(out, c.code)
		}
	}
	return out
}

// Encode is the plan's health as the numbers of a perception.plan.health payload. A
// non-finite number is encoded as 0: the world model refuses a payload that holds
// one, which would blind the trigger.
func (h Health) Encode() map[string]float64 {
	f := func(v float64) float64 {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return 0
		}
		return v
	}
	m := map[string]float64{
		keyMask:     float64(Mask(h.BreachCodes)),
		keySilenced: float64(Mask(h.SilencedCodes)),
		keyScore:    float64(h.Score),
		keyLiq:      f(h.LiquidationDistancePct),
		keyMargin:   f(h.MarginRatioPct),
		keyDrift:    f(h.DeltaDriftPct),
		keyLeg:      f(h.LegMismatchPct),
		keyAPY:      f(h.FundingAPYPct),
		keyRate:     f(h.FundingRate),
		keyInterval: float64(max(h.IntervalSeconds, 0)),
		keyCooldown: float64(max(h.CooldownSeconds, 0)),
		keyDataOK:   0,
	}
	if h.DataOK {
		m[keyDataOK] = 1
	}
	if h.PnLKnown {
		m[keyPnL] = f(h.PnLUSDT)
	}
	return m
}

// Decode reads a plan's health back from the numbers of the world model. It refuses
// a map that lacks the breach mask or the score, or holds a number out of range.
func Decode(m map[string]float64) (Health, error) {
	mask, ok := m[keyMask]
	if !ok {
		return Health{}, fmt.Errorf("planhealth: no %s", keyMask)
	}
	score, ok := m[keyScore]
	if !ok {
		return Health{}, fmt.Errorf("planhealth: no %s", keyScore)
	}
	for k, v := range m {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return Health{}, fmt.Errorf("planhealth: %s is not finite", k)
		}
	}
	if mask < 0 || mask >= 1<<52 || mask != math.Trunc(mask) {
		return Health{}, fmt.Errorf("planhealth: bad %s", keyMask)
	}
	if score < 0 || score > 100 {
		return Health{}, fmt.Errorf("planhealth: bad %s", keyScore)
	}
	silenced := m[keySilenced] // absent in a report that predates silences: none
	if silenced < 0 || silenced >= 1<<52 || silenced != math.Trunc(silenced) {
		return Health{}, fmt.Errorf("planhealth: bad %s", keySilenced)
	}
	h := Health{
		Score:                  int(score),
		BreachCodes:            FromMask(uint64(mask)),
		SilencedCodes:          FromMask(uint64(silenced)),
		LiquidationDistancePct: m[keyLiq],
		MarginRatioPct:         m[keyMargin],
		DeltaDriftPct:          m[keyDrift],
		LegMismatchPct:         m[keyLeg],
		FundingAPYPct:          m[keyAPY],
		FundingRate:            m[keyRate],
		IntervalSeconds:        int(math.Min(m[keyInterval], 7*24*3600)),
		CooldownSeconds:        int(math.Min(m[keyCooldown], 30*24*3600)),
		DataOK:                 m[keyDataOK] == 1,
	}
	if v, has := m[keyPnL]; has {
		h.PnLUSDT, h.PnLKnown = v, true
	}
	return h, nil
}

// SortedCodes returns the codes in table order, without duplicates and without any
// code the table does not know.
func SortedCodes(codes []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, c := range codes {
		if index(c) >= 0 && !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return index(out[i]) < index(out[j]) })
	return out
}
