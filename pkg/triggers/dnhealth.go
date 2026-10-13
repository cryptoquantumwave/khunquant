package triggers

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral/planhealth"
	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/worldmodel"
)

// KindDNHealth is the delta-neutral plan health trigger: it turns the breach codes of
// a plan's health check into candidates. It replaces the "dn:" cron job's alerting
// (the monitor itself, which reads the exchange, is the plan-health source).
const KindDNHealth = "dn_health"

const (
	// dnMinMaxAge and dnMaxMaxAge bound how old a plan's health may be when it is
	// judged: three of the plan's own check intervals, but never under five minutes
	// (a check takes a while) nor over six hours.
	dnMinMaxAge = 5 * time.Minute
	dnMaxMaxAge = 6 * time.Hour

	dnDefaultCooldown = time.Hour
	dnMaxCooldown     = 3 * 24 * time.Hour

	maxDNNameRunes = 200
	// dnForcedSkill is the skill a Warning's explaining turn carries in full.
	dnForcedSkill = "delta-neutral"
)

// DNHealthParams are the parameters of a dn_health spec: which plan to watch.
//
// Symbol is the plan's futures symbol, used only to name the plan in the message and
// to tell attention which asset it is about. Name is the plan's label, which a person
// typed: it is outside text, cleaned and capped wherever it is used, and shown only
// as a quotation (Candidate.Quote), never in a title, subject, detail or key.
type DNHealthParams struct {
	PlanID int64  `json:"plan_id"`
	Symbol string `json:"symbol,omitempty"`
	Name   string `json:"name,omitempty"`
}

// Validate checks the parameters.
func (p DNHealthParams) Validate() error {
	switch {
	case p.PlanID <= 0:
		return fmt.Errorf("plan_id must be a positive number")
	case p.Symbol != "" && !validAlertSymbol(p.Symbol):
		return fmt.Errorf("symbol must be at most %d letters, digits or '/', ':', '_', '-', '.'", maxAlertTokenLen)
	case !utf8.ValidString(p.Name) || utf8.RuneCountInString(p.Name) > maxDNNameRunes:
		return fmt.Errorf("name must be valid text of at most %d characters", maxDNNameRunes)
	}
	return nil
}

// JSON encodes the parameters for a Spec.
func (p DNHealthParams) JSON() json.RawMessage {
	b, _ := json.Marshal(p) // plain fields: cannot fail
	return b
}

// DNHealthConfig is what the dn_health kind is given by the gateway.
type DNHealthConfig struct {
	// Language returns the language of the owner's messages ("th" for Thai, anything
	// else English). It must be cheap and do no I/O. Nil means English.
	Language func() string
	// Stats receives the counters. Nil counts nothing.
	Stats *DNHealthStats
}

// DNHealthStats count what the kind did and did not act on. Counts only.
type DNHealthStats struct {
	// Evaluated counts plan.health events judged; Fired the candidates built.
	Evaluated, Fired atomic.Uint64
	// Stale counts reports older than allowed, Untrusted those whose trust was below
	// the exchange's, Invalid those whose health could not be read.
	Stale, Untrusted, Invalid atomic.Uint64
	// Silenced counts breach codes left unreported because the owner's silence on the
	// plan was in force.
	Silenced atomic.Uint64
}

// DNHealthFactory builds dn_health triggers.
func DNHealthFactory(cfg DNHealthConfig) Factory {
	if cfg.Stats == nil {
		cfg.Stats = &DNHealthStats{}
	}
	return func(spec Spec) (Trigger, error) {
		var p DNHealthParams
		if err := decodeStrict(spec.Params, &p); err != nil {
			return nil, err
		}
		if err := p.Validate(); err != nil {
			return nil, err
		}
		return &dnHealth{id: spec.Kind + ":" + spec.ID, p: p, cfg: cfg}, nil
	}
}

type dnHealth struct {
	id  string
	p   DNHealthParams
	cfg DNHealthConfig
}

func (t *dnHealth) ID() string { return t.id }

func (t *dnHealth) Kinds() []events.Kind { return []events.Kind{events.KindPerceptionPlanHealth} }

// Demands asks the plan-health source to check the plan. No spec, no demand, no read.
func (t *dnHealth) Demands() []perception.Demand {
	return []perception.Demand{{
		Provider: planhealth.Kind, Symbol: strconv.FormatInt(t.p.PlanID, 10), Kind: perception.DemandPlanHealth,
	}}
}

// Evaluate judges the plan's latest health report and returns one candidate per breach
// code in force.
//
// The report must be the one THIS event brought: the view's stamp for the plan equals
// the diff's time, so a report restored from a checkpoint (which keeps its old stamp)
// and another plan's report are never acted on. It must also be no older than three
// check intervals of the plan (five minutes at the least), come from an exchange-level
// source (trust at least TrustExchange) and decode. The trigger keeps no state: a
// breach that persists yields the same candidate on every report, and attention's
// dedup decides how often the owner hears of it (every CriticalRepeatMinutes for a
// protective Critical, the plan's alert cooldown for the rest).
//
// Nothing here can act on a position. The candidates carry no actions.
func (t *dnHealth) Evaluate(_ context.Context, v worldmodel.View, d worldmodel.Diff, _ *State) ([]proactive.Candidate, error) {
	pv, ok := v.Plan(planhealth.Kind, t.p.PlanID)
	if !ok || !pv.At.Equal(d.At) {
		return nil, nil
	}
	t.cfg.Stats.Evaluated.Add(1)
	h, err := planhealth.Decode(pv.Health)
	if err != nil {
		t.cfg.Stats.Invalid.Add(1)
		return nil, nil
	}
	maxAge := min(max(3*time.Duration(h.IntervalSeconds)*time.Second, dnMinMaxAge), dnMaxMaxAge)
	if !v.Fresh(pv.At, maxAge) {
		t.cfg.Stats.Stale.Add(1)
		return nil, nil
	}
	if pv.Trust < proactive.TrustExchange {
		t.cfg.Stats.Untrusted.Add(1)
		return nil, nil
	}
	trust := MinInputTrust(d, pv.Trust)

	// The owner's silences hold here exactly as in the legacy job: a code silenced by
	// the owner (or by a still-running cooldown the legacy job set) makes no candidate,
	// critical or not. The report carries which codes were silenced when it was read;
	// a silence that has ended or been cleared is simply absent from the next report.
	var out []proactive.Candidate
	for _, code := range h.BreachCodes {
		if slices.Contains(h.SilencedCodes, code) {
			t.cfg.Stats.Silenced.Add(1)
			continue
		}
		out = append(out, t.candidate(code, h, trust, v.Now()))
	}
	t.cfg.Stats.Fired.Add(uint64(len(out)))
	return out, nil
}

// candidate builds the candidate of one breach code. Its title, subject, detail and
// key are made from the plan's number, its symbol and the figures, all formatted
// here; the plan's label, which a person typed, is only in Quote.
//
// A Critical code is protective and a template message with the whole evidence block
// in it: it is never held for a model, and no model text is needed to understand it.
// A Warning is worth an explanation and asks for one (with the delta-neutral skill);
// a Notice is a template.
func (t *dnHealth) candidate(code string, h planhealth.Health, trust proactive.Trust, at time.Time) proactive.Candidate {
	class, _ := planhealth.ClassOf(code)
	thai := t.cfg.isThai()
	plan := strconv.FormatInt(t.p.PlanID, 10)

	c := proactive.Candidate{
		DedupKey:    DNHealthKey(plan, code),
		Subject:     t.subject(plan),
		Title:       dnTitle(thai, plan, t.p.Symbol, code, h),
		Detail:      dnDetail(thai, h, code),
		Quote:       CleanAlertMessage(t.p.Name),
		Evidence:    t.evidence(plan, code, h),
		Trust:       trust,
		DedupWindow: dnCooldown(h),
		At:          at,
	}
	switch class {
	case planhealth.ClassCritical:
		c.Severity, c.Protective, c.Salience, c.Compose = proactive.SeverityCritical, true, 0.95, proactive.ComposeTemplate
	case planhealth.ClassDanger:
		c.Severity, c.Salience, c.Compose = proactive.SeverityWarning, 0.7, proactive.ComposeLLM
		c.ForcedSkills = []string{dnForcedSkill}
	default:
		c.Severity, c.Salience, c.Compose = proactive.SeverityNotice, 0.4, proactive.ComposeTemplate
	}
	return c
}

func (t *dnHealth) subject(plan string) string {
	if t.p.Symbol != "" {
		return t.p.Symbol
	}
	return "plan:" + plan
}

func (t *dnHealth) evidence(plan, code string, h planhealth.Health) map[string]any {
	ev := map[string]any{
		"plan_id": t.p.PlanID, "code": code, "health_score": h.Score, "health_label": h.Label(),
		"liquidation_distance_pct": h.LiquidationDistancePct, "margin_ratio_pct": h.MarginRatioPct,
		"delta_drift_pct": h.DeltaDriftPct, "leg_mismatch_pct": h.LegMismatchPct,
		"funding_apy_pct": h.FundingAPYPct, "funding_rate": h.FundingRate,
		"breach_codes": h.BreachCodes, "recommended_action": planhealth.ActionText(code),
	}
	if t.p.Symbol != "" {
		ev["symbol"] = t.p.Symbol
		if base, _, ok := strings.Cut(t.p.Symbol, "/"); ok && base != "" {
			ev["base_asset"] = base
		}
	}
	if h.PnLKnown {
		ev["pnl_usdt"] = h.PnLUSDT
	}
	return ev
}

// dnCooldown is the plan's alert cooldown as the dedup window of its ordinary
// candidates.
func dnCooldown(h planhealth.Health) time.Duration {
	d := time.Duration(h.CooldownSeconds) * time.Second
	if d <= 0 {
		return dnDefaultCooldown
	}
	return min(d, dnMaxCooldown)
}

func (c DNHealthConfig) isThai() bool {
	return CrossConfig{Language: c.Language}.isThai()
}

func pct1(v float64) string { return strconv.FormatFloat(v, 'f', 1, 64) + "%" }

// dnTitle is the headline: the plan, the finding and the figure that matters.
func dnTitle(thai bool, plan, symbol, code string, h planhealth.Health) string {
	who := "Delta-neutral plan " + plan
	if thai {
		who = "แผน delta-neutral " + plan
	}
	if symbol != "" {
		who += " (" + symbol + ")"
	}
	finding := dnFinding(thai, code)
	switch code {
	case planhealth.CodeLiquidationDistanceLow:
		if thai {
			finding += " — ห่างจุดล้างพอร์ต " + pct1(h.LiquidationDistancePct)
		} else {
			finding += " — " + pct1(h.LiquidationDistancePct) + " from liquidation"
		}
	case planhealth.CodeMarginCritical, planhealth.CodeMarginDanger:
		if thai {
			finding += " — อัตรามาร์จิน " + pct1(h.MarginRatioPct)
		} else {
			finding += " — margin ratio " + pct1(h.MarginRatioPct)
		}
	case planhealth.CodeDeltaDriftHigh:
		finding += " — " + pct1(h.DeltaDriftPct)
	case planhealth.CodeLegMismatch:
		finding += " — " + pct1(h.LegMismatchPct)
	}
	return who + ": " + finding
}

var dnFindingsEN = map[string]string{
	planhealth.CodeDataUnavailable:        "data unavailable, the plan cannot be read",
	planhealth.CodeMarginCritical:         "margin critical",
	planhealth.CodeLiquidationDistanceLow: "liquidation distance low",
	planhealth.CodeFundingNegative:        "funding rate negative",
	planhealth.CodeFundingReversal:        "funding rates reversing",
	planhealth.CodeMarginDanger:           "margin in danger",
	planhealth.CodeDeltaDriftHigh:         "delta drift high",
	planhealth.CodeFundingUnavailable:     "funding rate unavailable",
	planhealth.CodeFundingBelowMin:        "funding below the minimum",
	planhealth.CodeExitSpreadTargetMet:    "exit spread target met",
	planhealth.CodeLegMismatch:            "spot and futures quantities differ",
	planhealth.CodeRecoveryRequired:       "recovery required",
	planhealth.CodeMaxDrawdownBreached:    "drawdown limit breached",
	planhealth.CodeProfitTargetMet:        "profit target met",
}

var dnFindingsTH = map[string]string{
	planhealth.CodeDataUnavailable:        "อ่านข้อมูลแผนไม่ได้",
	planhealth.CodeMarginCritical:         "มาร์จินวิกฤต",
	planhealth.CodeLiquidationDistanceLow: "ใกล้จุดล้างพอร์ตมาก",
	planhealth.CodeFundingNegative:        "ฟันดิ้งเรตติดลบ",
	planhealth.CodeFundingReversal:        "ฟันดิ้งเรตกลับด้าน",
	planhealth.CodeMarginDanger:           "มาร์จินอยู่ในภาวะอันตราย",
	planhealth.CodeDeltaDriftHigh:         "เดลต้าคลาดเคลื่อนสูง",
	planhealth.CodeFundingUnavailable:     "ไม่มีข้อมูลฟันดิ้งเรต",
	planhealth.CodeFundingBelowMin:        "ฟันดิ้งเรตต่ำกว่าขั้นต่ำ",
	planhealth.CodeExitSpreadTargetMet:    "ส่วนต่างราคาออกถึงเป้าแล้ว",
	planhealth.CodeLegMismatch:            "ปริมาณฝั่ง spot กับ futures ไม่ตรงกัน",
	planhealth.CodeRecoveryRequired:       "ต้องกู้คืนแผน",
	planhealth.CodeMaxDrawdownBreached:    "ขาดทุนเกินขีดจำกัด",
	planhealth.CodeProfitTargetMet:        "ถึงเป้ากำไรแล้ว",
}

var dnActionsTH = map[string]string{
	planhealth.CodeDataUnavailable:        "อ่านข้อมูลแผนที่ใช้งานอยู่ไม่ได้ ตรวจสอบเครือข่ายและ API key อาจต้องดำเนินการด้วยตนเอง",
	planhealth.CodeMarginCritical:         "อัตรามาร์จินวิกฤต พิจารณาลดขนาดสถานะหรือปิดแผนทันที",
	planhealth.CodeLiquidationDistanceLow: "ใกล้จุดล้างพอร์ตมาก ลดสถานะหรือเติมมาร์จินทันที",
	planhealth.CodeLegMismatch:            "ปริมาณ spot กับ futures ไม่ตรงกัน ปรับสมดุลฝั่งใดฝั่งหนึ่งเพื่อให้การป้องกันความเสี่ยงกลับมาสมบูรณ์",
	planhealth.CodeRecoveryRequired:       "แผนต้องกู้คืน: การดำเนินการยังไม่เสร็จ ตรวจสอบแต่ละฝั่งแล้วทำให้เสร็จหรือปิดแผนด้วยตนเอง",
	planhealth.CodeMaxDrawdownBreached:    "ขาดทุนของแผนเกินขีดจำกัด ตรวจสอบและพิจารณาปิดแผน",
	planhealth.CodeProfitTargetMet:        "ถึงเป้ากำไรของแผนแล้ว พิจารณาปิดแผนเพื่อล็อกกำไร",
}

func dnFinding(thai bool, code string) string {
	m := dnFindingsEN
	if thai {
		m = dnFindingsTH
	}
	if s, ok := m[code]; ok {
		return s
	}
	return code
}

func dnAction(thai bool, code string) string {
	if thai {
		if s, ok := dnActionsTH[code]; ok {
			return s
		}
	}
	return planhealth.ActionText(code)
}

// dnDetail is the evidence block: the recommended action first (a message that groups
// several candidates shows only the first line of each), then the health and the
// figures the finding rests on. It is complete without any model text, because a
// Critical message is delivered as it is. Four lines at most, built from numbers only.
func dnDetail(thai bool, h planhealth.Health, code string) string {
	var lines []string
	if thai {
		lines = append(lines,
			"คำแนะนำ: "+dnAction(true, code),
			"สุขภาพแผน "+strconv.Itoa(h.Score)+" ("+h.Label()+") · ห่างจุดล้างพอร์ต "+pct1(h.LiquidationDistancePct)+" · อัตรามาร์จิน "+pct1(h.MarginRatioPct),
			"เดลต้าคลาดเคลื่อน "+pct1(h.DeltaDriftPct)+" · ปริมาณไม่ตรงกัน "+pct1(h.LegMismatchPct)+" · ฟันดิ้ง APY "+pct1(h.FundingAPYPct))
	} else {
		lines = append(lines,
			"Recommended: "+dnAction(false, code),
			"Health "+strconv.Itoa(h.Score)+" ("+h.Label()+") · liquidation distance "+pct1(h.LiquidationDistancePct)+" · margin ratio "+pct1(h.MarginRatioPct),
			"Delta drift "+pct1(h.DeltaDriftPct)+" · leg mismatch "+pct1(h.LegMismatchPct)+" · funding APY "+pct1(h.FundingAPYPct))
	}
	if h.PnLKnown {
		if thai {
			lines = append(lines, "กำไร/ขาดทุน "+fmtNumber(h.PnLUSDT)+" USDT")
		} else {
			lines = append(lines, "P&L "+fmtNumber(h.PnLUSDT)+" USDT")
		}
	}
	return strings.Join(lines, "\n")
}
