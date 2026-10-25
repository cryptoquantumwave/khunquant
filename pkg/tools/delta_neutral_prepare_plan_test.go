package tools

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral"
)

func saveDraftPlan(t *testing.T, store *deltaneutral.Store, overrides func(*deltaneutral.Plan)) int64 {
	t.Helper()
	plan := &deltaneutral.Plan{
		Name:                "test-prepare",
		Asset:               "BTC",
		Status:              deltaneutral.PlanStatusDraft,
		Mode:                deltaneutral.ExecutionModeApproval,
		SpotProvider:        "binance",
		SpotSymbol:          "BTC/USDT",
		SpotSide:            "buy",
		FuturesProvider:     "binance",
		FuturesSymbol:       "BTC/USDT:USDT",
		FuturesSide:         "short",
		FuturesLeverage:     2,
		FuturesMarginMode:   "cross",
		CapitalUSDT:         10000,
		SpotNotionalUSDT:    6666,
		FuturesNotionalUSDT: 6666,
		ReserveMarginUSDT:   0,
		MonitorInterval:     "5m",
		Enabled:             true,
		RiskPolicy:          deltaneutral.DefaultRiskPolicy(),
		CreatedAt:           time.Now(),
		UpdatedAt:           time.Now(),
	}
	if overrides != nil {
		overrides(plan)
	}
	id, err := store.SavePlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("saveDraftPlan: %v", err)
	}
	return id
}

func TestPrepareDeltaNeutralPlan_HappyPath(t *testing.T) {
	store := setupTempDeltaNeutralStore(t)
	defer store.Close()

	id := saveDraftPlan(t, store, nil)
	tool := NewPrepareDeltaNeutralPlanTool(&config.Config{}, store)

	result := tool.Execute(context.Background(), map[string]any{"plan_id": float64(id)})
	if result.IsError {
		t.Fatalf("expected success, got error: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, "ready ✓") {
		t.Errorf("expected 'ready ✓' in output, got: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, "open_delta_neutral_position") {
		t.Errorf("expected next-step hint in output")
	}

	// Verify status persisted to store
	plan, err := store.GetPlan(context.Background(), id)
	if err != nil {
		t.Fatalf("GetPlan: %v", err)
	}
	if plan.Status != deltaneutral.PlanStatusReady {
		t.Errorf("expected status=ready, got %q", plan.Status)
	}
}

func TestPrepareDeltaNeutralPlan_Idempotent(t *testing.T) {
	store := setupTempDeltaNeutralStore(t)
	defer store.Close()

	id := saveDraftPlan(t, store, nil)
	tool := NewPrepareDeltaNeutralPlanTool(&config.Config{}, store)

	// First call promotes to ready
	r1 := tool.Execute(context.Background(), map[string]any{"plan_id": float64(id)})
	if r1.IsError {
		t.Fatalf("first prepare failed: %s", r1.ForLLM)
	}

	// Second call must succeed (idempotent) with an informational message
	r2 := tool.Execute(context.Background(), map[string]any{"plan_id": float64(id)})
	if r2.IsError {
		t.Errorf("second prepare (idempotent) returned error: %s", r2.ForLLM)
	}
	if !strings.Contains(r2.ForLLM, "already in 'ready' status") {
		t.Errorf("expected idempotent message, got: %s", r2.ForLLM)
	}
}

func TestPrepareDeltaNeutralPlan_RejectNonDraft(t *testing.T) {
	store := setupTempDeltaNeutralStore(t)
	defer store.Close()

	id := saveDraftPlan(t, store, func(p *deltaneutral.Plan) {
		p.Status = deltaneutral.PlanStatusActive
	})
	tool := NewPrepareDeltaNeutralPlanTool(&config.Config{}, store)

	result := tool.Execute(context.Background(), map[string]any{"plan_id": float64(id)})
	if !result.IsError {
		t.Errorf("expected error for active plan, got success")
	}
	if !strings.Contains(result.ForLLM, "only draft or failed plans") {
		t.Errorf("expected 'only draft or failed plans' in error, got: %s", result.ForLLM)
	}
}

func TestPrepareDeltaNeutralPlan_FailsOnBadLeverage(t *testing.T) {
	store := setupTempDeltaNeutralStore(t)
	defer store.Close()

	id := saveDraftPlan(t, store, func(p *deltaneutral.Plan) {
		p.FuturesLeverage = 0
	})
	tool := NewPrepareDeltaNeutralPlanTool(&config.Config{}, store)

	result := tool.Execute(context.Background(), map[string]any{"plan_id": float64(id)})
	if !result.IsError {
		t.Errorf("expected error for leverage=0, got success")
	}
	if !strings.Contains(result.ForLLM, "Failed checks") {
		t.Errorf("expected 'Failed checks' in output, got: %s", result.ForLLM)
	}
}

func TestPrepareDeltaNeutralPlan_FailsOnSpotOnlyFuturesProvider(t *testing.T) {
	store := setupTempDeltaNeutralStore(t)
	defer store.Close()

	id := saveDraftPlan(t, store, func(p *deltaneutral.Plan) {
		p.FuturesProvider = "bitkub"
	})
	tool := NewPrepareDeltaNeutralPlanTool(&config.Config{}, store)

	result := tool.Execute(context.Background(), map[string]any{"plan_id": float64(id)})
	if !result.IsError {
		t.Errorf("expected error for bitkub futures provider, got success")
	}
	if !strings.Contains(result.ForLLM, "Futures provider") {
		t.Errorf("expected 'Futures provider' check in output, got: %s", result.ForLLM)
	}
}

func TestPrepareDeltaNeutralPlan_FailsOnBadSymbolFormat(t *testing.T) {
	store := setupTempDeltaNeutralStore(t)
	defer store.Close()

	id := saveDraftPlan(t, store, func(p *deltaneutral.Plan) {
		p.FuturesSymbol = "BTCUSDT" // missing ":"
	})
	tool := NewPrepareDeltaNeutralPlanTool(&config.Config{}, store)

	result := tool.Execute(context.Background(), map[string]any{"plan_id": float64(id)})
	if !result.IsError {
		t.Errorf("expected error for bad futures symbol, got success")
	}
	if !strings.Contains(result.ForLLM, "Futures symbol format") {
		t.Errorf("expected symbol format check in output, got: %s", result.ForLLM)
	}
}

func TestPrepareDeltaNeutralPlan_MissingPlanID(t *testing.T) {
	store := setupTempDeltaNeutralStore(t)
	defer store.Close()

	tool := NewPrepareDeltaNeutralPlanTool(&config.Config{}, store)
	result := tool.Execute(context.Background(), map[string]any{})
	if !result.IsError {
		t.Errorf("expected error for missing plan_id")
	}
}

func TestPrepareDeltaNeutralPlan_CrossExchangeWarning(t *testing.T) {
	store := setupTempDeltaNeutralStore(t)
	defer store.Close()

	id := saveDraftPlan(t, store, func(p *deltaneutral.Plan) {
		p.SpotProvider = "binance"
		p.FuturesProvider = "okx"
		p.CrossExchange = true
	})
	tool := NewPrepareDeltaNeutralPlanTool(&config.Config{}, store)

	result := tool.Execute(context.Background(), map[string]any{"plan_id": float64(id)})
	if result.IsError {
		t.Fatalf("expected success for cross-exchange plan, got: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, "Cross-exchange") {
		t.Errorf("expected cross-exchange warning in output, got: %s", result.ForLLM)
	}
}
