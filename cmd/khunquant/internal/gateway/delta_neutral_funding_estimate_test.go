package gateway

import (
	"context"
	"math"
	"testing"
	"time"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// monitorFundingFuturesProvider is monitorFuturesProvider plus a real funding rate.
type monitorFundingFuturesProvider struct {
	*monitorFuturesProvider
	rate float64
}

func (m *monitorFundingFuturesProvider) FetchFuturesFundingRate(_ context.Context, _ string) (ccxt.FundingRate, error) {
	rate := m.rate
	ts := float64(time.Now().Add(time.Hour).UnixMilli())
	return ccxt.FundingRate{FundingRate: &rate, FundingTimestamp: &ts}, nil
}

// TestMonitorEstimatedNextFunding_UsesContractSize: Contracts is a contract COUNT, so the
// estimated next funding payment is notional x rate with notional = contracts x contract size
// x mark. 136 contracts of 10 CHZ at 0.033 is 44.88 USDT; at a 0.001 rate the payment is
// 0.04488. The old contracts x mark x rate gave 0.004488 (10x too small).
func TestMonitorEstimatedNextFunding_UsesContractSize(t *testing.T) {
	store := newMonitorStore(t)
	id, jobName := seedMonitorPlan(t, store, "mockspot")

	fut := &monitorFundingFuturesProvider{
		monitorFuturesProvider: &monitorFuturesProvider{contracts: 136, markPrice: 0.033, contractSize: 10.0},
		rate:                   0.001,
	}
	broker.RegisterFactory("mockfutures", func(_ *config.Config) (broker.Provider, error) {
		return fut, nil
	})
	t.Cleanup(func() { broker.RegisterFactory("mockfutures", nil) })

	p, err := store.GetPlan(context.Background(), id)
	if err != nil {
		t.Fatalf("GetPlan: %v", err)
	}
	p.FuturesProvider = "mockfutures"
	if err := store.UpdatePlan(context.Background(), p); err != nil {
		t.Fatalf("UpdatePlan: %v", err)
	}

	spot := &monitorSpotProvider{
		tickerPrice: 0.033,
		balances:    []broker.Balance{{Asset: "CHZ", Free: 1350.0}},
		earnErr:     errFakeEarnUnavailable,
	}

	snap := runMonitor(t, store, id, jobName, spot)
	if snap == nil {
		t.Fatal("expected a snapshot to be saved")
	}
	want := 136 * 10 * 0.033 * 0.001
	if math.Abs(snap.EstimatedNextFundingUSDT-want) > 1e-9 {
		t.Errorf("EstimatedNextFundingUSDT = %.6f, want %.6f (contracts x contract size x mark x rate)", snap.EstimatedNextFundingUSDT, want)
	}
}
