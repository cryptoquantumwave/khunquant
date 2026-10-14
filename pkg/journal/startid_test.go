package journal

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

func startIDDef(runs *atomic.Int32) Definition {
	return Definition{Kind: "once", Run: func(c *Ctx, _ json.RawMessage) error {
		return c.Step("only", nil, func(context.Context, int) (any, error) {
			runs.Add(1)
			return nil, nil
		})
	}}
}

// Starting the same id twice, even from several goroutines, creates one workflow
// and runs it once.
func TestStartWithID_IdempotentAndRunsOnce(t *testing.T) {
	var runs atomic.Int32
	n := newNode(t, dbPath(t), t0, []Definition{startIDDef(&runs)})
	ctx := context.Background()

	var created atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := n.StartWithID(ctx, "once", "ap_ABC", map[string]string{"proposal_id": "ABC"})
			if err != nil {
				t.Error(err)
			}
			if ok {
				created.Add(1)
			}
		}()
	}
	wg.Wait()
	if created.Load() != 1 {
		t.Fatalf("%d callers created the workflow, want exactly 1", created.Load())
	}
	if got := n.Stats().Started; got != 1 {
		t.Fatalf("Started = %d, want 1", got)
	}

	n.exec("ap_ABC")
	if w := status(t, n, "ap_ABC"); w.Status != StatusSucceeded {
		t.Fatalf("status = %s", w.Status)
	}
	// Starting it again after it finished does not run it again.
	if ok, err := n.StartWithID(ctx, "once", "ap_ABC", map[string]string{"proposal_id": "ABC"}); err != nil || ok {
		t.Fatalf("restart of a finished id = (%v, %v), want (false, nil)", ok, err)
	}
	n.exec("ap_ABC")
	if runs.Load() != 1 {
		t.Fatalf("the step ran %d times, want 1", runs.Load())
	}
}

// An id that already stands for another input is an error, not a silent no-op.
func TestStartWithID_DifferentInputOrKindRefused(t *testing.T) {
	var runs atomic.Int32
	n := newNode(t, dbPath(t), t0, []Definition{startIDDef(&runs), {Kind: "other", Run: func(*Ctx, json.RawMessage) error { return nil }}})
	ctx := context.Background()
	if _, err := n.StartWithID(ctx, "once", "ap_X", "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := n.StartWithID(ctx, "once", "ap_X", "b"); !errors.Is(err, ErrIDInUse) {
		t.Fatalf("different input: err = %v, want ErrIDInUse", err)
	}
	if _, err := n.StartWithID(ctx, "other", "ap_X", "a"); !errors.Is(err, ErrIDInUse) {
		t.Fatalf("different kind: err = %v, want ErrIDInUse", err)
	}
	if _, err := n.StartWithID(ctx, "nope", "ap_Y", nil); !errors.Is(err, ErrUnknownKind) {
		t.Fatalf("unknown kind: err = %v", err)
	}
	if _, err := n.StartWithID(ctx, "once", "bad id!", nil); !errors.Is(err, ErrInvalidName) {
		t.Fatalf("bad id: err = %v", err)
	}
}
