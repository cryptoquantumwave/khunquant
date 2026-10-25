package triggers

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/worldmodel"
)

// KindDCADue is the kind of the trigger that says "a DCA execution is due and its
// gate passes". It runs in SHADOW unless the plan has an active mandate: see the type
// doc of dcaDue.
const KindDCADue = "dca_due"

// DCASpecOrigin is the origin of the specs the gateway keeps in line with the plans.
const DCASpecOrigin = "builtin:dca"

// Bounds of a dca_due spec and its state.
const (
	// MaxDCAGateExpressionRunes and MaxDCAGateIndicators bound the gate a spec copies
	// from its plan; MaxDCAGateVars the numbers of the gate shown as evidence.
	MaxDCAGateExpressionRunes = 1000
	MaxDCAGateIndicators      = 8
	MaxDCAGateVars            = 24
	// MaxDCAGateBars is the most closes the trigger keeps and asks for: the market
	// source's own cap on a series.
	MaxDCAGateBars = 500
	// DCAOutcomesCap bounds the in-memory record of outcomes.
	DCAOutcomesCap = 4096

	// dcaDueMaxAge is how old a dca_due event may be, by its own stamp, when the
	// pipeline gets to it.
	dcaDueMaxAge = 15 * time.Minute
	// dcaGateSlack is how long after a bar should have closed the gate's data may
	// still be the previous bar: the market source's poll and its fetch.
	dcaGateSlack = 15 * time.Minute
	// dcaStateBudget is what the stored closes may take of MaxStateBytes.
	dcaStateBudget = MaxStateBytes - 2<<10
	// dcaDedupWindow is longer than the cron grace, so that a candidate made again
	// after a crash between the decision and the state save is absorbed.
	dcaDedupWindow = 2 * time.Hour
)

// Outcomes of one due slot, as recorded in DCAOutcomes and counted in DCAStats.
const (
	// DCAWouldExecute: every check passed; the legacy path would place the order.
	DCAWouldExecute = "would_execute"
	// DCAGateFalse: the plan's indicator gate was judged false.
	DCAGateFalse = "gate_false"
	// DCAGateUnavailable: the gate could not be judged from the closes the trigger
	// holds (none yet, too old, too few, or it needs more than closes). A mismatch
	// with the legacy path on such a slot is explained by this, not by a bug.
	DCAGateUnavailable = "gate_unavailable"
	// DCAGateError: the gate's expression or an indicator failed.
	DCAGateError = "gate_error"
	// DCACapReached: the plan's cap per period was already used.
	DCACapReached = "cap_reached"
	// DCACapUnknown: the period count could not be read.
	DCACapUnknown = "cap_unknown"
	// DCADisabled, DCAExpired: the plan was off or past its end date.
	DCADisabled = "disabled"
	DCAExpired  = "expired"
	// DCAInvalid: the plan's facts were not usable.
	DCAInvalid = "invalid"
	// DCAStaleEvent: the event was older than a pipeline that keeps up would show.
	DCAStaleEvent = "stale_event"
)

// DCAGateResult is what a DCAGateFunc says about a gate.
type DCAGateResult struct {
	// Met is the verdict, meaningful only when Unavailable is empty and Err is nil.
	Met bool
	// Unavailable is set when the gate cannot be judged from closes alone: why, as a
	// short code ("needs_ohlcv", "too_few_bars").
	Unavailable string
	// Err is a failure of the expression or an indicator.
	Err error
	// Bars is how many closes were judged; Vars the numbers the expression saw, by
	// name ("rsi14", "m.histogram"). Both are shown as evidence.
	Bars int
	Vars map[string]float64
}

// DCAGateFunc judges a plan's gate on closing prices, oldest first. It is pure: the
// gateway builds it over pkg/dca's closes-only evaluation, which this package cannot
// import (pkg/dca reaches the broker). Nil means gates cannot be judged.
type DCAGateFunc func(g perception.DCAGate, closes []float64) DCAGateResult

// DCAConfig is what the dca_due kind is given by the gateway.
type DCAConfig struct {
	// Language returns the owner's language ("th" for Thai, anything else English).
	Language func() string
	// Gate judges gates. Nil makes every gate unavailable.
	Gate DCAGateFunc
	// Stats and Outcomes receive the counters and the per-slot record. Nil keeps
	// nothing.
	Stats    *DCAStats
	Outcomes *DCAOutcomes
}

// DCAStats counts what the dca_due kind did, so that a silence has a number behind
// it. Counts only: no plan, no symbol, no price.
type DCAStats struct {
	// Slots are the due events judged, WouldExecute the candidates made. The others
	// count the slots by why they made none (see the DCA* outcome constants).
	Slots, WouldExecute, GateFalse, GateUnavailable, GateError     atomic.Uint64
	CapReached, CapUnknown, Disabled, Expired, Invalid, StaleEvent atomic.Uint64
	// Duplicates are slots seen before (a replay after a restart).
	Duplicates atomic.Uint64
	// ClosesStored are the candle events kept for a gate; ClosesRejected those refused
	// (untrusted, stale, malformed).
	ClosesStored, ClosesRejected atomic.Uint64
}

// count adds one to the counter of an outcome.
func (s *DCAStats) count(outcome string) {
	switch outcome {
	case DCAWouldExecute:
		s.WouldExecute.Add(1)
	case DCAGateFalse:
		s.GateFalse.Add(1)
	case DCAGateUnavailable:
		s.GateUnavailable.Add(1)
	case DCAGateError:
		s.GateError.Add(1)
	case DCACapReached:
		s.CapReached.Add(1)
	case DCACapUnknown:
		s.CapUnknown.Add(1)
	case DCADisabled:
		s.Disabled.Add(1)
	case DCAExpired:
		s.Expired.Add(1)
	case DCAInvalid:
		s.Invalid.Add(1)
	case DCAStaleEvent:
		s.StaleEvent.Add(1)
	}
}

// DCADueParams are the parameters of a dca_due spec: the plan as it was when the spec
// was last brought in line with it (the gateway does that, off the pipeline), without
// its name. What can change between two syncs and decides a slot (enabled, end date,
// cap and its count) is not here: it comes with each due event.
type DCADueParams struct {
	PlanID     int64               `json:"plan_id"`
	Provider   string              `json:"provider"`
	Account    string              `json:"account,omitempty"`
	Symbol     string              `json:"symbol"`
	Side       string              `json:"side"`
	Amount     float64             `json:"amount"`
	AmountUnit string              `json:"amount_unit"`
	Gate       *perception.DCAGate `json:"gate,omitempty"`
	// MandateID, when set, makes the trigger LIVE for this plan: the gateway sets it
	// (from the store, never from a person) only for a plan with an active mandate,
	// and the candidate then asks for the plan's execution by the dca.execute
	// workflow instead of being logged.
	MandateID string `json:"mandate_id,omitempty"`
}

// DCADueParamsFor copies the facts of a plan a spec needs.
func DCADueParamsFor(p perception.DCAPlan) DCADueParams {
	return DCADueParams{
		PlanID: p.ID, Provider: p.Provider, Account: p.Account, Symbol: p.Symbol,
		Side: p.Side, Amount: p.Amount, AmountUnit: p.AmountUnit, Gate: p.Gate,
	}
}

// JSON encodes the parameters for a Spec.
func (p DCADueParams) JSON() json.RawMessage {
	b, _ := json.Marshal(p) // plain fields: cannot fail unless Amount is not finite, which Validate refuses
	return b
}

var dcaAliasRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,31}$`)
var dcaVarRE = regexp.MustCompile(`^[A-Za-z0-9_.]{1,48}$`)

// Validate checks the parameters.
func (p DCADueParams) Validate() error {
	switch {
	case p.PlanID <= 0:
		return fmt.Errorf("plan_id must be a positive number")
	case !validAlertToken(p.Provider, false):
		return fmt.Errorf("provider must be 1-%d letters, digits, '_', '-', '.' or '@'", maxAlertTokenLen)
	case !validAlertToken(p.Account, true):
		return fmt.Errorf("account must be at most %d letters, digits, '_', '-', '.' or '@'", maxAlertTokenLen)
	case !validAlertSymbol(p.Symbol):
		return fmt.Errorf("symbol must be 1-%d letters, digits or '/', ':', '_', '-', '.'", maxAlertTokenLen)
	case p.Side != "buy" && p.Side != "sell":
		return fmt.Errorf("side must be buy or sell")
	case p.AmountUnit != "quote" && p.AmountUnit != "base":
		return fmt.Errorf("amount_unit must be quote or base")
	case !finite(p.Amount) || p.Amount <= 0:
		return fmt.Errorf("amount must be a positive number")
	case p.MandateID != "" && !validAlertToken(p.MandateID, false):
		return fmt.Errorf("mandate_id is malformed")
	}
	if g := p.Gate; g != nil {
		switch {
		case !validTimeframe(g.Timeframe):
			return fmt.Errorf("gate timeframe must look like 1m, 15m, 1h, 4h, 1d or 1w")
		case g.Lookback < 0 || g.Lookback > 100000:
			return fmt.Errorf("gate lookback is out of range")
		case utf8.RuneCountInString(g.Expression) > MaxDCAGateExpressionRunes:
			return fmt.Errorf("gate expression is over %d characters", MaxDCAGateExpressionRunes)
		case len(g.Indicators) > MaxDCAGateIndicators:
			return fmt.Errorf("gate has more than %d indicators", MaxDCAGateIndicators)
		}
		for _, ind := range g.Indicators {
			if !dcaAliasRE.MatchString(ind.Alias) || !dcaAliasRE.MatchString(ind.Kind) || len(ind.Params) > 8 {
				return fmt.Errorf("gate indicator is malformed")
			}
			for k, v := range ind.Params {
				if !dcaAliasRE.MatchString(k) || !finite(v) {
					return fmt.Errorf("gate indicator parameter is malformed")
				}
			}
		}
	}
	return nil
}

// gateBars is how many closes the gate wants: what the legacy gate fetches (200 when
// unset, 30 to 1000), at most what the market source carries.
func gateBars(g *perception.DCAGate) int {
	n := 0
	if g != nil {
		n = g.Lookback
	}
	switch {
	case n <= 0:
		n = 200
	case n < 30:
		n = 30
	case n > 1000:
		n = 1000
	}
	return min(n, MaxDCAGateBars)
}

// DCADueFactory builds dca_due triggers.
func DCADueFactory(cfg DCAConfig) Factory {
	if cfg.Stats == nil {
		cfg.Stats = &DCAStats{}
	}
	return func(spec Spec) (Trigger, error) {
		var p DCADueParams
		if err := decodeStrict(spec.Params, &p); err != nil {
			return nil, err
		}
		if err := p.Validate(); err != nil {
			return nil, err
		}
		return &dcaDue{id: spec.Kind + ":" + spec.ID, p: p, cfg: cfg}, nil
	}
}

// dcaDue judges one DCA plan's due slots.
//
// SHADOW unless the plan has a mandate, enforced here and not by configuration: for a
// plan whose params carry no MandateID every candidate it returns has Shadow set, so
// attention logs it and never delivers it, whatever the global mode, the spec's mode
// or the attention mode say. Nothing in this package can place or simulate an order,
// call a tool or fetch anything (the import boundary pins it); for such a plan the
// legacy "dca:" cron job remains the only thing that buys or sells, and the candidate
// is what the legacy path would do, recorded next to what it did so that the two can
// be compared (see CompareDCA).
//
// For a plan WITH a mandate the candidate is live (Evidence["action"] = "execute_dca",
// with the plan, slot and mandate ids) and is still only a candidate: the gateway's
// dispatcher hands it to the dca.execute workflow, which checks the mandate and every
// order gate in code. The registry still marks it shadow unless the spec and the
// triggers mode are live.
//
// It reads two kinds of event:
//
//   - perception.plan.dca_due, folded into the world model as the state of a "dca"
//     plan. The numbers of the slot are in that plan's Health (perception.DCAHealth*):
//     the slot, its lateness, whether the plan is enabled, its end date, its cap and the
//     executions counted against the cap. The trigger re-checks them at evaluation time
//     (the end date against the view's clock), then judges the gate and decides.
//   - perception.market.candle_closed of the gate's series (declared as a demand), whose
//     closes it keeps in its state. The gate is judged only on those: nothing is
//     fetched, and with no data, or data older than a bar should have replaced, the
//     slot makes no candidate and is counted as gate_unavailable. The closes are of
//     CLOSED bars; the legacy gate reads the bar still forming as its last, so the two
//     can disagree on a slot where the forming bar decides, and that is a known
//     difference, not a bug.
//
// Typed text never reaches a candidate: the plan's name is not in the spec at all, the
// gate's expression and indicator names are used only to judge the gate, and the
// title, subject, detail and dedup key are built from the plan's ID, symbol, provider,
// side and numbers.
type dcaDue struct {
	id  string
	p   DCADueParams
	cfg DCAConfig
}

func (t *dcaDue) ID() string { return t.id }

func (t *dcaDue) Kinds() []events.Kind {
	if t.hasGate() {
		return []events.Kind{events.KindPerceptionPlanDCADue, events.KindPerceptionMarketCandleClosed}
	}
	return []events.Kind{events.KindPerceptionPlanDCADue}
}

func (t *dcaDue) hasGate() bool { return t.p.Gate != nil && !t.p.Gate.Unsupported }

// Demands: the closed candles the gate reads, with the history it needs in every event.
func (t *dcaDue) Demands() []perception.Demand {
	if !t.hasGate() {
		return nil
	}
	return []perception.Demand{{
		Provider: t.p.Provider, Account: t.p.Account, Symbol: t.p.Symbol,
		Kind: perception.DemandOHLCV, Timeframe: t.p.Gate.Timeframe, Lookback: gateBars(t.p.Gate),
	}}
}

// dcaState is what the trigger keeps between events.
type dcaState struct {
	// LastDueMS is the newest slot judged: a slot is judged once, so that a replay
	// after a restart is not a second candidate.
	LastDueMS int64 `json:"last_due_ms,omitempty"`
	// Series holds the closes of the gate's series as the last candle event carried
	// them.
	Series *dcaSeries `json:"series,omitempty"`
}

type dcaSeries struct {
	BarOpenMS int64     `json:"bar_open_ms"`
	AtMS      int64     `json:"at_ms"`
	Closes    []float64 `json:"closes"`
}

func loadDCAState(st *State) dcaState {
	var s dcaState
	if len(st.Raw) > 0 {
		if err := json.Unmarshal(st.Raw, &s); err != nil {
			return dcaState{}
		}
	}
	return s
}

// save stores s into st when it differs from what st holds. The closes are cut from
// the old end until the state fits its budget.
func saveDCAState(st *State, s dcaState) {
	for {
		raw, err := json.Marshal(s)
		if err != nil {
			return
		}
		if len(raw) > dcaStateBudget && s.Series != nil && len(s.Series.Closes) > 1 {
			cut := *s.Series
			cut.Closes = cut.Closes[max(len(cut.Closes)/10, 1):]
			s.Series = &cut
			continue
		}
		if string(raw) != string(st.Raw) {
			st.Raw, st.Dirty = raw, true
		}
		return
	}
}

// Evaluate implements Trigger. It never returns an error for what an event contains:
// the registry switches a spec off after ten errors in a row, and an event must not be
// able to do that to a plan's trigger.
func (t *dcaDue) Evaluate(_ context.Context, v worldmodel.View, d worldmodel.Diff, st *State) ([]proactive.Candidate, error) {
	switch d.Event.Kind {
	case events.KindPerceptionMarketCandleClosed:
		t.keepCloses(v, d, st)
		return nil, nil
	case events.KindPerceptionPlanDCADue:
		return t.judge(v, d, st), nil
	}
	return nil, nil
}

// keepCloses stores the closes of the gate's series from a candle_closed event.
func (t *dcaDue) keepCloses(v worldmodel.View, d worldmodel.Diff, st *State) {
	cp, ok := d.Payload.(perception.CandlePayload)
	if !ok || !t.hasGate() {
		return // not a candle payload: not mine
	}
	c := cp.CandleClose()
	if c.Provider != t.p.Provider || c.Symbol != t.p.Symbol || c.Timeframe != t.p.Gate.Timeframe {
		return
	}
	if d.Event.TrustLevel() < proactive.TrustExchange || !v.Fresh(c.At, candleMaxAge) || len(c.Closes) == 0 {
		t.cfg.Stats.ClosesRejected.Add(1)
		return
	}
	closes := c.Closes[max(len(c.Closes)-gateBars(t.p.Gate), 0):]
	for _, x := range closes {
		if !finite(x) || x <= 0 {
			t.cfg.Stats.ClosesRejected.Add(1)
			return
		}
	}
	s := loadDCAState(st)
	s.Series = &dcaSeries{BarOpenMS: c.BarOpen.UnixMilli(), AtMS: c.At.UnixMilli(), Closes: append([]float64(nil), closes...)}
	saveDCAState(st, s)
	t.cfg.Stats.ClosesStored.Add(1)
}

// judge decides one due slot.
func (t *dcaDue) judge(v worldmodel.View, d worldmodel.Diff, st *State) []proactive.Candidate {
	pv, ok := v.Plan(perception.DCAPlanKind, t.p.PlanID)
	if !ok || !pv.At.Equal(d.At) {
		// Another plan's event (the registry routes every dca_due event to every dca_due
		// trigger), or a fact the model did not take from this event.
		return nil
	}
	dueMS, ok := pv.Health[perception.DCAHealthDueMS]
	if !ok || !finite(dueMS) || dueMS <= 0 || dueMS >= 1<<53 {
		return nil // not an event of this source
	}
	slot := int64(dueMS)
	s := loadDCAState(st)
	if slot <= s.LastDueMS {
		t.cfg.Stats.Duplicates.Add(1)
		return nil
	}
	s.LastDueMS = slot
	saveDCAState(st, s)
	t.cfg.Stats.Slots.Add(1)

	due := time.UnixMilli(slot)
	late := time.Duration(pv.Health[perception.DCAHealthLateMS]) * time.Millisecond
	rec := func(outcome, reason string) {
		t.cfg.Stats.count(outcome)
		t.cfg.Outcomes.Record(DCAOutcome{PlanID: t.p.PlanID, DueAt: due, Outcome: outcome, Reason: reason, Late: late > 0})
	}

	h := pv.Health
	switch {
	case !v.Fresh(pv.At, dcaDueMaxAge) || d.Event.TrustLevel() < proactive.TrustSystem:
		rec(DCAStaleEvent, "")
		return nil
	case h[perception.DCAHealthEnabled] != 1:
		rec(DCADisabled, "")
		return nil
	case h[perception.DCAHealthEndMS] > 0 && v.Now().UnixMilli() > int64(h[perception.DCAHealthEndMS]):
		rec(DCAExpired, "")
		return nil
	}
	capN, count := int(h[perception.DCAHealthPeriodCap]), 0
	if capN > 0 {
		cnt, has := h[perception.DCAHealthPeriodCount]
		switch {
		case !has || cnt < 0:
			rec(DCACapUnknown, "")
			return nil
		case int(cnt) >= capN:
			rec(DCACapReached, "")
			return nil
		}
		count = int(cnt)
	}

	trust := d.Event.TrustLevel()
	var gate *dcaGateFacts
	if g := t.p.Gate; g != nil {
		res, reason := t.judgeGate(v, s)
		switch {
		case reason != "":
			rec(DCAGateUnavailable, reason)
			return nil
		case res.Err != nil:
			rec(DCAGateError, "")
			return nil
		case !res.Met:
			rec(DCAGateFalse, "")
			return nil
		}
		gate = &dcaGateFacts{timeframe: g.Timeframe, bars: res.Bars, vars: cleanGateVars(res.Vars)}
		trust = proactive.MinTrust(trust, proactive.TrustExchange) // the closes came from an exchange
	}

	rec(DCAWouldExecute, "")
	return []proactive.Candidate{t.candidate(v, due, late, count, capN, gate, trust)}
}

// judgeGate runs the plan's gate on the stored closes. reason is non-empty when it
// cannot be judged: no closes yet, closes older than a bar should have replaced, a gate
// that needs more than closes, or no evaluator.
func (t *dcaDue) judgeGate(v worldmodel.View, s dcaState) (res DCAGateResult, reason string) {
	g := t.p.Gate
	switch {
	case g.Unsupported:
		return res, "unsupported"
	case t.cfg.Gate == nil:
		return res, "no_evaluator"
	case s.Series == nil || len(s.Series.Closes) == 0:
		return res, "no_data"
	}
	tf := tfDuration(g.Timeframe)
	lastClose := time.UnixMilli(s.Series.BarOpenMS).Add(tf)
	if v.Now().After(lastClose.Add(tf).Add(dcaGateSlack)) {
		return res, "stale_data"
	}
	res = t.cfg.Gate(*g, s.Series.Closes)
	if res.Unavailable != "" {
		return res, res.Unavailable
	}
	return res, ""
}

type dcaGateFacts struct {
	timeframe string
	bars      int
	vars      map[string]float64
}

// cleanGateVars keeps the gate's numbers that are finite and named plainly, at most
// MaxDCAGateVars of them in name order. The names come from the plan (typed text), so
// only identifier-shaped ones pass.
func cleanGateVars(in map[string]float64) map[string]float64 {
	if len(in) == 0 {
		return nil
	}
	names := make([]string, 0, len(in))
	for k, x := range in {
		if dcaVarRE.MatchString(k) && finite(x) {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	if len(names) > MaxDCAGateVars {
		names = names[:MaxDCAGateVars]
	}
	out := make(map[string]float64, len(names))
	for _, k := range names {
		out[k] = in[k]
	}
	return out
}

// candidate builds the shadow candidate for a slot that passed every check. Its title,
// subject, detail and dedup key are made from the plan's ID, symbol, provider, side and
// numbers; nothing a person typed is in it.
func (t *dcaDue) candidate(v worldmodel.View, due time.Time, late time.Duration, count, capN int, g *dcaGateFacts,
	trust proactive.Trust) proactive.Candidate {
	thai := t.cfg.isThai()
	where := t.p.Provider
	if t.p.Account != "" {
		where += "/" + t.p.Account
	}
	amount := fmtNumber(t.p.Amount)
	dueText := due.UTC().Format(time.RFC3339)

	lines := []string{dcaLine(thai, where, t.p.Side, amount, t.p.AmountUnit)}
	if late > 0 {
		lines = append(lines, dcaDueLine(thai, dueText, int(late/time.Minute)))
	} else {
		lines = append(lines, dcaDueLine(thai, dueText, 0))
	}
	if g != nil {
		lines = append(lines, dcaGateLine(thai, g.timeframe, g.bars))
	}
	if capN > 0 {
		lines = append(lines, dcaCapLine(thai, count, capN))
	}
	if t.p.MandateID == "" {
		lines = append(lines, dcaShadowLine(thai))
	}

	ev := map[string]any{
		"action": "execute_dca", "shadow": t.p.MandateID == "", "mandate_id": t.p.MandateID,
		"plan_id": t.p.PlanID, "due_at_ms": due.UnixMilli(), "late_ms": late.Milliseconds(),
		"provider": t.p.Provider, "account": t.p.Account, "symbol": t.p.Symbol,
		"side": t.p.Side, "amount": t.p.Amount, "amount_unit": t.p.AmountUnit,
	}
	if capN > 0 {
		ev["period_cap"], ev["period_count"] = capN, count
	}
	if g != nil {
		gate := map[string]any{"timeframe": g.timeframe, "bars": g.bars, "met": true}
		if len(g.vars) > 0 {
			gate["vars"] = g.vars
		}
		ev["gate"] = gate
	}
	return proactive.Candidate{
		DedupKey:    DCADueKey(t.p.PlanID, due.UnixMilli()),
		Severity:    proactive.SeverityInfo,
		Salience:    0.2,
		Subject:     t.p.Symbol,
		Title:       t.title(thai),
		Detail:      strings.Join(lines, "\n"),
		Evidence:    ev,
		Trust:       trust,
		Compose:     proactive.ComposeTemplate,
		DedupWindow: dcaDedupWindow,
		At:          v.Now(),
		Shadow:      t.p.MandateID == "", // see the type doc
	}
}

func (t *dcaDue) title(thai bool) string {
	if t.p.MandateID != "" {
		if thai {
			return "แผน DCA " + strconv.FormatInt(t.p.PlanID, 10) + " ถึงกำหนด: " + dcaSide(true, t.p.Side) + " " + t.p.Symbol
		}
		return "DCA plan " + strconv.FormatInt(t.p.PlanID, 10) + " is due: " + t.p.Side + " " + t.p.Symbol
	}
	return dcaTitle(thai, t.p.PlanID, t.p.Side, t.p.Symbol)
}

func (c DCAConfig) isThai() bool { return CrossConfig{Language: c.Language}.isThai() }

func dcaSide(thai bool, side string) string {
	switch {
	case thai && side == "sell":
		return "ขาย"
	case thai:
		return "ซื้อ"
	}
	return side
}

func dcaTitle(thai bool, id int64, side, symbol string) string {
	if thai {
		return "แผน DCA " + strconv.FormatInt(id, 10) + " ถึงกำหนด: " + dcaSide(true, side) + " " + symbol + " (โหมดเงา ไม่ได้ส่งคำสั่ง)"
	}
	return "DCA plan " + strconv.FormatInt(id, 10) + " is due: " + side + " " + symbol + " (shadow, no order placed)"
}

func dcaLine(thai bool, where, side, amount, unit string) string {
	if thai {
		return where + " · " + dcaSide(true, side) + " " + amount + " (" + unit + ")"
	}
	return where + " · " + side + " " + amount + " (" + unit + ")"
}

func dcaDueLine(thai bool, due string, lateMin int) string {
	switch {
	case thai && lateMin > 0:
		return "กำหนด " + due + " (ช้า " + strconv.Itoa(lateMin) + " นาที)"
	case thai:
		return "กำหนด " + due
	case lateMin > 0:
		return "due " + due + " (late " + strconv.Itoa(lateMin) + " min)"
	}
	return "due " + due
}

func dcaGateLine(thai bool, tf string, bars int) string {
	if thai {
		return "เงื่อนไข " + tf + ": ผ่าน (" + strconv.Itoa(bars) + " แท่ง ปิดแล้ว)"
	}
	return "gate " + tf + ": passed (" + strconv.Itoa(bars) + " closed bars)"
}

func dcaCapLine(thai bool, used, capN int) string {
	if thai {
		return "โควตาต่อรอบ " + strconv.Itoa(used) + "/" + strconv.Itoa(capN)
	}
	return "period cap " + strconv.Itoa(used) + "/" + strconv.Itoa(capN)
}

func dcaShadowLine(thai bool) string {
	if thai {
		return "โหมดเงา: งาน dca: เดิมยังเป็นผู้ส่งคำสั่ง รายการนี้ไม่ได้ส่งคำสั่ง"
	}
	return "Shadow: the existing dca: job still places the order; this places none."
}

// tfDuration is the length of a candle size accepted by validTimeframe.
func tfDuration(tf string) time.Duration {
	if len(tf) < 2 {
		return time.Hour
	}
	n, err := strconv.Atoi(tf[:len(tf)-1])
	if err != nil || n <= 0 {
		return time.Hour
	}
	switch tf[len(tf)-1] {
	case 'm':
		return time.Duration(n) * time.Minute
	case 'h':
		return time.Duration(n) * time.Hour
	case 'd':
		return time.Duration(n) * 24 * time.Hour
	case 'w':
		return time.Duration(n) * 7 * 24 * time.Hour
	}
	return time.Hour
}
