package tools

import (
	"context"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/orderfeed"
)

// dnUnits maps each event's origin+unit pair, to check that a futures leg is
// reported in contracts and a spot leg in base units.
func dnUnits(evs []orderfeed.OrderEvent) map[string]string {
	out := map[string]string{}
	for _, e := range evs {
		out[e.Origin+"/"+e.Symbol] = e.AmountUnit
	}
	return out
}

func TestOrderFeed_DeltaNeutralLegsArePublished(t *testing.T) {
	cases := []struct {
		name   string
		origin string
		run    func(t *testing.T, store *deltaneutral.Store) *ToolResult
	}{
		{"open", NameOpenDeltaNeutralPosition, func(t *testing.T, store *deltaneutral.Store) *ToolResult {
			id := seedDNFakePlan(t, store, "feed-open", "ready")
			return NewOpenDeltaNeutralPositionTool(leverageOnCfg(), store).
				Execute(context.Background(), map[string]any{"plan_id": float64(id), "confirm": true})
		}},
		{"unwind", NameUnwindDeltaNeutralPosition, func(t *testing.T, store *deltaneutral.Store) *ToolResult {
			id := seedDNFakePlan(t, store, "feed-unwind", "active")
			seedDNOpenLegs(t, store, id, deltaneutral.ExecutionStateBothLegsFilled, 10, 10)
			return NewUnwindDeltaNeutralPositionTool(leverageOnCfg(), store).
				Execute(context.Background(), map[string]any{"plan_id": float64(id), "confirm": true})
		}},
		{"resize", NameResizeDeltaNeutralPosition, func(t *testing.T, store *deltaneutral.Store) *ToolResult {
			id := seedDNFakePlan(t, store, "feed-resize", "active")
			return NewResizeDeltaNeutralPositionTool(leverageOnCfg(), store).
				Execute(context.Background(), map[string]any{"plan_id": float64(id), "delta_notional_usdt": 1000.0, "confirm": true})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := setupTempDeltaNeutralStore(t)
			defer store.Close()
			installDNFakes(t, &dnFakeSpot{id: "dnfake", baseFree: 10}, &dnFakeFutures{mockFuturesProvider: &mockFuturesProvider{}})
			feed := recordFeed(t)

			if res := tc.run(t, store); res.IsError {
				t.Fatalf("%s failed: %s", tc.name, res.ForLLM)
			}
			evs := feed.wait(t, 2)
			if len(evs) != 2 {
				t.Fatalf("%d events for a two-leg %s, want 2", len(evs), tc.name)
			}
			units := dnUnits(evs)
			if units[tc.origin+"/BTC/USDT:USDT"] != orderfeed.UnitContracts || units[tc.origin+"/BTC/USDT"] != orderfeed.UnitBase {
				t.Fatalf("legs reported as %v", units)
			}
			for _, e := range evs {
				if e.Provider != "dnfake" || e.Paper || e.Amount <= 0 {
					t.Errorf("event = %+v", e)
				}
			}
		})
	}
}

func TestOrderFeed_DeltaNeutralFailedFirstLegPublishesNothing(t *testing.T) {
	store := setupTempDeltaNeutralStore(t)
	defer store.Close()
	installDNFakes(t, &dnFakeSpot{id: "dnfake", baseFree: 10}, &dnFakeFutures{mockFuturesProvider: &mockFuturesProvider{}, orderErr: true})
	feed := recordFeed(t)
	id := seedDNFakePlan(t, store, "feed-firstfail", "ready")

	res := NewOpenDeltaNeutralPositionTool(leverageOnCfg(), store).
		Execute(context.Background(), map[string]any{"plan_id": float64(id), "confirm": true})
	if !res.IsError {
		t.Fatal("expected the failure")
	}
	feed.settle()
	if n := len(feed.events()); n != 0 {
		t.Fatalf("%d events for orders that were never placed", n)
	}
}
