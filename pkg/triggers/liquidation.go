package triggers

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/worldmodel"
)

const (
	// maxLiqAccounts and maxLiqPositions bound what one evaluation walks and what the
	// state keeps.
	maxLiqAccounts  = 64
	maxLiqPositions = 120
	// marginRatioPlausibleMax: the world model reads the exchange's margin ratio as the
	// fraction of the margin that is used (0.8 is 80%). A reading above 1 is past
	// liquidation by that definition, and is far more likely another venue's convention
	// (a coverage ratio, where larger is safer) than a position that is already gone.
	// Treating it as "80% used" on a first reading would raise a permanent false
	// Critical, so on ENTRY it is refused and counted; the distance to the liquidation
	// price, which does not depend on the convention, still judges the position. A
	// position that is ALREADY Critical stays Critical on such a reading (shown as
	// "over 100%"): an alert must not go quiet at the moment the margin is gone.
	marginRatioPlausibleMax = 1.0

	liqTierNone     = 0
	liqTierWarning  = 1
	liqTierCritical = 2
)

// liqState is what liquidation_distance keeps: the tier each position is in, for the
// hysteresis, and when a Warning was last said. It keeps nothing about repeats of a
// Critical: a Critical is emitted on every sample of the position while it holds, and
// attention's protective repeat window decides how often the owner hears it.
type liqState struct {
	P map[string]liqPos `json:"p,omitempty"`
}

type liqPos struct {
	Tier int   `json:"t"`
	W    int64 `json:"w,omitempty"` // last Warning said, unix ms
}

type liquidation struct {
	id  string
	p   LiquidationParams
	cfg PortfolioConfig
}

func liquidationFactory(cfg PortfolioConfig) Factory {
	return func(spec Spec) (Trigger, error) {
		p, err := decodeParams(spec.Params, DefaultLiquidationParams())
		if err != nil {
			return nil, err
		}
		return &liquidation{id: spec.Kind + ":" + spec.ID, p: p, cfg: cfg}, nil
	}
}

func (t *liquidation) ID() string { return t.id }

func (t *liquidation) Kinds() []events.Kind {
	return []events.Kind{events.KindPerceptionFuturesPositions}
}

// Demands: none. The futures source reads every configured account on its own.
func (t *liquidation) Demands() []perception.Demand { return nil }

// distancePct is how far the mark is from the liquidation price, in percent of the
// mark, on the side the position can be liquidated: a long is liquidated below the
// mark, a short above it. It is negative when the mark is already past the
// liquidation price. mark and liq must be positive and finite, side long or short.
func distancePct(side string, mark, liq float64) float64 {
	if side == "short" {
		return (liq - mark) / mark * 100
	}
	return (mark - liq) / mark * 100
}

// liqTier applies the tiers with their hysteresis to a position's readings. Tiers
// escalate at once and step down only after the reading has recovered past the
// boundary by the hysteresis, so a position hovering at 15% does not flap.
func (p LiquidationParams) liqTier(prev int, distKnown bool, dist float64, marginKnown bool, margin float64) int {
	h := p.HysteresisPct
	critEnter := distKnown && dist < p.CriticalPct || marginKnown && margin > p.MarginRatioCriticalPct
	critStay := prev == liqTierCritical &&
		(distKnown && dist < p.CriticalPct+h || marginKnown && margin > p.MarginRatioCriticalPct-h)
	switch {
	case critEnter || critStay:
		return liqTierCritical
	case distKnown && dist < p.WarningPct,
		prev >= liqTierWarning && distKnown && dist < p.WarningPct+h:
		return liqTierWarning
	}
	return liqTierNone
}

// Evaluate judges the positions of the account THIS event brought.
//
// A tracked event's Diff does not name its account, so the gateway injects the
// configured futures accounts and the trigger takes the one whose positions carry the
// diff's time (a sample restored from a checkpoint keeps its old stamp, so it is never
// the event's). Every number comes from the view and is tested before it is used:
//
//   - the sample is no older than max_age_minutes, the account is not marked
//     unreadable, the trust is at least the exchange's, and the side is known;
//   - the distance needs a mark price and a liquidation price the exchange reported
//     (a missing or zero liquidation price is "unknown", never "far away");
//   - the margin ratio needs a plausible reported value.
//
// An input that fails changes nothing and fires nothing; it is counted, and the
// position's tier is kept as it was (a reading that cannot be trusted is no reason to
// stand down). The data_stale kind says when the source itself has gone quiet.
//
// A Critical (distance under critical_pct, or margin ratio over its line) is a
// protective template message, built here from numbers and names. It is emitted on
// every sample while it holds, with a stable key. The notional shown is
// contracts x contract size x mark, never contracts x mark.
func (t *liquidation) Evaluate(_ context.Context, v worldmodel.View, d worldmodel.Diff, st *State) ([]proactive.Candidate, error) {
	if t.cfg.FuturesAccounts == nil {
		return nil, nil
	}
	accounts := slices.Clone(t.cfg.FuturesAccounts())
	slices.Sort(accounts)
	accounts = slices.Compact(accounts)
	if len(accounts) > maxLiqAccounts {
		accounts = accounts[:maxLiqAccounts]
	}
	var s liqState
	if len(st.Raw) > 0 {
		if json.Unmarshal(st.Raw, &s) != nil {
			s = liqState{}
		}
	}
	if s.P == nil {
		s.P = map[string]liqPos{}
	}
	before := len(s.P)
	var out []proactive.Candidate
	changed := false
	for _, acct := range accounts {
		ps, seen := v.FuturesPositions(acct)
		if !seen {
			continue
		}
		prefix := acct + "|"
		if len(ps) == 0 {
			// The latest sample of the account is flat: its tiers are over.
			for k := range s.P {
				if strings.HasPrefix(k, prefix) {
					delete(s.P, k)
					changed = true
				}
			}
			continue
		}
		if !ps[0].At.Equal(d.At) {
			continue // not this event's account
		}
		live := map[string]bool{}
		for _, p := range ps {
			key := prefix + p.Symbol + "|" + p.Side
			live[key] = true
			cur, had := s.P[key]
			c, next, ok := t.judge(v, d, acct, p, cur)
			if !ok {
				continue
			}
			if next.Tier == liqTierNone {
				if had {
					delete(s.P, key)
					changed = true
				}
			} else if len(s.P) < maxLiqPositions || had {
				if !had || next != cur {
					changed = true
				}
				s.P[key] = next
			}
			if c != nil {
				out = append(out, *c)
			}
		}
		for k := range s.P {
			if strings.HasPrefix(k, prefix) && !live[k] {
				delete(s.P, k) // the position is gone
				changed = true
			}
		}
	}
	if changed || len(s.P) != before {
		marshalState(st, s)
	}
	return out, nil
}

// judge applies the gates and the tiers to one position. ok is false when the
// position could not be judged at all (nothing may change for it).
func (t *liquidation) judge(v worldmodel.View, d worldmodel.Diff, acct string, p worldmodel.FuturesView, cur liqPos) (*proactive.Candidate, liqPos, bool) {
	stats := t.cfg.stats()
	maxAge := time.Duration(t.p.MaxAgeMinutes) * time.Minute
	switch {
	case !v.Fresh(p.At, maxAge):
		stats.LiqStale.Add(1)
		return nil, cur, false
	case p.AccountDegraded:
		stats.LiqDegraded.Add(1)
		return nil, cur, false
	case p.Trust < proactive.TrustExchange:
		stats.LiqUntrusted.Add(1)
		return nil, cur, false
	case !finite(p.Contracts) || p.Contracts <= 0:
		return nil, cur, false
	}
	stats.LiqEvaluated.Add(1)

	// The side is needed for the distance only: a position whose side the exchange did
	// not say is still judged on its margin ratio.
	sideKnown := p.Side == "long" || p.Side == "short"
	distKnown := false
	var dist float64
	switch {
	case !sideKnown:
		stats.LiqNoSide.Add(1)
	case !p.MarkPriceKnown || !finite(p.MarkPrice) || p.MarkPrice <= 0:
		stats.LiqNoMark.Add(1)
	case !p.LiqPriceKnown || !finite(p.LiqPrice) || p.LiqPrice <= 0:
		stats.LiqNoLiqPrice.Add(1)
	default:
		dist, distKnown = distancePct(p.Side, p.MarkPrice, p.LiqPrice), true
		if !finite(dist) {
			distKnown = false
		}
	}
	marginKnown := false
	var margin float64
	if p.MarginRatioKnown {
		switch r := p.MarginRatio; {
		case !finite(r) || r < 0:
			stats.LiqBadMargin.Add(1)
		case r > marginRatioPlausibleMax && cur.Tier == liqTierCritical:
			// A position that is already Critical stays Critical on a reading past 100%:
			// the alert must not go quiet at the moment the margin is gone. The reading
			// is not believed to the digit (see marginRatioPlausibleMax) and is shown as
			// "over 100%".
			margin, marginKnown = r*100, true
		case r > marginRatioPlausibleMax:
			stats.LiqBadMargin.Add(1) // a first reading past 100% is not taken as Critical on its own
		default:
			margin, marginKnown = r*100, true
		}
	}
	if !distKnown && !marginKnown {
		return nil, cur, false // nothing can be said; the tier stays as it was
	}
	tier := t.p.liqTier(cur.Tier, distKnown, dist, marginKnown, margin)
	next := liqPos{Tier: tier, W: cur.W}
	now := v.Now()
	switch tier {
	case liqTierCritical:
		c := t.candidate(acct, p, tier, distKnown, dist, marginKnown, margin, MinInputTrust(d, p.Trust), now)
		stats.LiqFired.Add(1)
		return &c, next, true
	case liqTierWarning:
		repeat := time.Duration(t.p.WarningRepeatHours) * time.Hour
		if cur.Tier != liqTierWarning || cur.W == 0 || now.Sub(time.UnixMilli(cur.W)) >= repeat || now.Before(time.UnixMilli(cur.W)) {
			next.W = now.UnixMilli()
			c := t.candidate(acct, p, tier, distKnown, dist, marginKnown, margin, MinInputTrust(d, p.Trust), now)
			stats.LiqFired.Add(1)
			return &c, next, true
		}
	default:
		next.W = 0
	}
	return nil, next, true
}

func (t *liquidation) candidate(acct string, p worldmodel.FuturesView, tier int, distKnown bool, dist float64,
	marginKnown bool, margin float64, trust proactive.Trust, at time.Time) proactive.Candidate {
	thai := t.cfg.isThai()
	name := "warning"
	if tier == liqTierCritical {
		name = "critical"
	}
	ev := map[string]any{
		"account": acct, "symbol": p.Symbol, "side": sideLabel(p), "tier": name, "contracts": p.Contracts,
		"critical_pct": t.p.CriticalPct, "warning_pct": t.p.WarningPct,
	}
	if b := baseAssetOf(p.Symbol); b != "" {
		ev["base_asset"] = b
	}
	if distKnown {
		ev["distance_pct"] = dist
		ev["mark_price"] = p.MarkPrice
		ev["liq_price"] = p.LiqPrice
	}
	if marginKnown {
		if margin > 100 {
			ev["margin_ratio_over_100"] = true
		} else {
			ev["margin_ratio_pct"] = margin
		}
	}
	if p.ContractSizeKnown {
		ev["contract_size"] = p.ContractSize
	}
	if p.NotionalKnown {
		ev["notional_usd"] = p.NotionalUSD
	}
	if p.UnrealizedKnown {
		ev["unrealized_pnl"] = p.UnrealizedPnl
	}
	c := proactive.Candidate{
		DedupKey:    LiquidationKey(acct, p.Symbol, sideLabel(p), name),
		Subject:     p.Symbol,
		Title:       liqTitle(thai, acct, p, tier, distKnown, dist, marginKnown, margin, t.p.MarginRatioCriticalPct),
		Detail:      liqDetail(thai, p, distKnown, dist, marginKnown, margin),
		Evidence:    ev,
		Trust:       trust,
		DedupWindow: time.Duration(t.p.WarningRepeatHours) * time.Hour,
		At:          at,
	}
	if tier == liqTierCritical {
		// A protective alert is a template message built here, never held for a
		// composer. Attention decides how often a persisting breach is repeated.
		c.Severity, c.Protective, c.Salience, c.Compose = proactive.SeverityCritical, true, 1, proactive.ComposeTemplate
	} else {
		c.Severity, c.Salience, c.Compose = proactive.SeverityWarning, 0.7, proactive.ComposeLLM
	}
	return c
}

func liqTitle(thai bool, acct string, p worldmodel.FuturesView, tier int, distKnown bool, dist float64,
	marginKnown bool, margin, marginLine float64) string {
	head := "Liquidation distance"
	if tier == liqTierCritical {
		head = "Liquidation risk"
	}
	if thai {
		head = "ระยะห่างจุดล้างพอร์ต"
		if tier == liqTierCritical {
			head = "เสี่ยงถูกล้างพอร์ต"
		}
	}
	who := p.Symbol + " " + sideLabel(p) + " (" + acct + ")"
	var parts []string
	if distKnown {
		if thai {
			parts = append(parts, "ห่างจุดล้างพอร์ต "+pct1(dist))
		} else {
			parts = append(parts, pct1(dist)+" from liquidation")
		}
	}
	if marginKnown && margin > marginLine {
		if thai {
			parts = append(parts, "อัตรามาร์จิน "+marginText(true, margin))
		} else {
			parts = append(parts, "margin ratio "+marginText(false, margin))
		}
	}
	return head + ": " + who + " — " + strings.Join(parts, ", ")
}

// liqDetail is the evidence block of the message: every figure, in at most four lines,
// made from numbers only. A Critical is delivered as it is, so it has to be complete.
func liqDetail(thai bool, p worldmodel.FuturesView, distKnown bool, dist float64, marginKnown bool, margin float64) string {
	var lines []string
	if distKnown {
		if thai {
			lines = append(lines, "ราคามาร์ก "+fmtNumber(p.MarkPrice)+" · ราคาล้างพอร์ต "+fmtNumber(p.LiqPrice)+" · ห่าง "+pct1(dist))
		} else {
			lines = append(lines, "Mark "+fmtNumber(p.MarkPrice)+" · liquidation "+fmtNumber(p.LiqPrice)+" · distance "+pct1(dist))
		}
	}
	size := ""
	if p.ContractSizeKnown {
		size = " × " + fmtNumber(p.ContractSize)
	}
	margins := ""
	if marginKnown {
		if thai {
			margins = " · อัตรามาร์จิน " + marginText(true, margin)
		} else {
			margins = " · margin ratio " + marginText(false, margin)
		}
	}
	if thai {
		lines = append(lines, "สัญญา "+fmtNumber(p.Contracts)+size+" · มูลค่า "+notionalText(thai, p)+margins)
	} else {
		lines = append(lines, "Contracts "+fmtNumber(p.Contracts)+size+" · notional "+notionalText(thai, p)+margins)
	}
	if p.UnrealizedKnown {
		if thai {
			lines = append(lines, "กำไร/ขาดทุนที่ยังไม่รับรู้ "+fmtNumber(p.UnrealizedPnl))
		} else {
			lines = append(lines, "Unrealized P&L "+fmtNumber(p.UnrealizedPnl))
		}
	}
	return strings.Join(lines, "\n")
}

// notionalText is the position's value: the model's USD figure when it is known,
// otherwise contracts x contract size x mark labelled with the contract's quote
// currency (which is not necessarily USD), otherwise unknown. Contracts x mark alone
// is never used: a contract is not one unit of the base currency.
func notionalText(thai bool, p worldmodel.FuturesView) string {
	switch {
	case p.NotionalKnown && finite(p.NotionalUSD):
		return usd(p.NotionalUSD)
	case p.ContractSizeKnown && p.MarkPriceKnown && p.ContractSize > 0 && p.MarkPrice > 0:
		q := quoteOf(p.Symbol)
		n := p.Contracts * p.ContractSize * p.MarkPrice
		if !finite(n) {
			break
		}
		if thai {
			return fmtNumber(n) + " " + q + " (ไม่ได้แปลงเป็น USD)"
		}
		return fmtNumber(n) + " " + q + " (not converted to USD)"
	}
	if thai {
		return "ไม่ทราบ"
	}
	return "unknown"
}

// quoteOf is the quote currency of a symbol ("BTC/USDT:USDT" gives "USDT"), or the
// word "quote" when it has none.
func quoteOf(symbol string) string {
	_, rest, ok := strings.Cut(symbol, "/")
	if !ok {
		return "quote"
	}
	q, _, _ := strings.Cut(rest, ":")
	if q == "" {
		return "quote"
	}
	return strings.ToUpper(q)
}

// sideLabel is the side of a position, or "position" when the exchange did not say.
func sideLabel(p worldmodel.FuturesView) string {
	if p.Side == "long" || p.Side == "short" {
		return p.Side
	}
	return "position"
}

// marginText renders a margin ratio in percent; a reading past 100% is shown as
// "over 100%" and not as a figure the trigger does not trust.
func marginText(thai bool, margin float64) string {
	if margin > 100 {
		if thai {
			return "เกิน 100%"
		}
		return "over 100%"
	}
	return pct1(margin)
}
