package triggers

import (
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/logger"
)

// DCAOutcome is what the dca_due trigger decided for one due slot of one plan.
type DCAOutcome struct {
	PlanID int64
	DueAt  time.Time
	// Outcome is one of the DCA* constants; Reason refines DCAGateUnavailable
	// ("no_data", "stale_data", "needs_ohlcv", "too_few_bars", "unsupported",
	// "no_evaluator").
	Outcome string
	Reason  string
	// Late says the slot was seen more than a minute after its time.
	Late bool
}

type dcaOutKey struct {
	plan int64
	due  int64
}

// DCAOutcomes remembers the outcomes of the dca_due trigger so that a daily
// comparison can set them beside what the legacy path did, including the slots that
// made no candidate. It lives in memory: it is a diagnostic record, not state any
// decision depends on, and a restart empties it (Since says from when it is
// complete). Recording a slot twice (a replay) keeps the latest. It is safe for
// concurrent use and bounded by DCAOutcomesCap.
type DCAOutcomes struct {
	mu    sync.Mutex
	since time.Time
	m     map[dcaOutKey]DCAOutcome
}

// NewDCAOutcomes returns an empty record that is complete from since on.
func NewDCAOutcomes(since time.Time) *DCAOutcomes {
	return &DCAOutcomes{since: since, m: map[dcaOutKey]DCAOutcome{}}
}

// Since is the moment the record starts: slots due before it may be missing.
func (o *DCAOutcomes) Since() time.Time {
	if o == nil {
		return time.Time{}
	}
	return o.since
}

// Record stores one outcome. It is a no-op on a nil record.
func (o *DCAOutcomes) Record(out DCAOutcome) {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.m) >= DCAOutcomesCap {
		all := o.sortedLocked()
		for _, old := range all[:len(all)/10+1] {
			delete(o.m, dcaOutKey{old.PlanID, old.DueAt.UnixMilli()})
		}
	}
	o.m[dcaOutKey{out.PlanID, out.DueAt.UnixMilli()}] = out
}

// Between returns the outcomes whose slot is in [from, to), oldest first, by plan
// within a time.
func (o *DCAOutcomes) Between(from, to time.Time) []DCAOutcome {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	var out []DCAOutcome
	for _, x := range o.sortedLocked() {
		if !x.DueAt.Before(from) && x.DueAt.Before(to) {
			out = append(out, x)
		}
	}
	return out
}

func (o *DCAOutcomes) sortedLocked() []DCAOutcome {
	all := make([]DCAOutcome, 0, len(o.m))
	for _, x := range o.m {
		all = append(all, x)
	}
	sort.Slice(all, func(i, j int) bool {
		if !all[i].DueAt.Equal(all[j].DueAt) {
			return all[i].DueAt.Before(all[j].DueAt)
		}
		return all[i].PlanID < all[j].PlanID
	})
	return all
}

// ---- the daily comparison ---------------------------------------------------

// DCAMatchWindow is how long after a slot a legacy attempt is still taken for that
// slot's: the cron grace plus the time of the model turn that runs the order tool.
// An attempt is recorded when the tool runs, not when the slot is due.
const DCAMatchWindow = 90 * time.Minute

// dcaMatchSlack is how much earlier than its slot an attempt may be stamped (clock
// differences between the store and the source).
const dcaMatchSlack = 2 * time.Minute

// DCALegacyRun is one row of the legacy path's record of an attempt (dca_executions):
// the plan, when the attempt was made, how it ended and, for a skip, why.
type DCALegacyRun struct {
	PlanID int64
	At     time.Time
	// Status is "completed", "failed" or "skipped".
	Status string
	Reason string
}

// Classes of a legacy attempt.
const (
	legacyPlaced      = "placed"
	legacyFailed      = "failed"
	legacySkippedGate = "skipped_gate" // the tool's own gate or cap check refused
	legacySkipped     = "skipped"      // any other refusal: paper mode, risk limits, permission
)

// classifyLegacy sorts an attempt. The prefixes are the words the order tool writes
// for a gate or cap refusal; they only decide how a mismatch is labelled.
func classifyLegacy(r DCALegacyRun) string {
	switch r.Status {
	case "completed":
		return legacyPlaced
	case "failed":
		return legacyFailed
	}
	if strings.HasPrefix(r.Reason, "trigger condition not met") || strings.HasPrefix(r.Reason, "period cap reached") {
		return legacySkippedGate
	}
	return legacySkipped
}

// DCAUnmatched is one slot or attempt without its counterpart on the other side, and
// why it is listed.
type DCAUnmatched struct {
	At  time.Time
	Why string
}

// DCAPlanReport is the comparison for one plan over one window.
type DCAPlanReport struct {
	PlanID int64
	// Slots is the number of due slots the trigger judged in the window,
	// WouldExecute those that made a candidate.
	Slots, WouldExecute int
	// Matched are the candidates with a legacy attempt of the order path (placed,
	// failed or skipped for another reason); AgreedSkip the slots with no candidate
	// whose legacy outcome was also a gate or cap refusal.
	Matched, AgreedSkip int
	// Legacy attempts in the window by how they ended.
	LegacyPlaced, LegacyFailed, LegacySkipped int
	// ShadowOnly are the candidates the legacy path did not act on; LegacyOnly the
	// attempts the trigger had no candidate for, with a reason.
	ShadowOnly, LegacyOnly []DCAUnmatched
	// Unjudged counts the slots the trigger could not judge (gate data unavailable, a
	// gate error, an unreadable cap), kept apart so that a mismatch on one is
	// explainable: it is not a disagreement.
	Unjudged int
	// NoCandidate counts the slots that made no candidate, by outcome.
	NoCandidate map[string]int
	// Partial says the trigger's record starts after the window does, so slots may be
	// missing from it.
	Partial bool
}

// CompareDCA sets the trigger's outcomes beside the legacy attempts for the window
// [from, to). outcomes must cover [from-DCAMatchWindow, to) and legacy [from,
// to+DCAMatchWindow), so that an attempt just across a boundary finds its slot. since
// is DCAOutcomes.Since. The result has one report per plan that has a slot or an
// attempt in the window, by plan ID.
//
// An attempt belongs to the slot whose time it follows, within DCAMatchWindow and
// before the plan's next slot; the first such attempt is the slot's match, and
// further ones are listed as extra. The comparison is deterministic: the same inputs
// give the same reports.
func CompareDCA(from, to, since time.Time, outcomes []DCAOutcome, legacy []DCALegacyRun) []DCAPlanReport {
	byPlanOut := map[int64][]DCAOutcome{}
	for _, o := range outcomes {
		byPlanOut[o.PlanID] = append(byPlanOut[o.PlanID], o)
	}
	byPlanRun := map[int64][]DCALegacyRun{}
	for _, r := range legacy {
		byPlanRun[r.PlanID] = append(byPlanRun[r.PlanID], r)
	}
	ids := map[int64]bool{}
	for id := range byPlanOut {
		ids[id] = true
	}
	for id := range byPlanRun {
		ids[id] = true
	}
	order := make([]int64, 0, len(ids))
	for id := range ids {
		order = append(order, id)
	}
	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })

	var reports []DCAPlanReport
	for _, id := range order {
		rep, any := comparePlan(id, from, to, since, byPlanOut[id], byPlanRun[id])
		if any {
			reports = append(reports, rep)
		}
	}
	return reports
}

func comparePlan(id int64, from, to, since time.Time, outs []DCAOutcome, runs []DCALegacyRun) (DCAPlanReport, bool) {
	sort.SliceStable(outs, func(i, j int) bool { return outs[i].DueAt.Before(outs[j].DueAt) })
	sort.SliceStable(runs, func(i, j int) bool { return runs[i].At.Before(runs[j].At) })
	rep := DCAPlanReport{PlanID: id, NoCandidate: map[string]int{}, Partial: !since.IsZero() && since.After(from)}
	claimed := make([]bool, len(runs))

	inWindow := func(t time.Time) bool { return !t.Before(from) && t.Before(to) }
	for i, o := range outs {
		end := o.DueAt.Add(DCAMatchWindow)
		if i+1 < len(outs) && outs[i+1].DueAt.Before(end) {
			end = outs[i+1].DueAt
		}
		match := -1
		for k, r := range runs {
			if claimed[k] || r.At.Before(o.DueAt.Add(-dcaMatchSlack)) || !r.At.Before(end) {
				continue
			}
			claimed[k] = true
			if match < 0 {
				match = k
			} else if inWindow(r.At) {
				rep.LegacyOnly = append(rep.LegacyOnly, DCAUnmatched{r.At, "second attempt for one slot"})
			}
		}
		if !inWindow(o.DueAt) {
			continue // there to claim attempts, not to be counted
		}
		rep.Slots++
		class := ""
		if match >= 0 {
			class = classifyLegacy(runs[match])
		}
		switch o.Outcome {
		case DCAWouldExecute:
			rep.WouldExecute++
			switch {
			case match < 0:
				rep.ShadowOnly = append(rep.ShadowOnly, DCAUnmatched{o.DueAt, "no legacy attempt"})
			case class == legacySkippedGate:
				rep.ShadowOnly = append(rep.ShadowOnly, DCAUnmatched{o.DueAt, "legacy skipped on its gate or cap"})
			default:
				rep.Matched++
			}
		case DCAGateUnavailable, DCAGateError, DCACapUnknown, DCAStaleEvent, DCAInvalid:
			rep.NoCandidate[o.Outcome]++
			rep.Unjudged++
		default:
			rep.NoCandidate[o.Outcome]++
			switch {
			case match < 0:
				// The shadow said no and the legacy path did nothing: agreement.
			case class == legacySkippedGate:
				rep.AgreedSkip++
			default:
				rep.LegacyOnly = append(rep.LegacyOnly, DCAUnmatched{runs[match].At, "trigger said " + o.Outcome})
			}
		}
	}
	for k, r := range runs {
		if !inWindow(r.At) {
			continue
		}
		switch classifyLegacy(r) {
		case legacyPlaced:
			rep.LegacyPlaced++
		case legacyFailed:
			rep.LegacyFailed++
		default:
			rep.LegacySkipped++
		}
		if !claimed[k] {
			rep.LegacyOnly = append(rep.LegacyOnly, DCAUnmatched{r.At, "no due slot"})
		}
	}
	sort.SliceStable(rep.LegacyOnly, func(i, j int) bool { return rep.LegacyOnly[i].At.Before(rep.LegacyOnly[j].At) })
	return rep, rep.Slots > 0 || rep.LegacyPlaced+rep.LegacyFailed+rep.LegacySkipped > 0
}

// maxLoggedUnmatched bounds how many unmatched entries one log line lists.
const maxLoggedUnmatched = 5

// LogDCAComparison writes the comparison as INFO lines: one per plan, then one for
// the whole window. Nothing is sent to the owner.
func LogDCAComparison(from, to time.Time, reports []DCAPlanReport) {
	var total DCAPlanReport
	for _, r := range reports {
		f := map[string]any{
			"plan_id": r.PlanID, "from": from.UTC().Format(time.RFC3339), "to": to.UTC().Format(time.RFC3339),
			"slots": r.Slots, "would_execute": r.WouldExecute, "matched": r.Matched, "agreed_skip": r.AgreedSkip,
			"legacy_placed": r.LegacyPlaced, "legacy_failed": r.LegacyFailed, "legacy_skipped": r.LegacySkipped,
			"shadow_only": len(r.ShadowOnly), "legacy_only": len(r.LegacyOnly), "unjudged": r.Unjudged,
			"partial": r.Partial,
		}
		if len(r.NoCandidate) > 0 {
			f["no_candidate"] = r.NoCandidate
		}
		if len(r.ShadowOnly) > 0 {
			f["shadow_only_at"] = unmatchedText(r.ShadowOnly)
		}
		if len(r.LegacyOnly) > 0 {
			f["legacy_only_at"] = unmatchedText(r.LegacyOnly)
		}
		logger.InfoCF("dca-shadow", "DCA shadow comparison for one plan", f)
		total.Slots += r.Slots
		total.WouldExecute += r.WouldExecute
		total.Matched += r.Matched
		total.Unjudged += r.Unjudged
		total.ShadowOnly = append(total.ShadowOnly, r.ShadowOnly...)
		total.LegacyOnly = append(total.LegacyOnly, r.LegacyOnly...)
	}
	logger.InfoCF("dca-shadow", "DCA shadow comparison", map[string]any{
		"from": from.UTC().Format(time.RFC3339), "to": to.UTC().Format(time.RFC3339), "plans": len(reports),
		"slots": total.Slots, "would_execute": total.WouldExecute, "matched": total.Matched,
		"shadow_only": len(total.ShadowOnly), "legacy_only": len(total.LegacyOnly), "unjudged": total.Unjudged,
	})
}

func unmatchedText(us []DCAUnmatched) []string {
	out := make([]string, 0, min(len(us), maxLoggedUnmatched))
	for i, u := range us {
		if i == maxLoggedUnmatched {
			break
		}
		out = append(out, u.At.UTC().Format(time.RFC3339)+" "+u.Why)
	}
	return out
}
