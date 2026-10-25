package dca

import (
	"context"
	"testing"
	"time"
)

// The mandate workflow stands down for a slot the legacy job already ran. A
// legacy run that left a pending or unknown order counts: an order may be live.
func TestStore_LegacyRunSince_CountsPendingAndUnknown(t *testing.T) {
	ctx := context.Background()
	since := time.Now().Add(-time.Hour)
	for _, tc := range []struct {
		status string
		want   bool
	}{{"completed", true}, {"pending", true}, {"unknown", true}, {"failed", false}, {"skipped", false}} {
		s := newTestStore(t)
		planID, _ := s.SavePlan(ctx, minimalPlan("Legacy-"+tc.status))
		e := minimalExecution(planID)
		e.ExecutedAt = time.Now()
		e.Status = tc.status
		if _, err := s.SaveExecution(ctx, e); err != nil {
			t.Fatalf("SaveExecution(%s): %v", tc.status, err)
		}
		got, err := s.LegacyRunSince(ctx, planID, since)
		if err != nil {
			t.Fatalf("LegacyRunSince(%s): %v", tc.status, err)
		}
		if got != tc.want {
			t.Errorf("LegacyRunSince with a %s row = %v, want %v", tc.status, got, tc.want)
		}
	}
}
