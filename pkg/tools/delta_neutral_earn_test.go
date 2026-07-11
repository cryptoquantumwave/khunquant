package tools

import (
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

func TestGetDeltaNeutralEarnTool_NameDescriptionParameters(t *testing.T) {
	tool := NewGetDeltaNeutralEarnTool(config.DefaultConfig())
	if tool.Name() != NameGetDeltaNeutralEarn {
		t.Errorf("Name() = %q, want %q", tool.Name(), NameGetDeltaNeutralEarn)
	}
	if tool.Description() != DescGetDeltaNeutralEarn {
		t.Errorf("Description() mismatch")
	}
	params := tool.Parameters()
	props, ok := params["properties"].(map[string]any)
	if !ok {
		t.Fatal("properties should be a map")
	}
	for _, prop := range []string{"provider", "account", "asset"} {
		if _, ok := props[prop]; !ok {
			t.Errorf("expected property %q not found", prop)
		}
	}
}

func TestEarnMinMaxPct(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	points := []broker.EarnRatePoint{
		{Rate: 0.05, Timestamp: now.Add(-10 * 24 * time.Hour).UnixMilli()},
		{Rate: 0.08, Timestamp: now.Add(-5 * 24 * time.Hour).UnixMilli()},
		{Rate: 0.03, Timestamp: now.Add(-1 * 24 * time.Hour).UnixMilli()},
		{Rate: 0.99, Timestamp: now.Add(-400 * 24 * time.Hour).UnixMilli()}, // outside 90d window
	}
	lo, hi := earnMinMaxPct(points, 90*24*time.Hour, now)
	if lo != 3 {
		t.Errorf("expected lo=3 (0.03*100), got %v", lo)
	}
	if hi != 8 {
		t.Errorf("expected hi=8 (0.08*100), got %v", hi)
	}
}

func TestEarnMinMaxPct_EmptyWindow(t *testing.T) {
	now := time.Now()
	lo, hi := earnMinMaxPct(nil, time.Hour, now)
	if lo != 0 || hi != 0 {
		t.Errorf("expected zero lo/hi for empty points, got lo=%v hi=%v", lo, hi)
	}
}

func TestFormatEarnStats_StakingDefi(t *testing.T) {
	out := formatEarnStats("binance", "ZEC", 5.25, "staking-defi", nil)
	if !strings.Contains(out, "staking-defi product") {
		t.Errorf("expected staking-defi note, got: %s", out)
	}
	if !strings.Contains(out, "5.2500%") {
		t.Errorf("expected current APY formatted, got: %s", out)
	}
}

func TestFormatEarnStats_NoHistory(t *testing.T) {
	out := formatEarnStats("okx", "BTC", 4.0, "flexible", nil)
	if !strings.Contains(out, "no rate history available") {
		t.Errorf("expected no-history note, got: %s", out)
	}
}

func TestFormatEarnStats_WithHistory(t *testing.T) {
	now := time.Now()
	var history []broker.EarnRatePoint
	for i := range 15 {
		history = append(history, broker.EarnRatePoint{
			Rate:      0.04 + float64(i)*0.001,
			Timestamp: now.Add(-time.Duration(i) * 24 * time.Hour).UnixMilli(),
		})
	}
	out := formatEarnStats("binance", "ETH", 6.5, "flexible", history)
	if !strings.Contains(out, "3M avg") || !strings.Contains(out, "6M avg") || !strings.Contains(out, "12M avg") {
		t.Errorf("expected all trailing windows, got: %s", out)
	}
	if !strings.Contains(out, "Recent Records") {
		t.Errorf("expected recent records section, got: %s", out)
	}
}

func TestFormatEarnStats_WithHistory_NoDataInWindow(t *testing.T) {
	// A single point far outside every trailing window (3M/6M/12M) so each
	// window reports "(no data)" while the recent-records section still runs.
	old := time.Now().Add(-400 * 24 * time.Hour)
	history := []broker.EarnRatePoint{{Rate: 0.05, Timestamp: old.UnixMilli()}}
	out := formatEarnStats("okx", "SOL", 2.0, "flexible", history)
	if !strings.Contains(out, "(no data)") {
		t.Errorf("expected no-data window note, got: %s", out)
	}
}
