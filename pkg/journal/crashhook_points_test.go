package journal

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

type seamCall struct {
	point, wf, step string
	attempt         int
}

// runTwoSteps runs a two-step workflow in a locked process with both hooks installed.
func runTwoSteps(t *testing.T) (wfID string, seam []seamCall, prod []CrashPoint) {
	t.Helper()
	sandbox.ResetLockForTest()
	t.Cleanup(sandbox.ResetLockForTest)
	if err := sandbox.Lock(sandbox.LockOptions{Token: testToken}); err != nil {
		t.Fatal(err)
	}
	l, x := newLedger(), newExchange()
	def := Definition{Kind: "two", Run: func(c *Ctx, _ json.RawMessage) error {
		for _, s := range []string{"a", "b"} {
			if err := orderStep(c, l, x, s); err != nil {
				return err
			}
		}
		return nil
	}}
	n := newNode(t, dbPath(t), t0, []Definition{def})
	var mu sync.Mutex
	n.hook = func(point, wf, step string, attempt int) {
		mu.Lock()
		seam = append(seam, seamCall{point, wf, step, attempt})
		mu.Unlock()
	}
	if err := n.SetCrashHook(func(p CrashPoint) { mu.Lock(); prod = append(prod, p); mu.Unlock() }); err != nil {
		t.Fatal(err)
	}
	wfID = mustStart(t, n.Engine, "two", nil)
	n.exec(wfID)
	if w := status(t, n, wfID); w.Status != StatusSucceeded {
		t.Fatalf("workflow %+v", w)
	}
	mu.Lock()
	defer mu.Unlock()
	return wfID, seam, prod
}

var fourPoints = []string{CrashBeforeStarted, CrashAfterStarted, CrashAfterFn, CrashAfterOutput}

// The test seam keeps getting the workflow id, the step and the attempt, at the same four
// points in the same order as before the production hook existed.
func TestCallHook_TestSeamStillGetsWorkflowID(t *testing.T) {
	wfID, seam, _ := runTwoSteps(t)
	if len(seam) != 8 {
		t.Fatalf("seam saw %d points, want 8", len(seam))
	}
	for i, s := range seam {
		wantAttempt := 1
		if s.point == CrashBeforeStarted {
			wantAttempt = 0
		}
		if step := []string{"a", "b"}[i/4]; s != (seamCall{fourPoints[i%4], wfID, step, wantAttempt}) {
			t.Errorf("seam call %d = %+v", i, s)
		}
	}
}

// The production hook gets the workflow's kind and step at each of the four points.
func TestCrashHook_ReceivesKindAndStep(t *testing.T) {
	wfID, _, prod := runTwoSteps(t)
	if len(prod) != 8 {
		t.Fatalf("crash hook saw %d points, want 8", len(prod))
	}
	for i, p := range prod {
		wantAttempt := 1
		if p.Point == CrashBeforeStarted {
			wantAttempt = 0
		}
		if step := []string{"a", "b"}[i/4]; p != (CrashPoint{fourPoints[i%4], wfID, "two", step, wantAttempt}) {
			t.Errorf("crash hook call %d = %+v", i, p)
		}
	}
}
