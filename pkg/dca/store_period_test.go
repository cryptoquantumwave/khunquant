package dca

import (
	"context"
	"testing"
	"time"
)

// setLocalZone points time.Local at a fixed +07:00 zone for the test, the zone
// the guardrail was miscounting in, and restores it on cleanup.
func setLocalZone(t *testing.T) {
	t.Helper()
	orig := time.Local
	time.Local = time.FixedZone("ICT", 7*3600)
	t.Cleanup(func() { time.Local = orig })
}

func saveExecAt(t *testing.T, s *Store, planID int64, at time.Time) int64 {
	t.Helper()
	e := minimalExecution(planID)
	e.ExecutedAt = at
	id, err := s.SaveExecution(context.Background(), e)
	if err != nil {
		t.Fatalf("SaveExecution: %v", err)
	}
	return id
}

func countPeriods(t *testing.T, s *Store, planID int64) (hour, day, week int) {
	t.Helper()
	ctx := context.Background()
	var err error
	if hour, err = s.CountExecutionsInPeriod(ctx, planID, "hour"); err != nil {
		t.Fatalf("hour: %v", err)
	}
	if day, err = s.CountExecutionsInPeriod(ctx, planID, "day"); err != nil {
		t.Fatalf("day: %v", err)
	}
	if week, err = s.CountExecutionsInPeriod(ctx, planID, "week"); err != nil {
		t.Fatalf("week: %v", err)
	}
	return hour, day, week
}

// A row stored the way dca_execute_order stores it (now.UTC(), "...Z") must be
// counted when the host zone is +07:00. The old text comparison missed it for
// "hour" and for local 00:00-07:00 under "day".
func TestStore_CountExecutionsInPeriod_UTCRowInNonUTCZone(t *testing.T) {
	setLocalZone(t)
	s := newTestStore(t)
	planID, _ := s.SavePlan(context.Background(), minimalPlan("TZUTC"))

	saveExecAt(t, s, planID, time.Now().UTC())

	hour, day, week := countPeriods(t, s, planID)
	if hour != 1 || day != 1 || week != 1 {
		t.Errorf("counts (hour,day,week) = (%d,%d,%d), want (1,1,1)", hour, day, week)
	}
}

// A row in local-offset form still counts, as it did before.
func TestStore_CountExecutionsInPeriod_LocalOffsetRow(t *testing.T) {
	setLocalZone(t)
	s := newTestStore(t)
	planID, _ := s.SavePlan(context.Background(), minimalPlan("TZLocal"))

	saveExecAt(t, s, planID, time.Now().In(time.Local))

	hour, day, week := countPeriods(t, s, planID)
	if hour != 1 || day != 1 || week != 1 {
		t.Errorf("counts (hour,day,week) = (%d,%d,%d), want (1,1,1)", hour, day, week)
	}
}

// Just after local midnight, stored as UTC, still falls in today: its UTC
// calendar date is yesterday, so a text comparison would drop it.
func TestStore_CountExecutionsInPeriod_DayBoundaryAcrossZones(t *testing.T) {
	setLocalZone(t)
	s := newTestStore(t)
	planID, _ := s.SavePlan(context.Background(), minimalPlan("TZBoundary"))

	now := time.Now()
	y, mo, d := now.Date()
	startOfToday := time.Date(y, mo, d, 0, 0, 0, 0, time.Local)

	saveExecAt(t, s, planID, startOfToday.Add(time.Minute).UTC())   // today
	saveExecAt(t, s, planID, startOfToday.Add(-time.Minute).UTC())  // yesterday, local
	saveExecAt(t, s, planID, startOfToday.Add(-time.Minute))        // yesterday, local-offset form
	saveExecAt(t, s, planID, startOfToday.Add(-25*time.Hour).UTC()) // well before

	_, day, _ := countPeriods(t, s, planID)
	if day != 1 {
		t.Errorf("day count = %d, want 1 (only the row after local midnight)", day)
	}
}

// An unparseable executed_at counts toward the cap (fail closed).
func TestStore_CountExecutionsInPeriod_UnparseableCounts(t *testing.T) {
	setLocalZone(t)
	s := newTestStore(t)
	ctx := context.Background()
	planID, _ := s.SavePlan(ctx, minimalPlan("TZCorrupt"))

	id := saveExecAt(t, s, planID, time.Now().UTC().Add(-30*24*time.Hour))
	if _, err := s.db.ExecContext(ctx,
		`UPDATE dca_executions SET executed_at = ? WHERE id = ?`, "not-a-timestamp", id); err != nil {
		t.Fatalf("corrupt executed_at: %v", err)
	}

	hour, day, week := countPeriods(t, s, planID)
	if hour != 1 || day != 1 || week != 1 {
		t.Errorf("counts (hour,day,week) = (%d,%d,%d), want (1,1,1)", hour, day, week)
	}
}

// Only completed rows count, as before.
func TestStore_CountExecutionsInPeriod_OnlyCompleted(t *testing.T) {
	setLocalZone(t)
	s := newTestStore(t)
	planID, _ := s.SavePlan(context.Background(), minimalPlan("TZStatus"))

	for _, status := range []string{"completed", "failed", "skipped"} {
		e := minimalExecution(planID)
		e.Status = status
		if _, err := s.SaveExecution(context.Background(), e); err != nil {
			t.Fatalf("SaveExecution(%s): %v", status, err)
		}
	}

	hour, day, week := countPeriods(t, s, planID)
	if hour != 1 || day != 1 || week != 1 {
		t.Errorf("counts (hour,day,week) = (%d,%d,%d), want (1,1,1)", hour, day, week)
	}
}

func TestStore_CountExecutionsInPeriod_UnknownPeriod(t *testing.T) {
	s := newTestStore(t)
	planID, _ := s.SavePlan(context.Background(), minimalPlan("BadPeriod"))
	if _, err := s.CountExecutionsInPeriod(context.Background(), planID, "month"); err == nil {
		t.Error("expected an error for unknown period")
	}
}
