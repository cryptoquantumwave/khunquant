package worldmodel

import (
	"context"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
)

// TestTracks_MatchesWhatApplyIgnores keeps Tracks (which the gateway adapter
// relies on to tell "nothing changed" from "not mine") in step with Apply.
func TestTracks_MatchesWhatApplyIgnores(t *testing.T) {
	kinds := []events.Kind{
		events.KindPerceptionMarketTick, events.KindPerceptionMarketCandleClosed, events.KindPerceptionPortfolioSnapshot,
		events.KindPerceptionFuturesPositions, events.KindPerceptionOrderFilled, events.KindPerceptionOrderPlaced,
		events.KindPerceptionPlanHealth, events.KindPerceptionPlanDCADue, events.KindPerceptionClockSession,
		events.KindPerceptionClockDayStart, events.KindPerceptionExternalTrigger, events.KindPerceptionDevice,
		events.KindPerceptionNewsItem, events.KindPerceptionSourceHealth, events.KindAgentTurnStart,
	}
	for _, k := range kinds {
		s := newMemStore(t, newClock())
		_, _ = s.Apply(context.Background(), events.Event{Kind: k, Time: t0})
		ignored := s.Stats().Ignored == 1
		if ignored == Tracks(k) {
			t.Errorf("%s: Apply ignored it = %v but Tracks = %v", k, ignored, Tracks(k))
		}
	}
}
