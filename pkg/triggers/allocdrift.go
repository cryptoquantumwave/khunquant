package triggers

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/worldmodel"
)

const (
	maxDriftClasses = 20
	// targetSumSlack: targets may add up to a little over 100 for rounding.
	targetSumSlack = 0.5
)

type driftState struct {
	// C maps a class to the unix ms it was last said while it stayed off target.
	C map[string]int64 `json:"c,omitempty"`
}

type allocDrift struct {
	id  string
	p   DriftParams
	cfg PortfolioConfig
}

func driftFactory(cfg PortfolioConfig) Factory {
	return func(spec Spec) (Trigger, error) {
		p, err := decodeParams(spec.Params, DefaultDriftParams())
		if err != nil {
			return nil, err
		}
		return &allocDrift{id: spec.Kind + ":" + spec.ID, p: p, cfg: cfg}, nil
	}
}

func (t *allocDrift) ID() string { return t.id }

func (t *allocDrift) Kinds() []events.Kind {
	return []events.Kind{events.KindPerceptionPortfolioSnapshot}
}

func (t *allocDrift) Demands() []perception.Demand { return nil }

// sanitizeTargets keeps the targets that can be judged against: finite, from 0 to 100,
// at most maxDriftClasses of them, adding up to no more than 100 (plus rounding
// slack). A set that does not add up is refused whole: a target of 60 and one of 70
// cannot both be right, and judging against either would invent a drift.
func sanitizeTargets(in map[string]float64) (map[string]float64, bool) {
	if len(in) == 0 || len(in) > maxDriftClasses {
		return nil, false
	}
	var sum float64
	out := make(map[string]float64, len(in))
	for k, v := range in {
		if k == "" || !finite(v) || v < 0 || v > 100 {
			return nil, false
		}
		out[k] = v
		sum += v
	}
	if sum <= 0 || sum > 100+targetSumSlack {
		return nil, false
	}
	return out, true
}

// Evaluate compares the weight of each asset class in the snapshot THIS event brought
// with the owner's target for it, and says a class that is off by threshold_pp or more.
// It is Info (the digest) and says each class once per repeat_days while it stays off;
// a class is back on target once the gap is hysteresis_pp under the threshold.
//
// Weights are over the valuation, which excludes the assets that could not be priced;
// the message says how many were left out. Targets come from the owner's profile
// through the gateway; none, or a set that does not add up, judges nothing.
func (t *allocDrift) Evaluate(_ context.Context, v worldmodel.View, d worldmodel.Diff, st *State) ([]proactive.Candidate, error) {
	stats := t.cfg.stats()
	if t.cfg.Targets == nil {
		stats.DriftNoTargets.Add(1)
		return nil, nil
	}
	raw := t.cfg.Targets()
	if len(raw) == 0 {
		stats.DriftNoTargets.Add(1)
		return nil, nil
	}
	targets, ok := sanitizeTargets(raw)
	if !ok {
		stats.DriftBadTargets.Add(1)
		return nil, nil
	}
	pf, ok := t.cfg.snapshotOf(v, d)
	if !ok {
		return nil, nil
	}
	if pf.TotalUSD < t.p.MinTotalUSD {
		return nil, nil
	}
	var s driftState
	if len(st.Raw) > 0 && json.Unmarshal(st.Raw, &s) != nil {
		s = driftState{}
	}
	if s.C == nil {
		s.C = map[string]int64{}
	}
	prev := fmt.Sprint(s.C)
	now := v.Now()
	repeat := time.Duration(t.p.RepeatDays) * 24 * time.Hour
	var out []proactive.Candidate
	for _, class := range sortedKeys(targets) {
		weight := pf.ByClass[class] / pf.TotalUSD * 100
		gap := weight - targets[class]
		if !finite(gap) {
			continue
		}
		at, said := s.C[class]
		switch {
		case math.Abs(gap) < t.p.ThresholdPP-t.p.HysteresisPP:
			delete(s.C, class) // back on target
		case math.Abs(gap) >= t.p.ThresholdPP:
			if said && now.Before(time.UnixMilli(at).Add(repeat)) && !now.Before(time.UnixMilli(at)) {
				continue
			}
			s.C[class] = now.UnixMilli()
			out = append(out, t.candidate(class, weight, targets[class], gap, pf, MinInputTrust(d, pf.Trust), now))
			stats.DriftFired.Add(1)
		}
	}
	if fmt.Sprint(s.C) != prev {
		marshalState(st, s)
	}
	return out, nil
}

func (t *allocDrift) candidate(class string, weight, target, gap float64, pf worldmodel.PortfolioView,
	trust proactive.Trust, now time.Time) proactive.Candidate {
	thai := t.cfg.isThai()
	dir := "over"
	if gap < 0 {
		dir = "under"
	}
	label := cleanKeyToken(class)
	var title string
	switch {
	case thai && dir == "over":
		title = "สัดส่วน " + label + " สูงกว่าเป้า " + pct1(math.Abs(gap)) + " (ตอนนี้ " + pct1(weight) + " เป้า " + pct1(target) + ")"
	case thai:
		title = "สัดส่วน " + label + " ต่ำกว่าเป้า " + pct1(math.Abs(gap)) + " (ตอนนี้ " + pct1(weight) + " เป้า " + pct1(target) + ")"
	case dir == "over":
		title = "Allocation drift: " + label + " is " + pct1(math.Abs(gap)) + " over target (" + pct1(weight) + " vs " + pct1(target) + ")"
	default:
		title = "Allocation drift: " + label + " is " + pct1(math.Abs(gap)) + " under target (" + pct1(weight) + " vs " + pct1(target) + ")"
	}
	detail := label + " " + usd(pf.ByClass[class]) + " of " + usd(pf.TotalUSD)
	if pf.UnpricedCount > 0 {
		detail += " · " + strconv.Itoa(pf.UnpricedCount) + " assets not priced and left out"
	}
	return proactive.Candidate{
		DedupKey: fmt.Sprintf("%s:%s:%s", KindAllocationDrift, label, dir),
		Severity: proactive.SeverityInfo, Salience: clamp01(math.Abs(gap) / 50), Subject: "portfolio",
		Title: title, Detail: detail,
		Evidence: map[string]any{
			"class": class, "weight_pct": weight, "target_pct": target, "gap_pp": gap, "total_usd": pf.TotalUSD,
			"class_usd": pf.ByClass[class], "unpriced_count": pf.UnpricedCount,
		},
		Trust: trust, Compose: proactive.ComposeTemplate,
		DedupWindow: time.Duration(t.p.RepeatDays) * 24 * time.Hour, At: now,
	}
}
