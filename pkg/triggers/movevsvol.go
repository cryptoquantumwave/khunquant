package triggers

import (
	"context"
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/calendar"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/worldmodel"
)

const (
	// MoveMaxAssets bounds how many holdings are tracked: the largest by value. The
	// state keeps about 30 samples per asset and must fit MaxStateBytes.
	MoveMaxAssets = 12
	moveWindow    = 24 * time.Hour

	// A snapshot is added to an asset's samples at most every moveMinSpacing. Samples
	// younger than moveFineSpan are all kept; older ones are thinned to one per
	// moveCoarseSpacing, up to moveHistory old.
	moveMinSpacing    = 5 * time.Minute
	moveFineSpan      = 90 * time.Minute
	moveCoarseSpacing = 55 * time.Minute
	moveHistory       = 25 * time.Hour
	moveMaxSamples    = 48
	// sigmaPairMin and sigmaPairMax bound the gap between two samples that make an
	// hourly return for the volatility estimate.
	sigmaPairMin = 40 * time.Minute
	sigmaPairMax = 130 * time.Minute

	oneHourTolerance    = 10 * time.Minute
	oneHourToleranceMax = 30 * time.Minute
	dayTolerance        = 45 * time.Minute

	// moveProtectiveRank is the ledger rank of a protective move, above tier 3: a move
	// that was Warning-sized and later became protective-sized is still a new message.
	moveProtectiveRank = 4
)

type moveState struct {
	A map[string]moveAsset   `json:"a,omitempty"`
	L map[string]ledgerEntry `json:"l,omitempty"`
}

// moveAsset is one tracked holding: its class and its price samples, [unix minutes,
// price in USD] in time order. A price is the implied price of the holding in the
// snapshots (value over quantity), which needs no market read of its own.
type moveAsset struct {
	C string       `json:"c"`
	S [][2]float64 `json:"s"`
}

type moveVsVol struct {
	id  string
	p   MoveParams
	cfg PortfolioConfig
}

func moveFactory(cfg PortfolioConfig) Factory {
	return func(spec Spec) (Trigger, error) {
		p, err := decodeParams(spec.Params, DefaultMoveParams())
		if err != nil {
			return nil, err
		}
		return &moveVsVol{id: spec.Kind + ":" + spec.ID, p: p, cfg: cfg}, nil
	}
}

func (t *moveVsVol) ID() string { return t.id }

func (t *moveVsVol) Kinds() []events.Kind {
	return []events.Kind{events.KindPerceptionPortfolioSnapshot}
}

// Demands: none. The prices are the ones the portfolio snapshots imply, so the
// trigger asks the market for nothing and sees a move with the snapshot's delay.
func (t *moveVsVol) Demands() []perception.Demand { return nil }

type heldAsset struct {
	name               string
	class              string
	value              float64 // all venues
	bestValue, bestQty float64 // the largest position, which prices the asset
}

// holdings groups the snapshot's positions by asset. Cash and unclassified classes
// are not judged (no floor is defined for them).
func (t *moveVsVol) holdings(pf worldmodel.PortfolioView) []heldAsset {
	by := map[string]*heldAsset{}
	for _, q := range pf.Positions { // by value, largest first
		if !finite(q.Value) || !finite(q.Qty) || q.Value <= 0 || q.Qty <= 0 || q.Asset == "" {
			continue
		}
		switch q.Class {
		case string(perception.ClassCrypto), string(perception.ClassUSEquity), string(perception.ClassTHEquity):
		case string(perception.ClassCash):
			continue
		default:
			t.cfg.stats().MoveUnknownClass.Add(1)
			continue
		}
		h := by[q.Asset]
		if h == nil {
			h = &heldAsset{name: q.Asset, class: q.Class, bestValue: q.Value, bestQty: q.Qty}
			by[q.Asset] = h
		}
		h.value += q.Value
	}
	out := make([]heldAsset, 0, len(by))
	for _, h := range by {
		w := h.value / pf.TotalUSD * 100
		if w >= t.p.MinWeightPct || h.value >= t.p.MinValueUSD {
			out = append(out, *h)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].value != out[j].value {
			return out[i].value > out[j].value
		}
		return out[i].name < out[j].name
	})
	if len(out) > MoveMaxAssets {
		out = out[:MoveMaxAssets]
	}
	return out
}

func roundSig(v float64) float64 {
	f, _ := strconv.ParseFloat(strconv.FormatFloat(v, 'g', 6, 64), 64)
	return f
}

func minuteOf(t time.Time) float64 { return float64(t.Unix() / 60) }

func (s *moveAsset) at(i int) time.Time { return time.Unix(int64(s.S[i][0])*60, 0) }

// addSample adds (at, price) when it is later than the last sample by moveMinSpacing,
// then thins: young samples stay, older ones keep one per moveCoarseSpacing, and
// anything older than moveHistory goes.
func (a *moveAsset) addSample(at time.Time, price float64) {
	if n := len(a.S); n > 0 && at.Sub(a.at(n-1)) < moveMinSpacing {
		return
	}
	a.S = append(a.S, [2]float64{minuteOf(at), roundSig(price)})
	kept := a.S[:0:0]
	var lastOld time.Time
	haveOld := false
	for i := range a.S {
		ts := a.at(i)
		age := at.Sub(ts)
		switch {
		case age > moveHistory || age < 0:
			continue
		case age <= moveFineSpan:
		case haveOld && ts.Sub(lastOld) < moveCoarseSpacing:
			continue
		default:
			lastOld, haveOld = ts, true
		}
		kept = append(kept, a.S[i])
	}
	if len(kept) > moveMaxSamples {
		kept = kept[len(kept)-moveMaxSamples:]
	}
	a.S = kept
}

// reference is the sample nearest to at-window, within tol.
func (a *moveAsset) reference(at time.Time, window, tol time.Duration) (time.Time, float64, bool) {
	target := at.Add(-window)
	best, bestD := -1, tol+1
	for i := range a.S {
		d := a.at(i).Sub(target)
		if d < 0 {
			d = -d
		}
		if d < bestD {
			best, bestD = i, d
		}
	}
	if best < 0 || bestD > tol {
		return time.Time{}, 0, false
	}
	return a.at(best), a.S[best][1], true
}

// hourlySigma is the root mean square of the hourly log returns between the older
// samples (those past moveFineSpan, so the move being judged is not in its own
// volatility), each divided by the square root of its length in hours. ok needs at
// least min returns.
func (a *moveAsset) hourlySigma(at time.Time, min int) (sigma float64, ok bool) {
	var sum float64
	n := 0
	for i := 1; i < len(a.S); i++ {
		t1, t0 := a.at(i), a.at(i-1)
		if at.Sub(t1) <= moveFineSpan {
			break
		}
		gap := t1.Sub(t0)
		p0, p1 := a.S[i-1][1], a.S[i][1]
		if gap < sigmaPairMin || gap > sigmaPairMax || p0 <= 0 || p1 <= 0 {
			continue
		}
		r := math.Log(p1/p0) / math.Sqrt(gap.Hours())
		sum += r * r
		n++
	}
	if n < min || n == 0 {
		return 0, false
	}
	return math.Sqrt(sum / float64(n)), true
}

func (t *moveVsVol) floorPct(class string) float64 {
	switch class {
	case string(perception.ClassUSEquity):
		return t.p.FloorUSPct
	case string(perception.ClassTHEquity):
		return t.p.FloorTHPct
	}
	return t.p.FloorCryptoPct
}

// marketGate says whether an equity's market is open now or closed less than
// after_close_minutes ago. Crypto is always open.
func (t *moveVsVol) marketGate(class string, at time.Time) bool {
	var m calendar.Market
	switch class {
	case string(perception.ClassUSEquity):
		m = calendar.US
	case string(perception.ClassTHEquity):
		m = calendar.SET
	default:
		return true
	}
	return t.cfg.open(m, at) || t.cfg.open(m, at.Add(-time.Duration(t.p.AfterCloseMinutes)*time.Minute))
}

type moveHit struct {
	window  string
	span    time.Duration
	ret     float64 // simple return, fraction
	refTime time.Time
	refP    float64
	sigmaW  float64 // zero when unknown
	tier    int
}

func (t *moveVsVol) oneHourTol() time.Duration {
	tol := t.cfg.snapshotEvery()/2 + 2*time.Minute
	return min(max(tol, oneHourTolerance), oneHourToleranceMax)
}

// Evaluate judges every tracked holding on the snapshot THIS event brought.
//
// Tiers are 1x, 2x and 3x the class floor; a move needs the tier and also needs to
// exceed k times the asset's own volatility scaled to the window (the hourly volatility
// times the square root of the window in hours). Until the asset has min_sigma_returns
// hourly returns only the class floor applies (counted as floor-only). A window needs a
// reference sample close to its start; without one nothing is judged for it.
//
// Per asset the larger of the 1-hour and 24-hour tiers is judged, escalate-only inside
// 24 hours. A protective Critical is a template message, for a move of
// protective_move_pct or more in a holding of protective_weight_pct or more of the
// portfolio.
func (t *moveVsVol) Evaluate(_ context.Context, v worldmodel.View, d worldmodel.Diff, st *State) ([]proactive.Candidate, error) {
	pf, ok := t.cfg.snapshotOf(v, d)
	if !ok {
		return nil, nil
	}
	stats := t.cfg.stats()
	var s moveState
	if len(st.Raw) > 0 && json.Unmarshal(st.Raw, &s) != nil {
		s = moveState{}
	}
	if s.A == nil {
		s.A = map[string]moveAsset{}
	}
	if s.L == nil {
		s.L = map[string]ledgerEntry{}
	}
	prevRaw := string(st.Raw)
	at := pf.At
	held := t.holdings(pf)
	keep := map[string]bool{}
	var out []proactive.Candidate
	for _, h := range held {
		keep[h.name] = true
		price := h.bestValue / h.bestQty
		if !finite(price) || price <= 0 {
			continue
		}
		a := s.A[h.name]
		if a.C != h.class {
			a = moveAsset{C: h.class}
		}
		a.addSample(at, price)
		s.A[h.name] = a
		if !t.marketGate(h.class, at) {
			stats.MoveMarketClosed.Add(1)
			continue
		}
		hit, ok := t.best(&a, at, price, h.class)
		if !ok {
			continue
		}
		weight := h.value / pf.TotalUSD
		protective := hit.tier >= 3 && math.Abs(hit.ret)*100 >= t.p.ProtectiveMovePct && weight*100 >= t.p.ProtectiveWeightPct
		rank := hit.tier
		if protective {
			rank = moveProtectiveRank
		}
		if hit.tier == 0 || !s.L[h.name].escalates(rank, at, moveWindow) {
			continue
		}
		s.L[h.name] = ledgerEntry{Tier: rank, AtMS: at.UnixMilli()}
		out = append(out, t.candidate(h, hit, price, weight, protective, MinInputTrust(d, pf.Trust), v.Now()))
		stats.MoveFired.Add(1)
	}
	for k := range s.A {
		if !keep[k] {
			delete(s.A, k)
		}
	}
	pruneLedger(s.L, at, moveWindow)
	if len(s.L) == 0 {
		s.L = nil
	}
	if len(s.A) == 0 {
		s.A = nil
	}
	if raw, err := json.Marshal(s); err == nil && string(raw) != prevRaw {
		st.Raw, st.Dirty = raw, true
	}
	return out, nil
}

// best returns the hit of the window with the higher tier (the shorter one on a tie).
func (t *moveVsVol) best(a *moveAsset, at time.Time, price float64, class string) (moveHit, bool) {
	stats := t.cfg.stats()
	sigma, sigmaOK := a.hourlySigma(at, t.p.MinSigmaReturns)
	floor := t.floorPct(class)
	var best moveHit
	found := false
	for _, w := range []struct {
		name string
		span time.Duration
		tol  time.Duration
	}{{"1h", time.Hour, t.oneHourTol()}, {"24h", 24 * time.Hour, dayTolerance}} {
		refT, refP, ok := a.reference(at, w.span, w.tol)
		if !ok || refP <= 0 {
			stats.MoveNoReference.Add(1)
			continue
		}
		hit := moveHit{window: w.name, span: w.span, ret: price/refP - 1, refTime: refT, refP: refP}
		mag := math.Abs(hit.ret) * 100
		need := floor
		if sigmaOK {
			hit.sigmaW = sigma * math.Sqrt(at.Sub(refT).Hours())
			need = math.Max(floor, t.p.K*hit.sigmaW*100)
		} else {
			stats.MoveFloorOnly.Add(1)
		}
		if finite(mag) && mag >= need {
			hit.tier = min(int(mag/floor), 3)
		}
		if !found || hit.tier > best.tier {
			best, found = hit, true
		}
	}
	return best, found
}

func (t *moveVsVol) candidate(h heldAsset, hit moveHit, price, weight float64, protective bool, trust proactive.Trust, at time.Time) proactive.Candidate {
	thai := t.cfg.isThai()
	sign := "+"
	if hit.ret < 0 {
		sign = "-"
	}
	mv := sign + pct1(math.Abs(hit.ret)*100)
	asset := cleanKeyToken(h.name)
	var title string
	if thai {
		title = asset + " เคลื่อนไหว " + mv + " ใน " + hit.window + " (น้ำหนักในพอร์ต " + pct1(weight*100) + ")"
	} else {
		title = asset + " moved " + mv + " in " + hit.window + " (" + pct1(weight*100) + " of the portfolio)"
	}
	if hit.sigmaW > 0 {
		mult := math.Abs(hit.ret) / hit.sigmaW
		if finite(mult) {
			title += " · " + fmtNumber(math.Round(mult*10)/10) + "σ"
		}
	}
	detail := "Was " + fmtNumber(hit.refP) + " → now " + fmtNumber(price) + " USD over " + hit.window +
		" · value " + usd(h.value) + " · tier " + strconv.Itoa(hit.tier)
	if thai {
		detail = "จาก " + fmtNumber(hit.refP) + " → ตอนนี้ " + fmtNumber(price) + " USD ใน " + hit.window +
			" · มูลค่า " + usd(h.value) + " · ระดับ " + strconv.Itoa(hit.tier)
	}
	ev := map[string]any{
		"asset": asset, "class": h.class, "window": hit.window, "return_pct": hit.ret * 100, "tier": hit.tier,
		"weight_pct": weight * 100, "value_usd": h.value, "price_usd": price, "reference_usd": hit.refP,
		"reference_age_minutes": int(at.Sub(hit.refTime) / time.Minute), "floor_pct": t.floorPct(h.class), "k": t.p.K,
	}
	if b := baseAssetOf(h.name); b != "" {
		ev["base_asset"] = b
	}
	if hit.sigmaW > 0 {
		ev["sigma_window_pct"] = hit.sigmaW * 100
		ev["sigma_multiple"] = math.Abs(hit.ret) / hit.sigmaW
	}
	c := proactive.Candidate{
		DedupKey: MoveKey(asset, hit.window, hit.tier),
		Subject:  asset, Title: title, Detail: detail, Evidence: ev, Trust: trust,
		Salience: clamp01(weight * float64(hit.tier) / 3), DedupWindow: moveWindow, At: at,
	}
	switch {
	case protective:
		c.Severity, c.Protective, c.Compose, c.Salience = proactive.SeverityCritical, true, proactive.ComposeTemplate, math.Max(c.Salience, 0.9)
	case hit.tier >= 2:
		c.Severity = proactive.SeverityWarning
		if weight*100 >= t.p.ProtectiveWeightPct {
			c.Compose = proactive.ComposeLLM
		}
	default:
		c.Severity = proactive.SeverityNotice
	}
	return c
}
