package engine

import (
	"context"
	"sync"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/perceptiontest"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// maybeEmpty is a Diff that says, through Empty, whether anything changed.
type maybeEmpty struct{ empty bool }

func (d maybeEmpty) Empty() bool { return d.empty }

type diffWM struct{ diff Diff }

func (w diffWM) Apply(context.Context, events.Event) (Diff, error) { return w.diff, nil }
func (diffWM) View() View                                          { return nil }

type evalFunc func(context.Context, View, Diff) ([]proactive.Candidate, error)

func (f evalFunc) Evaluate(ctx context.Context, v View, d Diff) ([]proactive.Candidate, error) {
	return f(ctx, v, d)
}

// A Diff that reports Empty() is evaluated and not published on the bus; one that
// reports a change, or has no Empty method, is published, as before.
func TestEngine_EmptyDiffIsEvaluatedButNotPublished(t *testing.T) {
	for name, tc := range map[string]struct {
		diff    Diff
		publish bool
		empties uint64
	}{
		"empty":           {maybeEmpty{true}, false, 1},
		"changed":         {maybeEmpty{false}, true, 0},
		"no Empty method": {42, true, 0},
	} {
		t.Run(name, func(t *testing.T) {
			bus := events.NewBus()
			t.Cleanup(func() { _ = bus.Close() })
			_, ch, err := bus.Channel().SubscribeChan(context.Background(), events.SubscribeOptions{Buffer: 100})
			if err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			evals := 0
			ev := evalFunc(func(context.Context, View, Diff) ([]proactive.Candidate, error) {
				mu.Lock()
				evals++
				mu.Unlock()
				return nil, nil
			})
			s := start(t, Options{WorldModel: diffWM{tc.diff}, Evaluator: ev, Bus: bus, Clock: perceptiontest.NewClock(noon)})
			s.Emitter().Emit(tickEvent("X", 1))
			waitFor(t, "the event", func() bool { return s.Stats().Processed == 1 })
			mu.Lock()
			n := evals
			mu.Unlock()
			if n != 1 {
				t.Fatalf("evaluator called %d times, want 1", n)
			}
			published := false
			for len(ch) > 0 {
				if (<-ch).Kind == events.KindWorldModelDiff {
					published = true
				}
			}
			if published != tc.publish || s.Stats().EmptyDiffs != tc.empties {
				t.Fatalf("published=%v (want %v), empty diffs counted %d", published, tc.publish, s.Stats().EmptyDiffs)
			}
		})
	}
}
