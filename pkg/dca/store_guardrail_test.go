package dca

import (
	"context"
	"testing"
	"time"
)

// A pending or unknown execution may have left a live order on the exchange,
// so it counts toward the per-period guardrail; a failed one does not.
func TestStore_CountExecutionsInPeriod_CountsPendingAndUnknown(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	planID, _ := s.SavePlan(ctx, minimalPlan("GuardrailPending"))
	for _, status := range []string{"completed", "pending", "unknown", "failed"} {
		e := minimalExecution(planID)
		e.ExecutedAt = time.Now()
		e.Status = status
		if _, err := s.SaveExecution(ctx, e); err != nil {
			t.Fatalf("SaveExecution(%s): %v", status, err)
		}
	}

	got, err := s.CountExecutionsInPeriod(ctx, planID, "hour")
	if err != nil {
		t.Fatalf("CountExecutionsInPeriod: %v", err)
	}
	if got != 3 {
		t.Errorf("count = %d, want 3 (completed + pending + unknown, not failed)", got)
	}
}
