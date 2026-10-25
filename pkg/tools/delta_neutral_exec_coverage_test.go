package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

// leverageOnCfg returns a config with futures leverage enabled so the execution
// tools pass their first safety gate and reach the dry-run / validation paths.
func leverageOnCfg() *config.Config {
	return &config.Config{TradingRisk: config.TradingRiskConfig{AllowLeverage: true}}
}

// TestOpenDeltaNeutralPosition_Coverage exercises the gated + dry-run paths of the
// open tool without placing any live orders.
func TestOpenDeltaNeutralPosition_Coverage(t *testing.T) {
	resetRateLimiter(t)
	store := setupTempDeltaNeutralStore(t)
	defer store.Close()

	tool := NewOpenDeltaNeutralPositionTool(leverageOnCfg(), store)
	if tool.Name() != NameOpenDeltaNeutralPosition {
		t.Fatalf("unexpected name %q", tool.Name())
	}
	if tool.Description() == "" || tool.Parameters() == nil {
		t.Fatal("description/parameters must be populated")
	}

	// Missing plan_id.
	if res := tool.Execute(context.Background(), map[string]any{}); !res.IsError {
		t.Fatal("expected error for missing plan_id")
	}
	// Unknown plan.
	if res := tool.Execute(context.Background(), map[string]any{"plan_id": 9999.0}); !res.IsError {
		t.Fatal("expected error for unknown plan")
	}

	// Draft plan -> refused, even as a dry-run: only a ready plan opens.
	draftID := seedDNPlan(t, store, "open-draft", "draft")
	if res := tool.Execute(context.Background(), map[string]any{"plan_id": float64(draftID)}); !res.IsError ||
		!strings.Contains(res.ForLLM, "prepare_delta_neutral_plan") {
		t.Fatalf("draft plan must be refused with a pointer to prepare: %+v", res)
	}

	// Ready plan, dry-run (confirm=false) → execution review, no orders placed.
	readyID := seedDNPlan(t, store, "open-ready", "ready")
	res := tool.Execute(context.Background(), map[string]any{"plan_id": float64(readyID), "confirm": false})
	if res.IsError {
		t.Fatalf("dry-run open unexpectedly errored: %v", res.ForLLM)
	}
	if !strings.Contains(strings.ToLower(res.ForUser), "confirm=true") {
		t.Fatalf("expected dry-run review prompting confirm=true:\n%s", res.ForUser)
	}
	// No execution row should have been created by a dry-run.
	if execs, _ := store.ListExecutions(context.Background(), readyID, 10, 0); len(execs) != 0 {
		t.Fatalf("dry-run must not create an execution row, got %d", len(execs))
	}

	if execs, _ := store.ListExecutions(context.Background(), draftID, 10, 0); len(execs) != 0 {
		t.Fatalf("draft dry-run must not create an execution row, got %d", len(execs))
	}
}

// TestOpenDeltaNeutralPosition_LeverageGate verifies the leverage opt-in gate blocks
// opening when AllowLeverage is false.
func TestOpenDeltaNeutralPosition_LeverageGate(t *testing.T) {
	resetRateLimiter(t)
	store := setupTempDeltaNeutralStore(t)
	defer store.Close()
	readyID := seedDNPlan(t, store, "open-lev-gate", "ready")

	tool := NewOpenDeltaNeutralPositionTool(&config.Config{}, store) // AllowLeverage=false
	res := tool.Execute(context.Background(), map[string]any{"plan_id": float64(readyID), "confirm": true})
	if !res.IsError {
		t.Fatal("expected leverage gate to block opening when AllowLeverage=false")
	}
}

// TestUnwindDeltaNeutralPosition_Coverage exercises the gated + dry-run paths of unwind.
func TestUnwindDeltaNeutralPosition_Coverage(t *testing.T) {
	resetRateLimiter(t)
	store := setupTempDeltaNeutralStore(t)
	defer store.Close()

	tool := NewUnwindDeltaNeutralPositionTool(leverageOnCfg(), store)
	if tool.Name() != NameUnwindDeltaNeutralPosition {
		t.Fatalf("unexpected name %q", tool.Name())
	}
	if tool.Description() == "" || tool.Parameters() == nil {
		t.Fatal("description/parameters must be populated")
	}

	if res := tool.Execute(context.Background(), map[string]any{}); !res.IsError {
		t.Fatal("expected error for missing plan_id")
	}

	// Draft plan cannot be unwound (only active/recovery_required).
	draftID := seedDNPlan(t, store, "unwind-draft", "draft")
	if res := tool.Execute(context.Background(), map[string]any{"plan_id": float64(draftID)}); !res.IsError {
		t.Fatal("expected rejection of non-active/recovery plan")
	}

	// Active plan, dry-run → closure review, no orders.
	activeID := seedDNPlan(t, store, "unwind-active", "active")
	res := tool.Execute(context.Background(), map[string]any{"plan_id": float64(activeID), "confirm": false})
	if res.IsError {
		t.Fatalf("dry-run unwind unexpectedly errored: %v", res.ForLLM)
	}
	if !strings.Contains(strings.ToLower(res.ForUser), "confirm=true") {
		t.Fatalf("expected dry-run closure review:\n%s", res.ForUser)
	}

	// recovery_required is also unwindable (dry-run).
	recID := seedDNPlan(t, store, "unwind-recovery", "recovery_required")
	if res := tool.Execute(context.Background(), map[string]any{"plan_id": float64(recID)}); res.IsError {
		t.Fatalf("recovery_required plan should be unwindable (dry-run): %v", res.ForLLM)
	}
}

// TestResizeDeltaNeutralPosition_Coverage exercises validation, guards, and the
// dry-run review of resize using delta_notional_usdt (no live position fetch).
func TestResizeDeltaNeutralPosition_Coverage(t *testing.T) {
	resetRateLimiter(t)
	store := setupTempDeltaNeutralStore(t)
	defer store.Close()

	tool := NewResizeDeltaNeutralPositionTool(leverageOnCfg(), store)
	if tool.Name() != NameResizeDeltaNeutralPosition {
		t.Fatalf("unexpected name %q", tool.Name())
	}
	if tool.Description() == "" || tool.Parameters() == nil {
		t.Fatal("description/parameters must be populated")
	}

	// Missing plan_id.
	if res := tool.Execute(context.Background(), map[string]any{"delta_notional_usdt": 100.0}); !res.IsError {
		t.Fatal("expected error for missing plan_id")
	}

	activeID := seedDNPlan(t, store, "resize-active", "active") // notional 5000/5000

	// Neither delta param → error.
	if res := tool.Execute(context.Background(), map[string]any{"plan_id": float64(activeID)}); !res.IsError {
		t.Fatal("expected error when neither delta param provided")
	}
	// Both delta params → error.
	if res := tool.Execute(context.Background(), map[string]any{
		"plan_id": float64(activeID), "delta_pct": 10.0, "delta_notional_usdt": 100.0,
	}); !res.IsError {
		t.Fatal("expected error when both delta params provided")
	}

	// Non-active plan rejected.
	draftID := seedDNPlan(t, store, "resize-draft", "draft")
	if res := tool.Execute(context.Background(), map[string]any{
		"plan_id": float64(draftID), "delta_notional_usdt": 100.0,
	}); !res.IsError {
		t.Fatal("expected rejection of non-active plan")
	}

	// Over-decrease guard (decrease larger than current notional).
	if res := tool.Execute(context.Background(), map[string]any{
		"plan_id": float64(activeID), "delta_notional_usdt": -999999.0,
	}); !res.IsError {
		t.Fatal("expected over-decrease guard to reject")
	}

	// Increase dry-run → review with new (equal) notionals, no orders.
	res := tool.Execute(context.Background(), map[string]any{
		"plan_id": float64(activeID), "delta_notional_usdt": 1000.0, "confirm": false,
	})
	if res.IsError {
		t.Fatalf("dry-run resize unexpectedly errored: %v", res.ForLLM)
	}
	if !strings.Contains(strings.ToLower(res.ForUser), "confirm=true") || !strings.Contains(res.ForUser, "increase") {
		t.Fatalf("expected increase dry-run review:\n%s", res.ForUser)
	}
	if execs, _ := store.ListExecutions(context.Background(), activeID, 10, 0); len(execs) != 0 {
		t.Fatalf("dry-run must not create an execution row, got %d", len(execs))
	}

	// Decrease dry-run → review labels "decrease".
	res2 := tool.Execute(context.Background(), map[string]any{
		"plan_id": float64(activeID), "delta_notional_usdt": -1000.0, "confirm": false,
	})
	if res2.IsError || !strings.Contains(res2.ForUser, "decrease") {
		t.Fatalf("expected decrease dry-run review:\n%s", res2.ForUser)
	}
}
