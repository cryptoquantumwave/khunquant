package tools

import (
	"context"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral"
)

// TestResize_Decrease_RecordsRealisedLoss: a reduce-only resize feeds the daily loss
// tracker even when the filled leg cannot be recorded.
func TestResize_Decrease_RecordsRealisedLoss(t *testing.T) {
	loss := -100.0
	for _, failSave := range []bool{false, true} {
		tr := swapLossTracker(t)
		store := setupTempDeltaNeutralStore(t)
		installDNFakes(t, &dnFakeSpot{id: "dnfake", baseFree: 10},
			&dnFakeFutures{mockFuturesProvider: &mockFuturesProvider{}, pnl: &loss})
		if failSave {
			failLegSaves(t, filledLegOf(deltaneutral.LegTypeFutures))
		}
		id := seedDNFakePlan(t, store, "resize-loss", "active")
		NewResizeDeltaNeutralPositionTool(leverageOnCfg(), store).
			Execute(context.Background(), map[string]any{"plan_id": float64(id), "delta_notional_usdt": -1000.0, "confirm": true})
		if got := tr.TodayLossUSD(); got != 20 { // 10 of 50 live contracts
			t.Fatalf("failSave=%v: recorded loss = %v, want 20", failSave, got)
		}
		store.Close()
	}
}
