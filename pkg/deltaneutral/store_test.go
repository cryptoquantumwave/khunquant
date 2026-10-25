package deltaneutral

import (
	"context"
	"testing"
	"time"
)

func TestNewStore(t *testing.T) {
	tempDir := t.TempDir()
	store, err := NewStore(tempDir)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	defer store.Close()

	if store == nil {
		t.Fatal("NewStore returned nil store")
	}

	// Test idempotency: re-open should succeed
	store2, err := NewStore(tempDir)
	if err != nil {
		t.Fatalf("NewStore second open failed: %v", err)
	}
	defer store2.Close()
}

func TestSavePlan(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()

	plan := &Plan{
		Name:                     "test-plan",
		Asset:                    "ETH",
		Status:                   PlanStatusDraft,
		Mode:                     ExecutionModeApproval,
		SpotProvider:             "binance",
		SpotAccount:              "spot1",
		SpotSymbol:               "ETHUSDT",
		SpotSide:                 "buy",
		FuturesProvider:          "binance",
		FuturesAccount:           "futures1",
		FuturesSymbol:            "ETHUSDT",
		FuturesSide:              "short",
		FuturesMarginMode:        "cross",
		FuturesLeverage:          1,
		CapitalUSDT:              10000,
		SpotNotionalUSDT:         5000,
		FuturesNotionalUSDT:      5000,
		ReserveMarginUSDT:        500,
		MonitorInterval:          "5m",
		Enabled:                  true,
		EntryRules:               EntryRules{MinFundingRate: 0.001, MaxSlippageBps: 20},
		ExitRules:                ExitRules{ProfitTargetUSDT: 100, MaxDrawdownUSDT: 50},
		RiskPolicy:               DefaultRiskPolicy(),
		EstimatedEntryCostUSDT:   50,
		EstimatedExitCostUSDT:    50,
		ExpectedDailyFundingUSDT: 10,
		BreakevenDays:            5,
		CrossExchange:            false,
		NotifyChannel:            "telegram",
		NotifyChatID:             "123456",
		CreatedAt:                time.Now(),
		UpdatedAt:                time.Now(),
	}

	id, err := store.SavePlan(ctx, plan)
	if err != nil {
		t.Fatalf("SavePlan failed: %v", err)
	}
	if id <= 0 {
		t.Fatalf("SavePlan returned invalid ID: %d", id)
	}
	if plan.ID != id {
		t.Fatalf("SavePlan did not set plan.ID; got %d, expected %d", plan.ID, id)
	}
}

func TestGetPlan(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	plan := createTestPlan(t, "test-get-plan")

	id, err := store.SavePlan(ctx, plan)
	if err != nil {
		t.Fatalf("SavePlan failed: %v", err)
	}

	retrieved, err := store.GetPlan(ctx, id)
	if err != nil {
		t.Fatalf("GetPlan failed: %v", err)
	}

	// Verify all fields round-trip
	if retrieved.Name != plan.Name {
		t.Errorf("Name mismatch: %s vs %s", retrieved.Name, plan.Name)
	}
	if retrieved.Asset != plan.Asset {
		t.Errorf("Asset mismatch: %s vs %s", retrieved.Asset, plan.Asset)
	}
	if retrieved.Status != plan.Status {
		t.Errorf("Status mismatch: %s vs %s", retrieved.Status, plan.Status)
	}
	if retrieved.Mode != plan.Mode {
		t.Errorf("Mode mismatch: %s vs %s", retrieved.Mode, plan.Mode)
	}
	if retrieved.SpotProvider != plan.SpotProvider {
		t.Errorf("SpotProvider mismatch")
	}
	if retrieved.CapitalUSDT != plan.CapitalUSDT {
		t.Errorf("CapitalUSDT mismatch: %f vs %f", retrieved.CapitalUSDT, plan.CapitalUSDT)
	}
	if retrieved.Enabled != plan.Enabled {
		t.Errorf("Enabled mismatch")
	}
	if retrieved.EntryRules.MinFundingRate != plan.EntryRules.MinFundingRate {
		t.Errorf("EntryRules.MinFundingRate mismatch")
	}
	if retrieved.ExitRules.ProfitTargetUSDT != plan.ExitRules.ProfitTargetUSDT {
		t.Errorf("ExitRules.ProfitTargetUSDT mismatch")
	}
	if retrieved.RiskPolicy.MinLiquidationDistancePct != plan.RiskPolicy.MinLiquidationDistancePct {
		t.Errorf("RiskPolicy mismatch")
	}
}

func TestListPlans(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()

	// Create multiple plans
	plan1 := createTestPlan(t, "eth-plan")
	plan1.Asset = "ETH"
	plan1.Status = PlanStatusDraft
	plan1.Enabled = true

	plan2 := createTestPlan(t, "btc-plan")
	plan2.Asset = "BTC"
	plan2.Status = PlanStatusActive
	plan2.Enabled = true

	plan3 := createTestPlan(t, "disabled-plan")
	plan3.Asset = "ETH"
	plan3.Status = PlanStatusDraft
	plan3.Enabled = false

	store.SavePlan(ctx, plan1)
	store.SavePlan(ctx, plan2)
	store.SavePlan(ctx, plan3)

	// List all
	all, err := store.ListPlans(ctx, QueryFilter{})
	if err != nil {
		t.Fatalf("ListPlans failed: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("Expected 3 plans, got %d", len(all))
	}

	// Filter by status
	enabled := true
	actives, err := store.ListPlans(ctx, QueryFilter{Enabled: &enabled})
	if err != nil {
		t.Fatalf("ListPlans with Enabled filter failed: %v", err)
	}
	if len(actives) != 2 {
		t.Errorf("Expected 2 enabled plans, got %d", len(actives))
	}

	// Filter by asset
	ethPlans, err := store.ListPlans(ctx, QueryFilter{Asset: "ETH"})
	if err != nil {
		t.Fatalf("ListPlans with Asset filter failed: %v", err)
	}
	if len(ethPlans) != 2 { // plan1 (enabled) + plan3 (disabled)
		t.Errorf("Expected 2 ETH plans, got %d", len(ethPlans))
	}

	// Filter by status
	status := PlanStatusActive
	activeStatuses, err := store.ListPlans(ctx, QueryFilter{Status: &status})
	if err != nil {
		t.Fatalf("ListPlans with Status filter failed: %v", err)
	}
	if len(activeStatuses) != 1 {
		t.Errorf("Expected 1 active plan, got %d", len(activeStatuses))
	}
}

func TestUpdatePlan(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	plan := createTestPlan(t, "test-update")
	id, err := store.SavePlan(ctx, plan)
	if err != nil {
		t.Fatalf("SavePlan failed: %v", err)
	}

	// Update fields
	plan.ID = id
	plan.Status = PlanStatusActive
	plan.CapitalUSDT = 20000
	plan.Enabled = false

	err = store.UpdatePlan(ctx, plan)
	if err != nil {
		t.Fatalf("UpdatePlan failed: %v", err)
	}

	retrieved, err := store.GetPlan(ctx, id)
	if err != nil {
		t.Fatalf("GetPlan failed: %v", err)
	}
	if retrieved.Status != PlanStatusActive {
		t.Errorf("Status not updated")
	}
	if retrieved.CapitalUSDT != 20000 {
		t.Errorf("CapitalUSDT not updated")
	}
	if retrieved.Enabled != false {
		t.Errorf("Enabled not updated")
	}
}

func TestUpdatePlanStatus(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	plan := createTestPlan(t, "test-status")
	id, err := store.SavePlan(ctx, plan)
	if err != nil {
		t.Fatalf("SavePlan failed: %v", err)
	}

	err = store.UpdatePlanStatus(ctx, id, PlanStatusActive)
	if err != nil {
		t.Fatalf("UpdatePlanStatus failed: %v", err)
	}

	retrieved, err := store.GetPlan(ctx, id)
	if err != nil {
		t.Fatalf("GetPlan failed: %v", err)
	}
	if retrieved.Status != PlanStatusActive {
		t.Errorf("Status not updated correctly")
	}
}

func TestSetCronJobID(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	plan := createTestPlan(t, "test-cron")
	id, err := store.SavePlan(ctx, plan)
	if err != nil {
		t.Fatalf("SavePlan failed: %v", err)
	}

	cronID := "dn:1:test-cron"
	err = store.SetCronJobID(ctx, id, cronID)
	if err != nil {
		t.Fatalf("SetCronJobID failed: %v", err)
	}

	retrieved, err := store.GetPlan(ctx, id)
	if err != nil {
		t.Fatalf("GetPlan failed: %v", err)
	}
	if retrieved.CronJobID != cronID {
		t.Errorf("CronJobID not set correctly: %s vs %s", retrieved.CronJobID, cronID)
	}
}

func TestDeletePlanCascade(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	plan := createTestPlan(t, "test-delete")
	planID, err := store.SavePlan(ctx, plan)
	if err != nil {
		t.Fatalf("SavePlan failed: %v", err)
	}

	// Create snapshots
	snap1 := &MonitorSnapshot{
		PlanID:    planID,
		CheckedAt: time.Now(),
		CreatedAt: time.Now(),
	}
	snapID1, err := store.SaveSnapshot(ctx, snap1)
	if err != nil {
		t.Fatalf("SaveSnapshot failed: %v", err)
	}

	// Create alerts
	alert1 := &Alert{
		PlanID:      planID,
		SnapshotID:  &snapID1,
		TriggeredAt: time.Now(),
		Severity:    "warning",
		Code:        "test_code",
		Message:     "test message",
		CreatedAt:   time.Now(),
	}
	_, err = store.SaveAlert(ctx, alert1)
	if err != nil {
		t.Fatalf("SaveAlert failed: %v", err)
	}

	// Create execution
	exec := &Execution{
		PlanID:      planID,
		AttemptID:   "attempt1",
		State:       "pending",
		RequestedAt: time.Now(),
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}
	execID, err := store.SaveExecution(ctx, exec)
	if err != nil {
		t.Fatalf("SaveExecution failed: %v", err)
	}

	// Create execution leg
	leg := &ExecutionLeg{
		ExecutionID: execID,
		LegType:     "spot",
		Provider:    "binance",
		Symbol:      "ETHUSDT",
		Side:        "buy",
		OrderType:   "limit",
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}
	_, err = store.SaveExecutionLeg(ctx, leg)
	if err != nil {
		t.Fatalf("SaveExecutionLeg failed: %v", err)
	}

	// Delete plan
	err = store.DeletePlan(ctx, planID)
	if err != nil {
		t.Fatalf("DeletePlan failed: %v", err)
	}

	// Verify cascades deleted
	_, err = store.GetPlan(ctx, planID)
	if err == nil {
		t.Errorf("Plan should be deleted but still exists")
	}

	snapshots, err := store.ListSnapshots(ctx, planID, 100, 0)
	if err != nil {
		t.Fatalf("ListSnapshots failed: %v", err)
	}
	if len(snapshots) != 0 {
		t.Errorf("Expected 0 snapshots after delete, got %d", len(snapshots))
	}

	alerts, err := store.ListAlerts(ctx, planID, 100, 0)
	if err != nil {
		t.Fatalf("ListAlerts failed: %v", err)
	}
	if len(alerts) != 0 {
		t.Errorf("Expected 0 alerts after delete, got %d", len(alerts))
	}

	executions, err := store.ListExecutions(ctx, planID, 100, 0)
	if err != nil {
		t.Fatalf("ListExecutions failed: %v", err)
	}
	if len(executions) != 0 {
		t.Errorf("Expected 0 executions after delete, got %d", len(executions))
	}
}

func TestSaveSnapshot(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	plan := createTestPlan(t, "test-snap")
	planID, err := store.SavePlan(ctx, plan)
	if err != nil {
		t.Fatalf("SavePlan failed: %v", err)
	}

	snap := &MonitorSnapshot{
		PlanID:              planID,
		CheckedAt:           time.Now(),
		SpotPrice:           1900,
		SpotQuantity:        2.5,
		SpotValueUSDT:       4750,
		FuturesMarkPrice:    1899,
		FuturesContracts:    2.5,
		FuturesNotionalUSDT: 4747.5,
		CurrentFundingRate:  0.0001,
		HealthScore:         85,
		HealthLabel:         HealthLabelHealthy,
		DataStatus:          DataStatusOK,
		BreachCodes:         []string{"code1", "code2"},
		CreatedAt:           time.Now(),
	}

	id, err := store.SaveSnapshot(ctx, snap)
	if err != nil {
		t.Fatalf("SaveSnapshot failed: %v", err)
	}
	if id <= 0 {
		t.Fatalf("SaveSnapshot returned invalid ID")
	}
	if snap.ID != id {
		t.Fatalf("SaveSnapshot did not set snap.ID")
	}
}

func TestLatestSnapshot(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	plan := createTestPlan(t, "test-latest-snap")
	planID, err := store.SavePlan(ctx, plan)
	if err != nil {
		t.Fatalf("SavePlan failed: %v", err)
	}

	// Create two snapshots
	snap1 := &MonitorSnapshot{
		PlanID:      planID,
		CheckedAt:   time.Now().Add(-10 * time.Second),
		HealthLabel: HealthLabelHealthy,
		DataStatus:  DataStatusOK,
		CreatedAt:   time.Now().Add(-10 * time.Second),
	}
	store.SaveSnapshot(ctx, snap1)

	time.Sleep(100 * time.Millisecond)

	snap2 := &MonitorSnapshot{
		PlanID:      planID,
		CheckedAt:   time.Now(),
		HealthLabel: HealthLabelWatch,
		DataStatus:  DataStatusOK,
		BreachCodes: []string{"test_breach"},
		CreatedAt:   time.Now(),
	}
	store.SaveSnapshot(ctx, snap2)

	latest, err := store.LatestSnapshot(ctx, planID)
	if err != nil {
		t.Fatalf("LatestSnapshot failed: %v", err)
	}
	if latest == nil {
		t.Fatalf("LatestSnapshot returned nil")
	}
	if latest.HealthLabel != HealthLabelWatch {
		t.Errorf("LatestSnapshot returned wrong snapshot: %s vs %s", latest.HealthLabel, HealthLabelWatch)
	}
	// Verify breach codes round-tripped
	if len(latest.BreachCodes) != 1 || latest.BreachCodes[0] != "test_breach" {
		t.Errorf("BreachCodes not round-tripped correctly")
	}
}

func TestListSnapshots(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	plan := createTestPlan(t, "test-list-snaps")
	planID, err := store.SavePlan(ctx, plan)
	if err != nil {
		t.Fatalf("SavePlan failed: %v", err)
	}

	// Create 5 snapshots
	for i := 0; i < 5; i++ {
		snap := &MonitorSnapshot{
			PlanID:      planID,
			CheckedAt:   time.Now().Add(time.Duration(i) * time.Second),
			DataStatus:  DataStatusOK,
			HealthLabel: HealthLabelHealthy,
			CreatedAt:   time.Now(),
		}
		store.SaveSnapshot(ctx, snap)
	}

	// List with limit
	snaps, err := store.ListSnapshots(ctx, planID, 3, 0)
	if err != nil {
		t.Fatalf("ListSnapshots failed: %v", err)
	}
	if len(snaps) != 3 {
		t.Errorf("Expected 3 snapshots, got %d", len(snaps))
	}

	// List with offset
	snaps, err = store.ListSnapshots(ctx, planID, 3, 2)
	if err != nil {
		t.Fatalf("ListSnapshots with offset failed: %v", err)
	}
	if len(snaps) != 3 {
		t.Errorf("Expected 3 snapshots with offset, got %d", len(snaps))
	}
}

func TestSaveAlert(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	plan := createTestPlan(t, "test-alert")
	planID, err := store.SavePlan(ctx, plan)
	if err != nil {
		t.Fatalf("SavePlan failed: %v", err)
	}

	alert := &Alert{
		PlanID:            planID,
		TriggeredAt:       time.Now(),
		Severity:          "critical",
		Code:              "liquidation_warning",
		Message:           "Liquidation distance below threshold",
		RecommendedAction: "Reduce position",
		AgentInvoked:      true,
		DeliveredChannel:  "telegram",
		DeliveredChatID:   "123456",
		CreatedAt:         time.Now(),
	}

	id, err := store.SaveAlert(ctx, alert)
	if err != nil {
		t.Fatalf("SaveAlert failed: %v", err)
	}
	if id <= 0 {
		t.Fatalf("SaveAlert returned invalid ID")
	}
}

func TestLatestAlert(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	plan := createTestPlan(t, "test-latest-alert")
	planID, err := store.SavePlan(ctx, plan)
	if err != nil {
		t.Fatalf("SavePlan failed: %v", err)
	}

	// Create two alerts
	alert1 := &Alert{
		PlanID:      planID,
		TriggeredAt: time.Now().Add(-10 * time.Second),
		Severity:    "warning",
		Code:        "code1",
		Message:     "msg1",
		CreatedAt:   time.Now().Add(-10 * time.Second),
	}
	store.SaveAlert(ctx, alert1)

	time.Sleep(100 * time.Millisecond)

	alert2 := &Alert{
		PlanID:      planID,
		TriggeredAt: time.Now(),
		Severity:    "critical",
		Code:        "code2",
		Message:     "msg2",
		CreatedAt:   time.Now(),
	}
	store.SaveAlert(ctx, alert2)

	latest, err := store.LatestAlert(ctx, planID)
	if err != nil {
		t.Fatalf("LatestAlert failed: %v", err)
	}
	if latest == nil {
		t.Fatalf("LatestAlert returned nil")
	}
	if latest.Severity != "critical" {
		t.Errorf("LatestAlert returned wrong alert: %s vs critical", latest.Severity)
	}
}

func TestListAlerts(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	plan := createTestPlan(t, "test-list-alerts")
	planID, err := store.SavePlan(ctx, plan)
	if err != nil {
		t.Fatalf("SavePlan failed: %v", err)
	}

	// Create 3 alerts
	for i := 0; i < 3; i++ {
		alert := &Alert{
			PlanID:      planID,
			TriggeredAt: time.Now(),
			Severity:    "warning",
			Code:        "test_code",
			Message:     "test message",
			CreatedAt:   time.Now(),
		}
		store.SaveAlert(ctx, alert)
	}

	alerts, err := store.ListAlerts(ctx, planID, 100, 0)
	if err != nil {
		t.Fatalf("ListAlerts failed: %v", err)
	}
	if len(alerts) != 3 {
		t.Errorf("Expected 3 alerts, got %d", len(alerts))
	}
}

func TestSaveExecution(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	plan := createTestPlan(t, "test-exec")
	planID, err := store.SavePlan(ctx, plan)
	if err != nil {
		t.Fatalf("SavePlan failed: %v", err)
	}

	exec := &Execution{
		PlanID:      planID,
		AttemptID:   "attempt-123",
		State:       "placing_first_leg",
		RequestedAt: time.Now(),
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}

	id, err := store.SaveExecution(ctx, exec)
	if err != nil {
		t.Fatalf("SaveExecution failed: %v", err)
	}
	if id <= 0 {
		t.Fatalf("SaveExecution returned invalid ID")
	}
}

func TestUpdateExecution(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	plan := createTestPlan(t, "test-update-exec")
	planID, err := store.SavePlan(ctx, plan)
	if err != nil {
		t.Fatalf("SavePlan failed: %v", err)
	}

	exec := &Execution{
		PlanID:      planID,
		AttemptID:   "attempt-456",
		State:       "placing_second_leg",
		RequestedAt: time.Now(),
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}
	id, err := store.SaveExecution(ctx, exec)
	if err != nil {
		t.Fatalf("SaveExecution failed: %v", err)
	}

	// Update state
	exec.ID = id
	now := time.Now()
	exec.ApprovedAt = &now
	exec.State = "both_legs_filled"
	exec.CompletedAt = &now

	err = store.UpdateExecution(ctx, exec)
	if err != nil {
		t.Fatalf("UpdateExecution failed: %v", err)
	}

	retrieved, err := store.GetExecution(ctx, id)
	if err != nil {
		t.Fatalf("GetExecution failed: %v", err)
	}
	if retrieved.State != "both_legs_filled" {
		t.Errorf("State not updated")
	}
	if retrieved.ApprovedAt == nil {
		t.Errorf("ApprovedAt not set")
	}
}

func TestListExecutions(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	plan := createTestPlan(t, "test-list-exec")
	planID, err := store.SavePlan(ctx, plan)
	if err != nil {
		t.Fatalf("SavePlan failed: %v", err)
	}

	// Create 3 executions
	for i := 0; i < 3; i++ {
		exec := &Execution{
			PlanID:      planID,
			AttemptID:   "attempt-" + string(rune(i)),
			State:       "pending",
			RequestedAt: time.Now(),
			CreatedAt:   time.Now(),
			UpdatedAt:   time.Now(),
		}
		store.SaveExecution(ctx, exec)
	}

	execs, err := store.ListExecutions(ctx, planID, 100, 0)
	if err != nil {
		t.Fatalf("ListExecutions failed: %v", err)
	}
	if len(execs) != 3 {
		t.Errorf("Expected 3 executions, got %d", len(execs))
	}
}

func TestSaveExecutionLeg(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	plan := createTestPlan(t, "test-leg")
	planID, err := store.SavePlan(ctx, plan)
	if err != nil {
		t.Fatalf("SavePlan failed: %v", err)
	}

	exec := &Execution{
		PlanID:      planID,
		AttemptID:   "attempt-leg",
		State:       "pending",
		RequestedAt: time.Now(),
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}
	execID, err := store.SaveExecution(ctx, exec)
	if err != nil {
		t.Fatalf("SaveExecution failed: %v", err)
	}

	leg := &ExecutionLeg{
		ExecutionID:           execID,
		LegType:               "spot",
		Provider:              "binance",
		Account:               "spot1",
		Symbol:                "ETHUSDT",
		Side:                  "buy",
		OrderType:             "limit",
		RequestedAmount:       2.5,
		RequestedNotionalUSDT: 5000,
		RequestedPrice:        2000,
		CreatedAt:             time.Now(),
		UpdatedAt:             time.Now(),
	}

	id, err := store.SaveExecutionLeg(ctx, leg)
	if err != nil {
		t.Fatalf("SaveExecutionLeg failed: %v", err)
	}
	if id <= 0 {
		t.Fatalf("SaveExecutionLeg returned invalid ID")
	}
}

func TestUpdateExecutionLeg(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	plan := createTestPlan(t, "test-update-leg")
	planID, err := store.SavePlan(ctx, plan)
	if err != nil {
		t.Fatalf("SavePlan failed: %v", err)
	}

	exec := &Execution{
		PlanID:      planID,
		AttemptID:   "attempt-update-leg",
		State:       "pending",
		RequestedAt: time.Now(),
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}
	execID, err := store.SaveExecution(ctx, exec)
	if err != nil {
		t.Fatalf("SaveExecution failed: %v", err)
	}

	leg := &ExecutionLeg{
		ExecutionID:           execID,
		LegType:               "futures",
		Provider:              "binance",
		Symbol:                "ETHUSDT",
		Side:                  "short",
		OrderType:             "limit",
		RequestedAmount:       2.5,
		RequestedNotionalUSDT: 5000,
		CreatedAt:             time.Now(),
		UpdatedAt:             time.Now(),
	}
	legID, err := store.SaveExecutionLeg(ctx, leg)
	if err != nil {
		t.Fatalf("SaveExecutionLeg failed: %v", err)
	}

	// Update leg
	leg.ID = legID
	leg.OrderID = "order123"
	leg.State = "filled"
	leg.FilledQuantity = 2.5
	leg.FilledNotionalUSDT = 5000
	leg.AvgFillPrice = 2000
	leg.FeeUSDT = 1

	err = store.UpdateExecutionLeg(ctx, leg)
	if err != nil {
		t.Fatalf("UpdateExecutionLeg failed: %v", err)
	}

	// Verify
	legs, err := store.ListExecutionLegs(ctx, execID)
	if err != nil {
		t.Fatalf("ListExecutionLegs failed: %v", err)
	}
	if len(legs) != 1 {
		t.Errorf("Expected 1 leg, got %d", len(legs))
	}
	if legs[0].OrderID != "order123" {
		t.Errorf("OrderID not updated")
	}
	if legs[0].State != "filled" {
		t.Errorf("State not updated")
	}
	if legs[0].FilledQuantity != 2.5 {
		t.Errorf("FilledQuantity not updated")
	}
}

func TestListExecutionLegs(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	plan := createTestPlan(t, "test-list-legs")
	planID, err := store.SavePlan(ctx, plan)
	if err != nil {
		t.Fatalf("SavePlan failed: %v", err)
	}

	exec := &Execution{
		PlanID:      planID,
		AttemptID:   "attempt-legs",
		State:       "pending",
		RequestedAt: time.Now(),
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}
	execID, err := store.SaveExecution(ctx, exec)
	if err != nil {
		t.Fatalf("SaveExecution failed: %v", err)
	}

	// Create 2 legs
	for i, legType := range []string{"spot", "futures"} {
		leg := &ExecutionLeg{
			ExecutionID: execID,
			LegType:     legType,
			Provider:    "binance",
			Symbol:      "ETHUSDT",
			Side:        "buy",
			OrderType:   "limit",
			CreatedAt:   time.Now(),
			UpdatedAt:   time.Now(),
		}
		if i > 0 {
			leg.Side = "short"
		}
		store.SaveExecutionLeg(ctx, leg)
	}

	legs, err := store.ListExecutionLegs(ctx, execID)
	if err != nil {
		t.Fatalf("ListExecutionLegs failed: %v", err)
	}
	if len(legs) != 2 {
		t.Errorf("Expected 2 legs, got %d", len(legs))
	}
	if legs[0].LegType != "spot" {
		t.Errorf("First leg should be spot")
	}
	if legs[1].LegType != "futures" {
		t.Errorf("Second leg should be futures")
	}
}

// Helper functions

func setupTestStore(t *testing.T) (*Store, func()) {
	tempDir := t.TempDir()
	store, err := NewStore(tempDir)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	cleanup := func() {
		store.Close()
	}
	return store, cleanup
}

func createTestPlan(t *testing.T, name string) *Plan {
	return &Plan{
		Name:                     name,
		Asset:                    "ETH",
		Status:                   PlanStatusDraft,
		Mode:                     ExecutionModeApproval,
		SpotProvider:             "binance",
		SpotAccount:              "spot1",
		SpotSymbol:               "ETHUSDT",
		SpotSide:                 "buy",
		FuturesProvider:          "binance",
		FuturesAccount:           "futures1",
		FuturesSymbol:            "ETHUSDT",
		FuturesSide:              "short",
		FuturesMarginMode:        "cross",
		FuturesLeverage:          1,
		CapitalUSDT:              10000,
		SpotNotionalUSDT:         5000,
		FuturesNotionalUSDT:      5000,
		ReserveMarginUSDT:        500,
		MonitorInterval:          "5m",
		Enabled:                  true,
		EntryRules:               EntryRules{MinFundingRate: 0.001, MaxSlippageBps: 20},
		ExitRules:                ExitRules{ProfitTargetUSDT: 100, MaxDrawdownUSDT: 50},
		RiskPolicy:               DefaultRiskPolicy(),
		EstimatedEntryCostUSDT:   50,
		EstimatedExitCostUSDT:    50,
		ExpectedDailyFundingUSDT: 10,
		BreakevenDays:            5,
		CrossExchange:            false,
		NotifyChannel:            "telegram",
		NotifyChatID:             "123456",
		CreatedAt:                time.Now(),
		UpdatedAt:                time.Now(),
	}
}

// --- Alert silence tests ---

func TestAlertSilences_UpsertAndGet(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	plan := createTestPlan(t, "silence-test")
	id, _ := store.SavePlan(ctx, plan)

	until := time.Now().Add(time.Hour)
	if err := store.UpsertAlertSilences(ctx, id, []string{"funding_negative", "funding_below_min"}, until); err != nil {
		t.Fatalf("UpsertAlertSilences: %v", err)
	}

	silences, err := store.GetActiveAlertSilences(ctx, id)
	if err != nil {
		t.Fatalf("GetActiveAlertSilences: %v", err)
	}
	if len(silences) != 2 {
		t.Errorf("expected 2 silences, got %d", len(silences))
	}
	if _, ok := silences["funding_negative"]; !ok {
		t.Error("expected funding_negative to be silenced")
	}
}

func TestAlertSilences_ExpiredNotReturned(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	plan := createTestPlan(t, "silence-expired")
	id, _ := store.SavePlan(ctx, plan)

	// Set silence that expired 1 second ago
	past := time.Now().Add(-time.Second)
	if err := store.UpsertAlertSilences(ctx, id, []string{"delta_drift_high"}, past); err != nil {
		t.Fatalf("UpsertAlertSilences: %v", err)
	}

	silences, err := store.GetActiveAlertSilences(ctx, id)
	if err != nil {
		t.Fatalf("GetActiveAlertSilences: %v", err)
	}
	if len(silences) != 0 {
		t.Errorf("expected expired silence to be excluded, got %d", len(silences))
	}
}

func TestAlertSilences_UpsertExtends(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	plan := createTestPlan(t, "silence-extend")
	id, _ := store.SavePlan(ctx, plan)

	// First upsert with 1h window
	first := time.Now().Add(time.Hour)
	store.UpsertAlertSilences(ctx, id, []string{"funding_negative"}, first)

	// Second upsert with 4h — should replace
	longer := time.Now().Add(4 * time.Hour)
	store.UpsertAlertSilences(ctx, id, []string{"funding_negative"}, longer)

	silences, _ := store.GetActiveAlertSilences(ctx, id)
	until, ok := silences["funding_negative"]
	if !ok {
		t.Fatal("expected funding_negative silence after upsert")
	}
	// Should be closer to 4h than 1h
	if until.Before(time.Now().Add(3 * time.Hour)) {
		t.Errorf("expected silence extended to ~4h, got %v", until)
	}
}

func TestAlertSilences_ClearSpecific(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	plan := createTestPlan(t, "silence-clear")
	id, _ := store.SavePlan(ctx, plan)

	until := time.Now().Add(time.Hour)
	store.UpsertAlertSilences(ctx, id, []string{"funding_negative", "delta_drift_high"}, until)

	// Clear only funding_negative
	if err := store.ClearAlertSilences(ctx, id, []string{"funding_negative"}); err != nil {
		t.Fatalf("ClearAlertSilences: %v", err)
	}

	silences, _ := store.GetActiveAlertSilences(ctx, id)
	if _, ok := silences["funding_negative"]; ok {
		t.Error("funding_negative should have been cleared")
	}
	if _, ok := silences["delta_drift_high"]; !ok {
		t.Error("delta_drift_high should still be silenced")
	}
}

func TestAlertSilences_ClearAll(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	plan := createTestPlan(t, "silence-clear-all")
	id, _ := store.SavePlan(ctx, plan)

	until := time.Now().Add(time.Hour)
	store.UpsertAlertSilences(ctx, id, []string{"funding_negative", "delta_drift_high"}, until)

	// Clear all (empty slice)
	if err := store.ClearAlertSilences(ctx, id, []string{}); err != nil {
		t.Fatalf("ClearAlertSilences all: %v", err)
	}

	silences, _ := store.GetActiveAlertSilences(ctx, id)
	if len(silences) != 0 {
		t.Errorf("expected all silences cleared, got %d", len(silences))
	}
}

// --- Fee snapshot tests ---

func TestSavePlanFeeSnapshot(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	plan := createTestPlan(t, "fee-snap-save")
	planID, _ := store.SavePlan(ctx, plan)

	now := time.Now().UTC().Truncate(time.Second)
	start := now.Add(-time.Hour)
	end := now

	snap := &PlanFeeSnapshot{
		PlanID:         planID,
		TradingFeeUSDT: -0.123,
		FundingFeeUSDT: 0.456,
		PeriodStart:    &start,
		PeriodEnd:      &end,
		FetchedAt:      now,
		CreatedAt:      now,
	}

	id, err := store.SavePlanFeeSnapshot(ctx, snap)
	if err != nil {
		t.Fatalf("SavePlanFeeSnapshot: %v", err)
	}
	if id <= 0 {
		t.Errorf("expected positive ID, got %d", id)
	}
	if snap.ID != id {
		t.Errorf("snap.ID not updated: got %d, want %d", snap.ID, id)
	}
}

func TestGetLatestPlanFeeSnapshot(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	plan := createTestPlan(t, "fee-snap-latest")
	planID, _ := store.SavePlan(ctx, plan)

	// No snapshot yet — should return nil.
	snap, err := store.GetLatestPlanFeeSnapshot(ctx, planID)
	if err != nil {
		t.Fatalf("GetLatestPlanFeeSnapshot (empty): %v", err)
	}
	if snap != nil {
		t.Fatalf("expected nil for empty plan, got %+v", snap)
	}

	now := time.Now().UTC().Truncate(time.Second)
	start := now.Add(-2 * time.Hour)
	end := now

	store.SavePlanFeeSnapshot(ctx, &PlanFeeSnapshot{
		PlanID: planID, TradingFeeUSDT: -1.0, FundingFeeUSDT: 0.5,
		PeriodStart: &start, PeriodEnd: &end, FetchedAt: now.Add(-time.Minute), CreatedAt: now.Add(-time.Minute),
	})
	// Insert a later one — should be returned as latest.
	later := now
	store.SavePlanFeeSnapshot(ctx, &PlanFeeSnapshot{
		PlanID: planID, TradingFeeUSDT: -2.5, FundingFeeUSDT: 1.25,
		PeriodStart: &start, PeriodEnd: &end, FetchedAt: later, CreatedAt: later,
	})

	got, err := store.GetLatestPlanFeeSnapshot(ctx, planID)
	if err != nil {
		t.Fatalf("GetLatestPlanFeeSnapshot: %v", err)
	}
	if got == nil {
		t.Fatal("expected snapshot, got nil")
	}
	if got.TradingFeeUSDT != -2.5 {
		t.Errorf("TradingFeeUSDT: got %v, want -2.5", got.TradingFeeUSDT)
	}
	if got.FundingFeeUSDT != 1.25 {
		t.Errorf("FundingFeeUSDT: got %v, want 1.25", got.FundingFeeUSDT)
	}
	if got.PeriodStart == nil {
		t.Error("PeriodStart should not be nil")
	}
	if got.PeriodEnd == nil {
		t.Error("PeriodEnd should not be nil")
	}
}
