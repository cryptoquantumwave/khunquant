package cron

import (
	"testing"
	"time"

	"github.com/adhocore/gronx"
)

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("LoadLocation(%q): %v", name, err)
	}
	return loc
}

// TestGronx_EvaluatesInReferenceLocation is the spike required before the
// timezone work: it proves gronx evaluates a cron expression in the location of
// the reference time (rather than in UTC or time.Local), including across a DST
// transition. If this ever fails, computeNextRun's `.In(loc)` approach is no
// longer valid and the offset-conversion fallback is needed.
func TestGronx_EvaluatesInReferenceLocation(t *testing.T) {
	bkk := mustLoc(t, "Asia/Bangkok")
	ny := mustLoc(t, "America/New_York")

	// Monday 2025-06-02 00:00 UTC; Bangkok (UTC+7) is already 07:00 that day.
	ref := time.Date(2025, 6, 2, 0, 0, 0, 0, time.UTC).In(bkk)
	next, err := gronx.NextTickAfter("0 9 * * 1", ref, false)
	if err != nil {
		t.Fatalf("NextTickAfter: %v", err)
	}
	if want := time.Date(2025, 6, 2, 9, 0, 0, 0, bkk); !next.Equal(want) {
		t.Errorf("Bangkok: got %s, want %s", next, want)
	}
	if next.Location() != bkk {
		t.Errorf("result location = %v, want Asia/Bangkok", next.Location())
	}
	// 09:00 ICT is 02:00 UTC, which a UTC-evaluated expression would never produce.
	if got := next.UTC().Hour(); got != 2 {
		t.Errorf("09:00 ICT should be 02:00 UTC, got UTC hour %d", got)
	}

	// DST spring-forward in New York: 2025-03-09 02:00 EST -> 03:00 EDT.
	// "30 9 * * *" is 09:30 local on both sides, i.e. 14:30 UTC before and
	// 13:30 UTC after the change.
	before := time.Date(2025, 3, 8, 12, 0, 0, 0, ny) // EST
	next, err = gronx.NextTickAfter("30 9 * * *", before, false)
	if err != nil {
		t.Fatalf("NextTickAfter: %v", err)
	}
	if want := time.Date(2025, 3, 9, 9, 30, 0, 0, ny); !next.Equal(want) {
		t.Errorf("NY spring-forward: got %s, want %s", next, want)
	}
	if got := next.UTC().Format("15:04"); got != "13:30" {
		t.Errorf("NY spring-forward: 09:30 EDT should be 13:30 UTC, got %s", got)
	}

	// DST fall-back: 2025-11-02 02:00 EDT -> 01:00 EST.
	before = time.Date(2025, 11, 1, 12, 0, 0, 0, ny) // EDT
	next, err = gronx.NextTickAfter("30 9 * * *", before, false)
	if err != nil {
		t.Fatalf("NextTickAfter: %v", err)
	}
	if want := time.Date(2025, 11, 2, 9, 30, 0, 0, ny); !next.Equal(want) {
		t.Errorf("NY fall-back: got %s, want %s", next, want)
	}
	if got := next.UTC().Format("15:04"); got != "14:30" {
		t.Errorf("NY fall-back: 09:30 EST should be 14:30 UTC, got %s", got)
	}
}
