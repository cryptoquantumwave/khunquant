package triggers

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/logger"
)

var cmpDay = time.Date(2026, 3, 10, 0, 0, 0, 0, bkk) // the day compared, local midnight to midnight

func slotAt(h, m int) time.Time {
	return cmpDay.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute)
}

func out(plan int64, due time.Time, outcome string) DCAOutcome {
	return DCAOutcome{PlanID: plan, DueAt: due, Outcome: outcome}
}

func run(plan int64, at time.Time, status, reason string) DCALegacyRun {
	return DCALegacyRun{PlanID: plan, At: at, Status: status, Reason: reason}
}

func compare(outs []DCAOutcome, runs []DCALegacyRun) []DCAPlanReport {
	return CompareDCA(cmpDay, cmpDay.Add(24*time.Hour), cmpDay.Add(-time.Hour), outs, runs)
}

func onlyReport(t *testing.T, rs []DCAPlanReport) DCAPlanReport {
	t.Helper()
	if len(rs) != 1 {
		t.Fatalf("%d reports, want 1: %+v", len(rs), rs)
	}
	return rs[0]
}

// TestDCAShadowComparison: the three things the daily log is for. A candidate and a
// legacy execution that match; an execution the trigger made no candidate for; a
// candidate the legacy path never acted on.
func TestDCAShadowComparison(t *testing.T) {
	outs := []DCAOutcome{
		out(1, slotAt(9, 0), DCAWouldExecute),  // legacy placed an order: a match
		out(1, slotAt(21, 0), DCAWouldExecute), // legacy did nothing: shadow-only
		out(2, slotAt(9, 0), DCAGateFalse),     // legacy did nothing either: agreement
	}
	runs := []DCALegacyRun{
		run(1, slotAt(9, 0).Add(40*time.Second), "completed", ""),
		run(2, slotAt(15, 30), "completed", ""), // an execution with no slot at all: legacy-only
	}
	rs := compare(outs, runs)
	if len(rs) != 2 {
		t.Fatalf("%d reports, want 2: %+v", len(rs), rs)
	}
	p1, p2 := rs[0], rs[1]
	if p1.PlanID != 1 || p2.PlanID != 2 {
		t.Fatalf("reports are not in plan order: %d, %d", p1.PlanID, p2.PlanID)
	}
	if p1.Slots != 2 || p1.WouldExecute != 2 || p1.Matched != 1 || p1.LegacyPlaced != 1 {
		t.Errorf("plan 1 = %+v", p1)
	}
	if len(p1.ShadowOnly) != 1 || !p1.ShadowOnly[0].At.Equal(slotAt(21, 0)) || p1.ShadowOnly[0].Why != "no legacy attempt" {
		t.Errorf("plan 1 shadow-only = %+v", p1.ShadowOnly)
	}
	if len(p1.LegacyOnly) != 0 {
		t.Errorf("plan 1 legacy-only = %+v", p1.LegacyOnly)
	}
	if p2.Slots != 1 || p2.WouldExecute != 0 || p2.NoCandidate[DCAGateFalse] != 1 {
		t.Errorf("plan 2 = %+v", p2)
	}
	if len(p2.LegacyOnly) != 1 || !p2.LegacyOnly[0].At.Equal(slotAt(15, 30)) || p2.LegacyOnly[0].Why != "no due slot" {
		t.Errorf("plan 2 legacy-only = %+v", p2.LegacyOnly)
	}
}

func TestCompareDCA_AttemptFollowsItsSlotAfterTheModelTurn(t *testing.T) {
	// The attempt is stamped when the tool runs: minutes after the slot, not at it.
	rs := compare(
		[]DCAOutcome{out(1, slotAt(9, 0), DCAWouldExecute)},
		[]DCALegacyRun{run(1, slotAt(9, 0).Add(7*time.Minute), "failed", "ticker unavailable")})
	r := onlyReport(t, rs)
	if r.Matched != 1 || r.LegacyFailed != 1 || len(r.ShadowOnly) != 0 || len(r.LegacyOnly) != 0 {
		t.Errorf("report = %+v", r)
	}
	// Past the match window it is another thing.
	rs = compare(
		[]DCAOutcome{out(1, slotAt(9, 0), DCAWouldExecute)},
		[]DCALegacyRun{run(1, slotAt(9, 0).Add(DCAMatchWindow+time.Minute), "completed", "")})
	r = onlyReport(t, rs)
	if r.Matched != 0 || len(r.ShadowOnly) != 1 || len(r.LegacyOnly) != 1 {
		t.Errorf("an attempt beyond the window matched: %+v", r)
	}
}

func TestCompareDCA_AttemptDoesNotCrossToTheNextSlot(t *testing.T) {
	// Hourly plan: the 10:30 attempt belongs to the 10:00 slot, not the 09:00 one.
	rs := compare(
		[]DCAOutcome{out(1, slotAt(9, 0), DCAWouldExecute), out(1, slotAt(10, 0), DCAWouldExecute)},
		[]DCALegacyRun{run(1, slotAt(10, 30), "completed", "")})
	r := onlyReport(t, rs)
	if r.Matched != 1 || len(r.ShadowOnly) != 1 || !r.ShadowOnly[0].At.Equal(slotAt(9, 0)) {
		t.Errorf("report = %+v", r)
	}
}

func TestCompareDCA_ASecondAttemptForOneSlotIsListed(t *testing.T) {
	rs := compare(
		[]DCAOutcome{out(1, slotAt(9, 0), DCAWouldExecute)},
		[]DCALegacyRun{run(1, slotAt(9, 1), "completed", ""), run(1, slotAt(9, 20), "completed", "")})
	r := onlyReport(t, rs)
	if r.Matched != 1 || len(r.LegacyOnly) != 1 || r.LegacyOnly[0].Why != "second attempt for one slot" {
		t.Errorf("report = %+v", r)
	}
}

func TestCompareDCA_LegacyGateAndCapSkips(t *testing.T) {
	// The tool's own gate said no where the trigger said yes: listed, not matched.
	rs := compare(
		[]DCAOutcome{out(1, slotAt(9, 0), DCAWouldExecute)},
		[]DCALegacyRun{run(1, slotAt(9, 2), "skipped", "trigger condition not met: rsi14 < 40")})
	r := onlyReport(t, rs)
	if r.Matched != 0 || len(r.ShadowOnly) != 1 || r.ShadowOnly[0].Why != "legacy skipped on its gate or cap" {
		t.Errorf("report = %+v", r)
	}
	// Both said no: an agreement.
	rs = compare(
		[]DCAOutcome{out(1, slotAt(9, 0), DCACapReached)},
		[]DCALegacyRun{run(1, slotAt(9, 2), "skipped", "period cap reached: 1/1 executions this day")})
	r = onlyReport(t, rs)
	if r.AgreedSkip != 1 || len(r.LegacyOnly) != 0 || len(r.ShadowOnly) != 0 {
		t.Errorf("report = %+v", r)
	}
	// A skip for another reason (paper mode) is the order path: the candidate matches it.
	rs = compare(
		[]DCAOutcome{out(1, slotAt(9, 0), DCAWouldExecute)},
		[]DCALegacyRun{run(1, slotAt(9, 2), "skipped", "paper mode: simulated, no order placed")})
	r = onlyReport(t, rs)
	if r.Matched != 1 || r.LegacySkipped != 1 {
		t.Errorf("report = %+v", r)
	}
}

// TestCompareDCA_UnjudgedSlotsAreExplainedNotMismatched: a slot the trigger could not
// judge (gate data unavailable) is kept apart, so that a legacy execution on it is
// explained rather than counted against the trigger.
func TestCompareDCA_UnjudgedSlotsAreExplainedNotMismatched(t *testing.T) {
	rs := compare(
		[]DCAOutcome{
			{PlanID: 1, DueAt: slotAt(9, 0), Outcome: DCAGateUnavailable, Reason: "no_data"},
			out(1, slotAt(10, 0), DCAGateError),
			out(1, slotAt(11, 0), DCACapUnknown),
		},
		[]DCALegacyRun{run(1, slotAt(9, 3), "completed", "")})
	r := onlyReport(t, rs)
	if r.Unjudged != 3 || len(r.LegacyOnly) != 0 || len(r.ShadowOnly) != 0 {
		t.Errorf("report = %+v", r)
	}
	if r.NoCandidate[DCAGateUnavailable] != 1 || r.NoCandidate[DCAGateError] != 1 || r.NoCandidate[DCACapUnknown] != 1 {
		t.Errorf("no-candidate counts = %v", r.NoCandidate)
	}
}

func TestCompareDCA_LegacyRanWhereTheTriggerSaidNo(t *testing.T) {
	rs := compare(
		[]DCAOutcome{out(1, slotAt(9, 0), DCAGateFalse)},
		[]DCALegacyRun{run(1, slotAt(9, 3), "completed", "")})
	r := onlyReport(t, rs)
	if len(r.LegacyOnly) != 1 || r.LegacyOnly[0].Why != "trigger said gate_false" {
		t.Errorf("report = %+v", r)
	}
}

func TestCompareDCA_WindowEdgesAndPartialRecord(t *testing.T) {
	to := cmpDay.Add(24 * time.Hour)
	// A 23:50 slot whose attempt landed at 00:05 the next day is the old day's.
	late := out(1, to.Add(-10*time.Minute), DCAWouldExecute)
	next := run(1, to.Add(5*time.Minute), "completed", "")
	r := onlyReport(t, CompareDCA(cmpDay, to, time.Time{}, []DCAOutcome{late}, []DCALegacyRun{next}))
	if r.Matched != 1 || r.LegacyPlaced != 0 {
		t.Errorf("the attempt is the next day's by its stamp, the slot's by its match: %+v", r)
	}
	// And from the next day's side, that attempt is not a mystery.
	nextDay := CompareDCA(to, to.Add(24*time.Hour), time.Time{}, []DCAOutcome{late}, []DCALegacyRun{next})
	if len(nextDay) != 1 || len(nextDay[0].LegacyOnly) != 0 || nextDay[0].LegacyPlaced != 1 {
		t.Errorf("next day = %+v", nextDay)
	}
	// A record that starts after the window does is partial.
	r = onlyReport(t, CompareDCA(cmpDay, to, cmpDay.Add(6*time.Hour), []DCAOutcome{out(1, slotAt(9, 0), DCAWouldExecute)}, nil))
	if !r.Partial {
		t.Error("a record that starts at 06:00 is not complete for the day")
	}
	r = onlyReport(t, CompareDCA(cmpDay, to, cmpDay.Add(-time.Hour), []DCAOutcome{out(1, slotAt(9, 0), DCAWouldExecute)}, nil))
	if r.Partial {
		t.Error("a record that starts before the window is complete")
	}
}

func TestCompareDCA_EmptyInputsGiveNoReports(t *testing.T) {
	if rs := compare(nil, nil); len(rs) != 0 {
		t.Fatalf("%d reports from nothing", len(rs))
	}
}

func TestCompareDCA_IsDeterministic(t *testing.T) {
	var outs []DCAOutcome
	var runs []DCALegacyRun
	for p := int64(5); p >= 1; p-- {
		for h := 0; h < 6; h++ {
			outs = append(outs, out(p, slotAt(h*3, 0), DCAWouldExecute))
			if h%2 == 0 {
				runs = append(runs, run(p, slotAt(h*3, 4), "completed", ""))
			}
		}
	}
	want := fmt.Sprintf("%+v", compare(outs, runs))
	for i := 0; i < 20; i++ {
		// Reverse the inputs: the answer must not depend on their order.
		ro := append([]DCAOutcome(nil), outs...)
		rr := append([]DCALegacyRun(nil), runs...)
		for a, b := 0, len(ro)-1; a < b; a, b = a+1, b-1 {
			ro[a], ro[b] = ro[b], ro[a]
		}
		for a, b := 0, len(rr)-1; a < b; a, b = a+1, b-1 {
			rr[a], rr[b] = rr[b], rr[a]
		}
		if got := fmt.Sprintf("%+v", compare(ro, rr)); got != want {
			t.Fatalf("run %d differs:\n%s\n%s", i, got, want)
		}
	}
}

func TestDCAOutcomes_RecordIsIdempotentBoundedAndOrdered(t *testing.T) {
	o := NewDCAOutcomes(cmpDay)
	o.Record(out(1, slotAt(9, 0), DCAGateUnavailable))
	o.Record(out(1, slotAt(9, 0), DCAWouldExecute)) // a replay: the latest wins
	o.Record(out(2, slotAt(8, 0), DCAGateFalse))
	got := o.Between(cmpDay, cmpDay.Add(24*time.Hour))
	if len(got) != 2 || got[0].PlanID != 2 || got[1].Outcome != DCAWouldExecute {
		t.Fatalf("outcomes = %+v", got)
	}
	if n := len(o.Between(slotAt(9, 0), slotAt(9, 0))); n != 0 {
		t.Errorf("an empty window returned %d", n)
	}
	for i := 0; i < DCAOutcomesCap+500; i++ {
		o.Record(out(3, cmpDay.Add(time.Duration(i)*time.Minute), DCAWouldExecute))
	}
	o.mu.Lock()
	n := len(o.m)
	o.mu.Unlock()
	if n > DCAOutcomesCap {
		t.Fatalf("%d outcomes kept, the cap is %d", n, DCAOutcomesCap)
	}
	var nilOutcomes *DCAOutcomes
	nilOutcomes.Record(out(1, cmpDay, DCAWouldExecute)) // a nil record is a no-op
	if nilOutcomes.Between(cmpDay, cmpDay.Add(time.Hour)) != nil || !nilOutcomes.Since().IsZero() {
		t.Error("a nil record is not empty")
	}
}

// lockedBuf is a bytes.Buffer safe to write from the logger and read from the test.
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestLogDCAComparison_OneInfoLinePerPlanAndOneSummary(t *testing.T) {
	var buf lockedBuf
	logger.SetAdditionalWriter(&buf)
	t.Cleanup(func() { logger.SetAdditionalWriter(nil) })

	rs := compare(
		[]DCAOutcome{
			out(1, slotAt(9, 0), DCAWouldExecute), out(1, slotAt(21, 0), DCAWouldExecute),
			{PlanID: 2, DueAt: slotAt(9, 0), Outcome: DCAGateUnavailable, Reason: "no_data"},
		},
		[]DCALegacyRun{run(1, slotAt(9, 1), "completed", ""), run(2, slotAt(9, 2), "completed", "")})
	LogDCAComparison(cmpDay, cmpDay.Add(24*time.Hour), rs)

	text := buf.String()
	lines := strings.Split(strings.TrimSpace(text), "\n")
	var perPlan, summary int
	for _, l := range lines {
		switch {
		case strings.Contains(l, "DCA shadow comparison for one plan"):
			perPlan++
		case strings.Contains(l, "DCA shadow comparison"):
			summary++
		}
		if !strings.Contains(l, "INF") {
			t.Errorf("not an INFO line: %q", l)
		}
	}
	if perPlan != 2 || summary != 1 {
		t.Fatalf("%d per-plan lines and %d summaries, want 2 and 1:\n%s", perPlan, summary, text)
	}
	if !strings.Contains(text, "no legacy attempt") || !strings.Contains(text, "unjudged") {
		t.Errorf("the unmatched ones are not in the log:\n%s", text)
	}
}
