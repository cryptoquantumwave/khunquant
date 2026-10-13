package triggers

import (
	"context"
	"encoding/json"
	"math"
	"strconv"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/worldmodel"
)

const (
	drawdownWindow = 24 * time.Hour
	// peakHysteresisPP: a drawdown from the peak has to recover this many points under
	// a tier's line before the tier counts as left (so that re-entering it is news).
	peakHysteresisPP = 1.0
	// baselineMaxAge: the day-start baseline of a day older than this is not used (the
	// model starts a new one at the first snapshot of every owner-local day).
	baselineMaxAge = 36 * time.Hour
	// classSumTolerance: ByClass has to add up to the total within this fraction.
	classSumTolerance = 0.005

	drawRefDay  = "day"
	drawRefPeak = "peak"
)

type drawState struct {
	// Day is the owner-local date the "day" ledger belongs to; a new date forgets it.
	Day string                 `json:"d,omitempty"`
	L   map[string]ledgerEntry `json:"l,omitempty"`
	RB  rebaseCount            `json:"rb"`
}

// rebaseCount counts, for one owner-local day, how many times the model moved the
// day-start baseline because the set of unpriced assets changed. The count lives here
// and not in the world model.
type rebaseCount struct {
	Date     string `json:"date,omitempty"`
	Start    int64  `json:"start,omitempty"` // DayStartAt, unix ms
	N        int    `json:"n,omitempty"`
	Notified bool   `json:"notified,omitempty"`
}

type drawdown struct {
	id  string
	p   DrawdownParams
	cfg PortfolioConfig
}

func drawdownFactory(cfg PortfolioConfig) Factory {
	return func(spec Spec) (Trigger, error) {
		p, err := decodeParams(spec.Params, DefaultDrawdownParams())
		if err != nil {
			return nil, err
		}
		return &drawdown{id: spec.Kind + ":" + spec.ID, p: p, cfg: cfg}, nil
	}
}

func (t *drawdown) ID() string { return t.id }

func (t *drawdown) Kinds() []events.Kind {
	return []events.Kind{events.KindPerceptionPortfolioSnapshot}
}

func (t *drawdown) Demands() []perception.Demand { return nil }

// tierOf is how many of the ascending tiers a drawdown of dd percent has reached.
func tierOf(dd float64, tiers []float64) int {
	n := 0
	for _, x := range tiers {
		if dd >= x {
			n++
		}
	}
	return n
}

// Evaluate judges the portfolio snapshot THIS event brought against the day-start
// baseline and the 30-day peak, both of which the world model keeps on the same
// footing as the total (see worldmodel.PortfolioView). It never subtracts one view
// from another: the model re-bases both when the set of unpriced assets changes and
// refuses to make a change across a re-base, so the figures read here are always
// comparable.
//
// It refuses, and counts, a snapshot that is stale, restored, from a source below the
// exchange's trust, whose class values do not add up to its total, or while the
// portfolio source reports it cannot read an account (a half-loaded portfolio must
// never look like a drawdown; the source emits only complete snapshots, and this
// asserts it again). Tiers escalate only: inside the 24-hour window a tier is said
// only if it is higher than the last one said for that reference, and the ledger is
// forgotten after the window.
//
// A drawdown is at most a Warning. Nothing here is protective and nothing acts.
func (t *drawdown) Evaluate(_ context.Context, v worldmodel.View, d worldmodel.Diff, st *State) ([]proactive.Candidate, error) {
	pf, ok := t.cfg.snapshotOf(v, d)
	if !ok {
		return nil, nil
	}
	stats := t.cfg.stats()
	now := v.Now()
	var s drawState
	if len(st.Raw) > 0 && json.Unmarshal(st.Raw, &s) != nil {
		s = drawState{}
	}
	if s.L == nil {
		s.L = map[string]ledgerEntry{}
	}
	prevRaw := string(st.Raw)
	var out []proactive.Candidate

	// Re-base counting: the day-start baseline moved inside the same local day.
	startMS := pf.DayStartAt.UnixMilli()
	if pf.DayDate != "" {
		if s.RB.Date != pf.DayDate {
			s.RB = rebaseCount{Date: pf.DayDate, Start: startMS}
		} else if s.RB.Start != startMS {
			s.RB.N++
			s.RB.Start = startMS
			stats.DrawdownRebased.Add(1)
		}
		if s.RB.N >= t.p.RebaseNoticeCount && !s.RB.Notified {
			s.RB.Notified = true
			out = append(out, t.rebaseNotice(pf, s.RB.N, now))
			stats.RebaseNotices.Add(1)
		}
	}

	if ss, ok := v.SourceState(worldmodel.PortfolioSource); ok && ss.Degraded {
		stats.DrawdownSourceDegraded.Add(1)
		t.save(st, s, prevRaw)
		return out, nil
	}
	var sum float64
	for _, x := range pf.ByClass {
		sum += x
	}
	if !finite(sum) || math.Abs(sum-pf.TotalUSD) > classSumTolerance*pf.TotalUSD+0.01 {
		stats.DrawdownInconsistent.Add(1)
		t.save(st, s, prevRaw)
		return out, nil
	}

	if s.Day != pf.DayDate {
		delete(s.L, drawRefDay) // a new day, a new baseline, a new ledger
		s.Day = pf.DayDate
	}
	if e, ok := s.L[drawRefDay]; ok && now.Sub(time.UnixMilli(e.AtMS)) >= drawdownWindow {
		delete(s.L, drawRefDay)
	}
	peakWindow := time.Duration(t.p.PeakRepeatDays) * 24 * time.Hour
	if e, ok := s.L[drawRefPeak]; ok && now.Sub(time.UnixMilli(e.AtMS)) >= peakWindow {
		delete(s.L, drawRefPeak)
	}

	baselineOK := pf.DayDate != "" && finite(pf.DayStartUSD) && pf.DayStartUSD > 0 &&
		pf.DayStartAt.Before(pf.At) && v.Fresh(pf.DayStartAt, baselineMaxAge)
	if baselineOK {
		dd := (pf.DayStartUSD - pf.TotalUSD) / pf.DayStartUSD * 100
		if c, ok := t.tier(s.L, drawRefDay, dd, t.p.DayTiers, drawdownWindow, pf.DayStartUSD, pf, d, now); ok {
			out = append(out, c)
		}
	} else {
		stats.DrawdownNoBaseline.Add(1)
	}
	if finite(pf.Peak30dUSD) && pf.Peak30dUSD > 0 && pf.Peak30dUSD >= pf.TotalUSD {
		dd := (pf.Peak30dUSD - pf.TotalUSD) / pf.Peak30dUSD * 100
		// Stepping back under the tier (with a margin) forgets it, so that re-entering
		// counts as entering; a drawdown that merely persists is not news every day.
		if e, ok := s.L[drawRefPeak]; ok {
			if held := tierOf(dd+peakHysteresisPP, t.p.PeakTiers); held < e.Tier {
				if held == 0 {
					delete(s.L, drawRefPeak)
				} else {
					e.Tier = held
					s.L[drawRefPeak] = e
				}
			}
		}
		if c, ok := t.tier(s.L, drawRefPeak, dd, t.p.PeakTiers, peakWindow, pf.Peak30dUSD, pf, d, now); ok {
			out = append(out, c)
		}
	}
	t.save(st, s, prevRaw)
	return out, nil
}

func (t *drawdown) save(st *State, s drawState, prevRaw string) {
	if len(s.L) == 0 {
		s.L = nil
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return
	}
	if string(raw) != prevRaw {
		st.Raw, st.Dirty = raw, true
	}
}

// tier moves the ledger of ref and returns the candidate when the drawdown reached a
// tier that is worth saying.
func (t *drawdown) tier(led map[string]ledgerEntry, ref string, dd float64, tiers []float64, window time.Duration, baseline float64,
	pf worldmodel.PortfolioView, d worldmodel.Diff, now time.Time) (proactive.Candidate, bool) {
	n := tierOf(dd, tiers)
	if !finite(dd) || n == 0 || !led[ref].escalates(n, now, window) {
		return proactive.Candidate{}, false
	}
	led[ref] = ledgerEntry{Tier: n, AtMS: now.UnixMilli()}
	t.cfg.stats().DrawdownFired.Add(1)
	return t.candidate(ref, n, dd, tiers[n-1], window, baseline, pf, MinInputTrust(d, pf.Trust), now), true
}

func (t *drawdown) candidate(ref string, tier int, dd, line float64, window time.Duration, baseline float64, pf worldmodel.PortfolioView,
	trust proactive.Trust, now time.Time) proactive.Candidate {
	thai := t.cfg.isThai()
	sev := proactive.SeverityWarning
	if ref == drawRefDay && tier == 1 {
		sev = proactive.SeverityNotice
	}
	compose := proactive.ComposeTemplate
	if tier >= 2 {
		compose = proactive.ComposeLLM
	}
	var title, from string
	switch {
	case ref == drawRefDay && thai:
		title, from = "พอร์ตลดลง "+pct1(dd)+" จากต้นวัน", "ต้นวัน"
	case ref == drawRefDay:
		title, from = "Portfolio is down "+pct1(dd)+" from the day start", "Day start"
	case thai:
		title, from = "พอร์ตลดลง "+pct1(dd)+" จากจุดสูงสุด 30 วัน", "จุดสูงสุด 30 วัน"
	default:
		title, from = "Portfolio is down "+pct1(dd)+" from its 30-day peak", "30-day peak"
	}
	detail := from + " " + usd(baseline) + " → now " + usd(pf.TotalUSD) + " · tier " + strconv.Itoa(tier) + " (line " + pct1(line) + ")"
	if thai {
		detail = from + " " + usd(baseline) + " → ตอนนี้ " + usd(pf.TotalUSD) + " · ระดับ " + strconv.Itoa(tier) + " (เส้น " + pct1(line) + ")"
	}
	if pf.UnpricedCount > 0 {
		if thai {
			detail += "\nไม่รวม " + strconv.Itoa(pf.UnpricedCount) + " สินทรัพย์ที่ตีราคาไม่ได้"
		} else {
			detail += "\nExcludes " + strconv.Itoa(pf.UnpricedCount) + " assets that cannot be priced"
		}
	}
	return proactive.Candidate{
		DedupKey: DrawdownKey(ref, tier),
		Severity: sev, Salience: clamp01(0.4 + 0.15*float64(tier)), Subject: "portfolio",
		Title: title, Detail: detail,
		Evidence: map[string]any{
			"reference": ref, "tier": tier, "drawdown_pct": dd, "line_pct": line, "total_usd": pf.TotalUSD,
			"baseline_usd": baseline, "unpriced_count": pf.UnpricedCount, "day_date": pf.DayDate,
			"baseline_rebased": pf.DayStartRebased,
		},
		Trust: trust, Compose: compose, DedupWindow: window, At: now,
	}
}

// rebaseNotice is the once-a-day message that the portfolio change alerts cannot be
// relied on: the set of assets that cannot be priced keeps changing, so the baselines
// keep moving and a drawdown or a move of the total cannot be judged.
func (t *drawdown) rebaseNotice(pf worldmodel.PortfolioView, n int, now time.Time) proactive.Candidate {
	thai := t.cfg.isThai()
	var title string
	switch {
	case pf.UnpricedCount > 0 && thai:
		title = "การแจ้งเตือนการเปลี่ยนแปลงของพอร์ตวันนี้ไม่น่าเชื่อถือ: " + strconv.Itoa(pf.UnpricedCount) + " สินทรัพย์ตีราคาได้ไม่สม่ำเสมอ"
	case pf.UnpricedCount > 0:
		title = "Portfolio change alerts are unreliable today: " + strconv.Itoa(pf.UnpricedCount) + " assets cannot be priced consistently"
	case thai:
		title = "การแจ้งเตือนการเปลี่ยนแปลงของพอร์ตวันนี้ไม่น่าเชื่อถือ: ชุดสินทรัพย์ที่ตีราคาไม่ได้เปลี่ยนไปมา"
	default:
		title = "Portfolio change alerts are unreliable today: the set of assets that cannot be priced keeps changing"
	}
	detail := "The valuation baseline was re-based " + strconv.Itoa(n) + " times today; drawdown alerts need a steady baseline."
	if thai {
		detail = "จุดเปรียบเทียบมูลค่าถูกตั้งใหม่ " + strconv.Itoa(n) + " ครั้งวันนี้ การแจ้งเตือนการลดลงของพอร์ตต้องใช้จุดเปรียบเทียบที่คงที่"
	}
	return proactive.Candidate{
		DedupKey: DrawdownRebaseKey(pf.DayDate),
		Severity: proactive.SeverityNotice, Salience: 0.5, Subject: "portfolio",
		Title: title, Detail: detail,
		Evidence: map[string]any{"rebases": n, "unpriced_count": pf.UnpricedCount, "day_date": pf.DayDate, "unpriced_truncated": pf.UnpricedTruncated},
		Trust:    proactive.TrustSystem, Compose: proactive.ComposeTemplate, DedupWindow: drawdownWindow, At: now,
	}
}
