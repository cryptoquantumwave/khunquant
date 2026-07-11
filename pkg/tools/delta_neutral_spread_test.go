package tools

import (
	"testing"
	"time"

	ccxt "github.com/ccxt/ccxt/go/v4"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

func TestGetDeltaNeutralSpreadTool_NameDescriptionParameters(t *testing.T) {
	tool := NewGetDeltaNeutralSpreadTool(config.DefaultConfig())
	if tool.Name() != NameGetDeltaNeutralSpread {
		t.Errorf("Name() = %q, want %q", tool.Name(), NameGetDeltaNeutralSpread)
	}
	if tool.Description() != DescGetDeltaNeutralSpread {
		t.Errorf("Description() mismatch")
	}
	params := tool.Parameters()
	props, ok := params["properties"].(map[string]any)
	if !ok {
		t.Fatal("properties should be a map")
	}
	for _, prop := range []string{"spot_provider", "spot_account", "spot_symbol", "futures_provider", "futures_account", "futures_symbol"} {
		if _, ok := props[prop]; !ok {
			t.Errorf("expected property %q not found", prop)
		}
	}
}

func fundingRatePtr(f float64) *float64 { return &f }
func tsPtr(ms int64) *int64             { return &ms }

func TestComputeWindowAPR_Success(t *testing.T) {
	now := time.Now()
	history := []ccxt.FundingRateHistory{
		{FundingRate: fundingRatePtr(0.0001), Timestamp: tsPtr(now.Add(-1 * time.Hour).UnixMilli())},
		{FundingRate: fundingRatePtr(0.0002), Timestamp: tsPtr(now.Add(-2 * time.Hour).UnixMilli())},
		{FundingRate: fundingRatePtr(0.0003), Timestamp: tsPtr(now.Add(-100 * 24 * time.Hour).UnixMilli())}, // outside window
	}
	got := computeWindowAPR(history, 7*24*time.Hour, 3) // 3 funding periods/day (8h funding)
	if got == nil {
		t.Fatal("expected non-nil APR")
	}
	wantMean := (0.0001 + 0.0002) / 2
	wantAPR := wantMean * 3 * 365 * 100
	if diff := *got - wantAPR; diff > 0.0001 || diff < -0.0001 {
		t.Errorf("expected APR %.6f, got %.6f", wantAPR, *got)
	}
}

func TestComputeWindowAPR_NoDataInWindow(t *testing.T) {
	old := time.Now().Add(-100 * 24 * time.Hour)
	history := []ccxt.FundingRateHistory{
		{FundingRate: fundingRatePtr(0.0001), Timestamp: tsPtr(old.UnixMilli())},
	}
	got := computeWindowAPR(history, 7*24*time.Hour, 3)
	if got != nil {
		t.Errorf("expected nil APR when no points fall in window, got %v", *got)
	}
}

func TestComputeWindowAPR_NilFieldsSkipped(t *testing.T) {
	now := time.Now()
	history := []ccxt.FundingRateHistory{
		{FundingRate: nil, Timestamp: tsPtr(now.UnixMilli())},
		{FundingRate: fundingRatePtr(0.0001), Timestamp: nil},
		{FundingRate: fundingRatePtr(0.0002), Timestamp: tsPtr(now.UnixMilli())},
	}
	got := computeWindowAPR(history, 7*24*time.Hour, 3)
	if got == nil {
		t.Fatal("expected non-nil APR from the one valid point")
	}
	want := 0.0002 * 3 * 365 * 100
	if diff := *got - want; diff > 0.0001 || diff < -0.0001 {
		t.Errorf("expected APR %.6f (only valid point counted), got %.6f", want, *got)
	}
}
