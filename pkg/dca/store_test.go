package dca

import (
	"context"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func minimalPlan(name string) *Plan {
	now := time.Now().UTC()
	return &Plan{
		Name:           name,
		Provider:       "bitkub",
		Symbol:         "BTC/THB",
		AmountPerOrder: 100,
		FrequencyExpr:  "0 9 * * 1",
		Timezone:       "UTC",
		Enabled:        true,
		Side:           "buy",
		StartDate:      now,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
}

func minimalExecution(planID int64) *Execution {
	now := time.Now().UTC()
	return &Execution{
		PlanID:      planID,
		ExecutedAt:  now,
		Symbol:      "BTC/THB",
		Provider:    "bitkub",
		AmountQuote: 100,
		Status:      "completed",
		CreatedAt:   now,
	}
}

func TestStore_SaveAndGetPlan(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	plan := minimalPlan("BTCWeekly")
	plan.Account = "main"
	plan.NotifyChannel = "telegram"
	plan.NotifyChatID = "chat-1"
	plan.MaxExecPerPeriod = 3
	plan.ExecPeriod = "day"

	id, err := s.SavePlan(ctx, plan)
	if err != nil {
		t.Fatalf("SavePlan: %v", err)
	}
	if id <= 0 {
		t.Fatal("expected positive plan ID")
	}

	got, err := s.GetPlan(ctx, id)
	if err != nil {
		t.Fatalf("GetPlan: %v", err)
	}
	if got.Name != plan.Name {
		t.Errorf("Name = %q, want %q", got.Name, plan.Name)
	}
	if got.Provider != plan.Provider {
		t.Errorf("Provider = %q, want %q", got.Provider, plan.Provider)
	}
	if got.Symbol != plan.Symbol {
		t.Errorf("Symbol = %q, want %q", got.Symbol, plan.Symbol)
	}
	if got.AmountPerOrder != plan.AmountPerOrder {
		t.Errorf("AmountPerOrder = %g, want %g", got.AmountPerOrder, plan.AmountPerOrder)
	}
	if got.Side != plan.Side {
		t.Errorf("Side = %q, want %q", got.Side, plan.Side)
	}
	if got.MaxExecPerPeriod != plan.MaxExecPerPeriod {
		t.Errorf("MaxExecPerPeriod = %d, want %d", got.MaxExecPerPeriod, plan.MaxExecPerPeriod)
	}
	if got.ExecPeriod != plan.ExecPeriod {
		t.Errorf("ExecPeriod = %q, want %q", got.ExecPeriod, plan.ExecPeriod)
	}
	if got.NotifyChannel != plan.NotifyChannel {
		t.Errorf("NotifyChannel = %q, want %q", got.NotifyChannel, plan.NotifyChannel)
	}
	if got.NotifyChatID != plan.NotifyChatID {
		t.Errorf("NotifyChatID = %q, want %q", got.NotifyChatID, plan.NotifyChatID)
	}
	if !got.Enabled {
		t.Error("expected Enabled=true")
	}
}

func TestStore_UpdatePlan(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	plan := minimalPlan("UpdateTest")
	id, err := s.SavePlan(ctx, plan)
	if err != nil {
		t.Fatalf("SavePlan: %v", err)
	}

	plan.Enabled = false
	plan.Timezone = "Asia/Bangkok"
	end := time.Now().UTC().Add(30 * 24 * time.Hour)
	plan.EndDate = &end
	plan.Side = "sell"

	if err := s.UpdatePlan(ctx, plan); err != nil {
		t.Fatalf("UpdatePlan: %v", err)
	}

	got, err := s.GetPlan(ctx, id)
	if err != nil {
		t.Fatalf("GetPlan after update: %v", err)
	}
	if got.Enabled {
		t.Error("expected Enabled=false after update")
	}
	if got.Timezone != "Asia/Bangkok" {
		t.Errorf("Timezone = %q, want Asia/Bangkok", got.Timezone)
	}
	if got.EndDate == nil {
		t.Fatal("expected EndDate to be set")
	}
	if got.Side != "sell" {
		t.Errorf("Side = %q, want sell", got.Side)
	}
}

func TestStore_ListPlans_All(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	for i, name := range []string{"Plan A", "Plan B", "Plan C"} {
		p := minimalPlan(name)
		p.Enabled = i%2 == 0
		if _, err := s.SavePlan(ctx, p); err != nil {
			t.Fatalf("SavePlan %q: %v", name, err)
		}
	}

	plans, err := s.ListPlans(ctx, QueryFilter{})
	if err != nil {
		t.Fatalf("ListPlans: %v", err)
	}
	if len(plans) != 3 {
		t.Errorf("len(plans) = %d, want 3", len(plans))
	}
}

func TestStore_ListPlans_FilterEnabled(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	enabled := minimalPlan("Enabled")
	enabled.Enabled = true
	if _, err := s.SavePlan(ctx, enabled); err != nil {
		t.Fatalf("SavePlan enabled: %v", err)
	}

	disabled := minimalPlan("Disabled")
	disabled.Enabled = false
	if _, err := s.SavePlan(ctx, disabled); err != nil {
		t.Fatalf("SavePlan disabled: %v", err)
	}

	trueVal := true
	falseVal := false

	got, err := s.ListPlans(ctx, QueryFilter{Enabled: &trueVal})
	if err != nil {
		t.Fatalf("ListPlans(enabled): %v", err)
	}
	if len(got) != 1 || got[0].Name != "Enabled" {
		t.Errorf("filter_enabled=true: got %d plans", len(got))
	}

	got, err = s.ListPlans(ctx, QueryFilter{Enabled: &falseVal})
	if err != nil {
		t.Fatalf("ListPlans(disabled): %v", err)
	}
	if len(got) != 1 || got[0].Name != "Disabled" {
		t.Errorf("filter_enabled=false: got %d plans", len(got))
	}
}

func TestStore_GetPlan_NotFound(t *testing.T) {
	s := newTestStore(t)
	_, err := s.GetPlan(context.Background(), 99999)
	if err == nil {
		t.Fatal("expected error for missing plan, got nil")
	}
}

func TestStore_TriggerRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	plan := minimalPlan("TriggerPlan")
	plan.Trigger = &Trigger{
		Timeframe: "1h",
		Lookback:  100,
		Indicators: []IndicatorSpec{
			{Alias: "rsi14", Kind: "rsi", Params: map[string]any{"period": float64(14)}},
			{Alias: "ema50", Kind: "ema", Params: map[string]any{"period": float64(50)}},
		},
		Expression: "rsi14 < 30 and close > ema50",
	}

	id, err := s.SavePlan(ctx, plan)
	if err != nil {
		t.Fatalf("SavePlan: %v", err)
	}

	got, err := s.GetPlan(ctx, id)
	if err != nil {
		t.Fatalf("GetPlan: %v", err)
	}
	if got.Trigger == nil {
		t.Fatal("expected Trigger to be non-nil after round-trip")
	}
	tr := got.Trigger
	if tr.Timeframe != "1h" {
		t.Errorf("Timeframe = %q, want 1h", tr.Timeframe)
	}
	if tr.Lookback != 100 {
		t.Errorf("Lookback = %d, want 100", tr.Lookback)
	}
	if tr.Expression != "rsi14 < 30 and close > ema50" {
		t.Errorf("Expression = %q", tr.Expression)
	}
	if len(tr.Indicators) != 2 {
		t.Fatalf("len(Indicators) = %d, want 2", len(tr.Indicators))
	}
	if tr.Indicators[0].Alias != "rsi14" || tr.Indicators[0].Kind != "rsi" {
		t.Errorf("Indicators[0] = %+v", tr.Indicators[0])
	}
}

func TestStore_DeletePlan_CascadesExecutions(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	plan := minimalPlan("ToDelete")
	id, err := s.SavePlan(ctx, plan)
	if err != nil {
		t.Fatalf("SavePlan: %v", err)
	}

	exec := minimalExecution(id)
	if _, err := s.SaveExecution(ctx, exec); err != nil {
		t.Fatalf("SaveExecution: %v", err)
	}

	count, _ := s.CountExecutions(ctx, id)
	if count != 1 {
		t.Fatalf("expected 1 execution before delete, got %d", count)
	}

	if err := s.DeletePlan(ctx, id); err != nil {
		t.Fatalf("DeletePlan: %v", err)
	}

	_, err = s.GetPlan(ctx, id)
	if err == nil {
		t.Error("expected error after plan deleted, got nil")
	}

	count, _ = s.CountExecutions(ctx, id)
	if count != 0 {
		t.Errorf("expected 0 executions after plan deleted, got %d", count)
	}
}

func TestStore_SaveAndGetExecution(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	plan := minimalPlan("ExecPlan")
	planID, err := s.SavePlan(ctx, plan)
	if err != nil {
		t.Fatalf("SavePlan: %v", err)
	}

	now := time.Now().UTC().Round(time.Second)
	exec := &Execution{
		PlanID:         planID,
		ExecutedAt:     now,
		Symbol:         "BTC/THB",
		Provider:       "bitkub",
		Account:        "main",
		OrderID:        "order-123",
		AmountQuote:    100.5,
		FilledPrice:    50000.0,
		FilledQuantity: 0.002,
		FeeQuote:       0.25,
		Status:         "completed",
		CreatedAt:      now,
	}

	id, err := s.SaveExecution(ctx, exec)
	if err != nil {
		t.Fatalf("SaveExecution: %v", err)
	}
	if id <= 0 {
		t.Fatal("expected positive execution ID")
	}

	pID := planID
	execs, err := s.GetExecutions(ctx, QueryFilter{PlanID: &pID, Limit: 10})
	if err != nil {
		t.Fatalf("GetExecutions: %v", err)
	}
	if len(execs) != 1 {
		t.Fatalf("len(execs) = %d, want 1", len(execs))
	}
	e := execs[0]
	if e.OrderID != "order-123" {
		t.Errorf("OrderID = %q, want order-123", e.OrderID)
	}
	if e.FilledPrice != 50000.0 {
		t.Errorf("FilledPrice = %g, want 50000", e.FilledPrice)
	}
	if e.Status != "completed" {
		t.Errorf("Status = %q, want completed", e.Status)
	}
}

func TestStore_CountExecutions(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	plan := minimalPlan("CountPlan")
	planID, _ := s.SavePlan(ctx, plan)

	for i := 0; i < 3; i++ {
		e := minimalExecution(planID)
		if _, err := s.SaveExecution(ctx, e); err != nil {
			t.Fatalf("SaveExecution %d: %v", i, err)
		}
	}

	count, err := s.CountExecutions(ctx, planID)
	if err != nil {
		t.Fatalf("CountExecutions: %v", err)
	}
	if count != 3 {
		t.Errorf("count = %d, want 3", count)
	}
}

func TestStore_CountExecutionsInPeriod(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	plan := minimalPlan("GuardrailPlan")
	planID, _ := s.SavePlan(ctx, plan)

	now := time.Now()

	// Execution 1: now (in current hour, day, week)
	exec1 := minimalExecution(planID)
	exec1.ExecutedAt = now
	if _, err := s.SaveExecution(ctx, exec1); err != nil {
		t.Fatalf("SaveExecution 1: %v", err)
	}

	// Execution 2: now - 90 minutes (in current day and week, but NOT current hour)
	exec2 := minimalExecution(planID)
	exec2.ExecutedAt = now.Add(-90 * time.Minute)
	if _, err := s.SaveExecution(ctx, exec2); err != nil {
		t.Fatalf("SaveExecution 2: %v", err)
	}

	// Execution 3: now - 14 days (not in current hour, day, or week)
	exec3 := minimalExecution(planID)
	exec3.ExecutedAt = now.Add(-14 * 24 * time.Hour)
	if _, err := s.SaveExecution(ctx, exec3); err != nil {
		t.Fatalf("SaveExecution 3: %v", err)
	}

	hourCount, err := s.CountExecutionsInPeriod(ctx, planID, "hour")
	if err != nil {
		t.Fatalf("CountExecutionsInPeriod(hour): %v", err)
	}
	if hourCount != 1 {
		t.Errorf("hour count = %d, want 1", hourCount)
	}

	dayCount, err := s.CountExecutionsInPeriod(ctx, planID, "day")
	if err != nil {
		t.Fatalf("CountExecutionsInPeriod(day): %v", err)
	}
	if dayCount != 2 {
		t.Errorf("day count = %d, want 2", dayCount)
	}

	weekCount, err := s.CountExecutionsInPeriod(ctx, planID, "week")
	if err != nil {
		t.Fatalf("CountExecutionsInPeriod(week): %v", err)
	}
	if weekCount != 2 {
		t.Errorf("week count = %d, want 2", weekCount)
	}
}

func TestStore_CountExecutionsInPeriod_Empty(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	plan := minimalPlan("EmptyPeriod")
	planID, _ := s.SavePlan(ctx, plan)

	// period="" returns 0 with no error (no guardrail)
	count, err := s.CountExecutionsInPeriod(ctx, planID, "")
	if err != nil {
		t.Fatalf("unexpected error for empty period: %v", err)
	}
	if count != 0 {
		t.Errorf("count = %d, want 0", count)
	}
}

func TestStore_LastExecution(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	plan := minimalPlan("LastExecPlan")
	planID, _ := s.SavePlan(ctx, plan)

	// nil when no executions
	last, err := s.LastExecution(ctx, planID)
	if err != nil {
		t.Fatalf("LastExecution (empty): %v", err)
	}
	if last != nil {
		t.Error("expected nil when no executions")
	}

	older := minimalExecution(planID)
	older.ExecutedAt = time.Now().UTC().Add(-2 * time.Hour)
	if _, err := s.SaveExecution(ctx, older); err != nil {
		t.Fatalf("SaveExecution older: %v", err)
	}

	newer := minimalExecution(planID)
	newer.ExecutedAt = time.Now().UTC()
	newer.OrderID = "newer-order"
	if _, err := s.SaveExecution(ctx, newer); err != nil {
		t.Fatalf("SaveExecution newer: %v", err)
	}

	last, err = s.LastExecution(ctx, planID)
	if err != nil {
		t.Fatalf("LastExecution: %v", err)
	}
	if last == nil {
		t.Fatal("expected non-nil last execution")
	}
	if last.OrderID != "newer-order" {
		t.Errorf("last execution OrderID = %q, want newer-order", last.OrderID)
	}
}

func TestStore_UpdatePlanStats(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	plan := minimalPlan("StatsPlan")
	planID, _ := s.SavePlan(ctx, plan)

	// First execution: buy 0.1 BTC for 5000 THB → avg cost = 50000
	if err := s.UpdatePlanStats(ctx, planID, 5000, 0.1); err != nil {
		t.Fatalf("UpdatePlanStats 1: %v", err)
	}
	// Second execution: buy 0.1 BTC for 3000 THB → avg cost = (5000+3000)/(0.1+0.1) = 40000
	if err := s.UpdatePlanStats(ctx, planID, 3000, 0.1); err != nil {
		t.Fatalf("UpdatePlanStats 2: %v", err)
	}

	got, err := s.GetPlan(ctx, planID)
	if err != nil {
		t.Fatalf("GetPlan after stats: %v", err)
	}
	if got.TotalInvested != 8000 {
		t.Errorf("TotalInvested = %g, want 8000", got.TotalInvested)
	}
	if got.TotalQuantity != 0.2 {
		t.Errorf("TotalQuantity = %g, want 0.2", got.TotalQuantity)
	}
	if got.AvgCost != 40000 {
		t.Errorf("AvgCost = %g, want 40000", got.AvgCost)
	}
}
