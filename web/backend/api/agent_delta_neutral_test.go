package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral"
)

func setupDeltaNeutralTestEnv(t *testing.T) (string, func()) {
	t.Helper()

	tmpDir := t.TempDir()
	workspaceDir := filepath.Join(tmpDir, "workspace")
	t.Setenv("HOME", tmpDir)
	t.Setenv("KHUNQUANT_HOME", filepath.Join(tmpDir, ".khunquant"))

	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = workspaceDir
	configPath := filepath.Join(tmpDir, "config.json")
	if err := config.SaveConfig(configPath, cfg); err != nil {
		t.Fatalf("SaveConfig error: %v", err)
	}

	return configPath, func() {
		// Cleanup is handled by t.TempDir
	}
}

func TestDeltaNeutralPlanListHandlerEmpty(t *testing.T) {
	configPath, cleanup := setupDeltaNeutralTestEnv(t)
	defer cleanup()

	// Load config to get the workspace path
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}
	ws := cfg.WorkspacePath()

	// Create store and close it immediately
	store, err := deltaneutral.NewStore(ws)
	if err != nil {
		t.Fatalf("Failed to create test store: %v", err)
	}
	store.Close()

	handler := NewHandler(configPath)
	mux := http.NewServeMux()
	handler.registerAgentDeltaNeutralRoutes(mux)

	req := httptest.NewRequest("GET", "/api/agent/delta-neutral/plans", nil)
	w := httptest.NewRecorder()

	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected status 200, got %d", w.Code)
	}

	var items []dnPlanListItem
	err = json.Unmarshal(w.Body.Bytes(), &items)
	if err != nil {
		t.Fatalf("Failed to unmarshal response: %v", err)
	}

	if len(items) != 0 {
		t.Fatalf("Expected 0 items, got %d", len(items))
	}
}

func TestDeltaNeutralPlanListHandlerWithPlan(t *testing.T) {
	configPath, cleanup := setupDeltaNeutralTestEnv(t)
	defer cleanup()

	// Load config to get the workspace path
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}
	ws := cfg.WorkspacePath()

	// Create and seed a test plan
	store, storeErr := deltaneutral.NewStore(ws)
	if storeErr != nil {
		t.Fatalf("Failed to create test store: %v", storeErr)
	}

	now := time.Now()
	testPlan := &deltaneutral.Plan{
		Name:              "test-eth-plan",
		Asset:             "ETH",
		Status:            deltaneutral.PlanStatusActive,
		Mode:              deltaneutral.ExecutionModeApproval,
		SpotProvider:      "binance",
		SpotAccount:       "spot-1",
		SpotSymbol:        "ETHUSDT",
		SpotSide:          "buy",
		FuturesProvider:   "binance",
		FuturesAccount:    "futures-1",
		FuturesSymbol:     "ETHUSDT",
		FuturesSide:       "short",
		FuturesMarginMode: "cross",
		FuturesLeverage:   1,
		CapitalUSDT:       10000,
		MonitorInterval:   "5m",
		Enabled:           true,
		CrossExchange:     false,
		CreatedAt:         now,
		UpdatedAt:         now,
	}

	planID, err := store.SavePlan(context.Background(), testPlan)
	if err != nil {
		store.Close()
		t.Fatalf("Failed to save test plan: %v", err)
	}

	// Create a snapshot for health enrichment
	snapshot := &deltaneutral.MonitorSnapshot{
		PlanID:      planID,
		CheckedAt:   now,
		HealthScore: 85,
		HealthLabel: deltaneutral.HealthLabelHealthy,
		CreatedAt:   now,
	}
	_, err = store.SaveSnapshot(context.Background(), snapshot)
	if err != nil {
		store.Close()
		t.Fatalf("Failed to save test snapshot: %v", err)
	}

	store.Close()

	// Now test the handler
	handler := NewHandler(configPath)
	mux := http.NewServeMux()
	handler.registerAgentDeltaNeutralRoutes(mux)

	req := httptest.NewRequest("GET", "/api/agent/delta-neutral/plans", nil)
	w := httptest.NewRecorder()

	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected status 200, got %d", w.Code)
	}

	var items []dnPlanListItem
	err = json.Unmarshal(w.Body.Bytes(), &items)
	if err != nil {
		t.Fatalf("Failed to unmarshal response: %v", err)
	}

	if len(items) != 1 {
		t.Fatalf("Expected 1 item, got %d", len(items))
	}

	item := items[0]
	if item.ID != planID {
		t.Errorf("Expected plan ID %d, got %d", planID, item.ID)
	}
	if item.Name != "test-eth-plan" {
		t.Errorf("Expected name 'test-eth-plan', got %q", item.Name)
	}
	if item.Asset != "ETH" {
		t.Errorf("Expected asset 'ETH', got %q", item.Asset)
	}
	if item.HealthScore != 85 {
		t.Errorf("Expected health score 85, got %d", item.HealthScore)
	}
	if item.HealthLabel != deltaneutral.HealthLabelHealthy {
		t.Errorf("Expected health label %q, got %q", deltaneutral.HealthLabelHealthy, item.HealthLabel)
	}
	if item.LastCheckedAt == nil {
		t.Errorf("Expected last_checked_at to be set")
	}
}

func TestDeltaNeutralPlanGetHandler(t *testing.T) {
	configPath, cleanup := setupDeltaNeutralTestEnv(t)
	defer cleanup()

	// Load config to get the workspace path
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}
	ws := cfg.WorkspacePath()

	// Create and seed a test plan
	store, err := deltaneutral.NewStore(ws)
	if err != nil {
		t.Fatalf("Failed to create test store: %v", err)
	}

	now := time.Now()
	testPlan := &deltaneutral.Plan{
		Name:              "test-btc-plan",
		Asset:             "BTC",
		Status:            deltaneutral.PlanStatusReady,
		Mode:              deltaneutral.ExecutionModeApproval,
		SpotProvider:      "binance",
		SpotAccount:       "spot-1",
		SpotSymbol:        "BTCUSDT",
		SpotSide:          "buy",
		FuturesProvider:   "binance",
		FuturesAccount:    "futures-1",
		FuturesSymbol:     "BTCUSDT",
		FuturesSide:       "short",
		FuturesMarginMode: "cross",
		FuturesLeverage:   1,
		CapitalUSDT:       20000,
		MonitorInterval:   "5m",
		Enabled:           true,
		CrossExchange:     false,
		CreatedAt:         now,
		UpdatedAt:         now,
	}

	planID, err := store.SavePlan(context.Background(), testPlan)
	if err != nil {
		store.Close()
		t.Fatalf("Failed to save test plan: %v", err)
	}

	store.Close()

	// Test the handler
	handler := NewHandler(configPath)
	mux := http.NewServeMux()
	handler.registerAgentDeltaNeutralRoutes(mux)

	req := httptest.NewRequest("GET", fmt.Sprintf("/api/agent/delta-neutral/plans/%d", planID), nil)
	w := httptest.NewRecorder()

	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected status 200, got %d", w.Code)
	}

	var item dnPlanListItem
	err = json.Unmarshal(w.Body.Bytes(), &item)
	if err != nil {
		t.Fatalf("Failed to unmarshal response: %v", err)
	}

	if item.ID != planID {
		t.Errorf("Expected plan ID %d, got %d", planID, item.ID)
	}
	if item.Name != "test-btc-plan" {
		t.Errorf("Expected name 'test-btc-plan', got %q", item.Name)
	}
	if item.Asset != "BTC" {
		t.Errorf("Expected asset 'BTC', got %q", item.Asset)
	}
}

func TestDeltaNeutralPlanGetHandlerNotFound(t *testing.T) {
	configPath, cleanup := setupDeltaNeutralTestEnv(t)
	defer cleanup()

	handler := NewHandler(configPath)
	mux := http.NewServeMux()
	handler.registerAgentDeltaNeutralRoutes(mux)

	req := httptest.NewRequest("GET", "/api/agent/delta-neutral/plans/99999", nil)
	w := httptest.NewRecorder()

	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("Expected status 404, got %d", w.Code)
	}
}

func TestDeltaNeutralSnapshotsHandler(t *testing.T) {
	configPath, cleanup := setupDeltaNeutralTestEnv(t)
	defer cleanup()

	// Load config to get the workspace path
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}
	ws := cfg.WorkspacePath()

	// Create and seed test data
	store, err := deltaneutral.NewStore(ws)
	if err != nil {
		t.Fatalf("Failed to create test store: %v", err)
	}

	now := time.Now()
	testPlan := &deltaneutral.Plan{
		Name:            "test-plan",
		Asset:           "ETH",
		Status:          deltaneutral.PlanStatusActive,
		Mode:            deltaneutral.ExecutionModeApproval,
		SpotProvider:    "binance",
		SpotAccount:     "spot-1",
		SpotSymbol:      "ETHUSDT",
		FuturesProvider: "binance",
		FuturesAccount:  "futures-1",
		FuturesSymbol:   "ETHUSDT",
		CreatedAt:       now,
		UpdatedAt:       now,
	}

	planID, err := store.SavePlan(context.Background(), testPlan)
	if err != nil {
		store.Close()
		t.Fatalf("Failed to save test plan: %v", err)
	}

	snapshot := &deltaneutral.MonitorSnapshot{
		PlanID:      planID,
		CheckedAt:   now,
		HealthScore: 75,
		HealthLabel: deltaneutral.HealthLabelWatch,
		CreatedAt:   now,
	}
	_, err = store.SaveSnapshot(context.Background(), snapshot)
	if err != nil {
		store.Close()
		t.Fatalf("Failed to save test snapshot: %v", err)
	}

	store.Close()

	// Test the handler
	handler := NewHandler(configPath)
	mux := http.NewServeMux()
	handler.registerAgentDeltaNeutralRoutes(mux)

	req := httptest.NewRequest("GET", fmt.Sprintf("/api/agent/delta-neutral/plans/%d/monitor-snapshots", planID), nil)
	w := httptest.NewRecorder()

	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected status 200, got %d, body: %s", w.Code, w.Body.String())
	}

	var snapshots []dnMonitorSnapshotDTO
	err = json.Unmarshal(w.Body.Bytes(), &snapshots)
	if err != nil {
		t.Fatalf("Failed to unmarshal response: %v", err)
	}

	if len(snapshots) != 1 {
		t.Fatalf("Expected 1 snapshot, got %d", len(snapshots))
	}

	if snapshots[0].HealthScore != 75 {
		t.Errorf("Expected health score 75, got %d", snapshots[0].HealthScore)
	}
}

func TestDeltaNeutralAlertsHandler(t *testing.T) {
	configPath, cleanup := setupDeltaNeutralTestEnv(t)
	defer cleanup()

	// Load config to get the workspace path
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}
	ws := cfg.WorkspacePath()

	// Create and seed test data
	store, err := deltaneutral.NewStore(ws)
	if err != nil {
		t.Fatalf("Failed to create test store: %v", err)
	}

	now := time.Now()
	testPlan := &deltaneutral.Plan{
		Name:            "test-plan",
		Asset:           "ETH",
		Status:          deltaneutral.PlanStatusActive,
		Mode:            deltaneutral.ExecutionModeApproval,
		SpotProvider:    "binance",
		SpotAccount:     "spot-1",
		SpotSymbol:      "ETHUSDT",
		FuturesProvider: "binance",
		FuturesAccount:  "futures-1",
		FuturesSymbol:   "ETHUSDT",
		CreatedAt:       now,
		UpdatedAt:       now,
	}

	planID, err := store.SavePlan(context.Background(), testPlan)
	if err != nil {
		store.Close()
		t.Fatalf("Failed to save test plan: %v", err)
	}

	alert := &deltaneutral.Alert{
		PlanID:      planID,
		TriggeredAt: now,
		Severity:    "warning",
		Code:        "LOW_FUNDING",
		Message:     "Funding rate is below minimum threshold",
		CreatedAt:   now,
	}
	_, err = store.SaveAlert(context.Background(), alert)
	if err != nil {
		store.Close()
		t.Fatalf("Failed to save test alert: %v", err)
	}

	store.Close()

	// Test the handler
	handler := NewHandler(configPath)
	mux := http.NewServeMux()
	handler.registerAgentDeltaNeutralRoutes(mux)

	req := httptest.NewRequest("GET", fmt.Sprintf("/api/agent/delta-neutral/plans/%d/alerts", planID), nil)
	w := httptest.NewRecorder()

	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected status 200, got %d", w.Code)
	}

	var alerts []dnAlertDTO
	err = json.Unmarshal(w.Body.Bytes(), &alerts)
	if err != nil {
		t.Fatalf("Failed to unmarshal response: %v", err)
	}

	if len(alerts) != 1 {
		t.Fatalf("Expected 1 alert, got %d", len(alerts))
	}

	if alerts[0].Code != "LOW_FUNDING" {
		t.Errorf("Expected code 'LOW_FUNDING', got %q", alerts[0].Code)
	}
}

func TestDeltaNeutralExecutionsHandler(t *testing.T) {
	configPath, cleanup := setupDeltaNeutralTestEnv(t)
	defer cleanup()

	// Load config to get the workspace path
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}
	ws := cfg.WorkspacePath()

	// Create and seed test data
	store, err := deltaneutral.NewStore(ws)
	if err != nil {
		t.Fatalf("Failed to create test store: %v", err)
	}

	now := time.Now()
	testPlan := &deltaneutral.Plan{
		Name:            "test-plan",
		Asset:           "ETH",
		Status:          deltaneutral.PlanStatusActive,
		Mode:            deltaneutral.ExecutionModeApproval,
		SpotProvider:    "binance",
		SpotAccount:     "spot-1",
		SpotSymbol:      "ETHUSDT",
		FuturesProvider: "binance",
		FuturesAccount:  "futures-1",
		FuturesSymbol:   "ETHUSDT",
		CreatedAt:       now,
		UpdatedAt:       now,
	}

	planID, err := store.SavePlan(context.Background(), testPlan)
	if err != nil {
		store.Close()
		t.Fatalf("Failed to save test plan: %v", err)
	}

	exec := &deltaneutral.Execution{
		PlanID:      planID,
		AttemptID:   "attempt-001",
		State:       "placing_first_leg",
		RequestedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	execID, err := store.SaveExecution(context.Background(), exec)
	if err != nil {
		store.Close()
		t.Fatalf("Failed to save test execution: %v", err)
	}

	leg := &deltaneutral.ExecutionLeg{
		ExecutionID:           execID,
		LegType:               "spot",
		Provider:              "binance",
		Account:               "spot-1",
		Symbol:                "ETHUSDT",
		Side:                  "buy",
		OrderType:             "market",
		RequestedAmount:       10,
		RequestedNotionalUSDT: 30000,
		State:                 "filled",
		FilledQuantity:        10,
		FilledNotionalUSDT:    30000,
		AvgFillPrice:          3000,
		CreatedAt:             now,
		UpdatedAt:             now,
	}
	_, err = store.SaveExecutionLeg(context.Background(), leg)
	if err != nil {
		store.Close()
		t.Fatalf("Failed to save test execution leg: %v", err)
	}

	store.Close()

	// Test the handler
	handler := NewHandler(configPath)
	mux := http.NewServeMux()
	handler.registerAgentDeltaNeutralRoutes(mux)

	req := httptest.NewRequest("GET", fmt.Sprintf("/api/agent/delta-neutral/plans/%d/executions", planID), nil)
	w := httptest.NewRecorder()

	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected status 200, got %d", w.Code)
	}

	var execs []dnExecutionDTO
	err = json.Unmarshal(w.Body.Bytes(), &execs)
	if err != nil {
		t.Fatalf("Failed to unmarshal response: %v", err)
	}

	if len(execs) != 1 {
		t.Fatalf("Expected 1 execution, got %d", len(execs))
	}

	if len(execs[0].Legs) != 1 {
		t.Fatalf("Expected 1 leg, got %d", len(execs[0].Legs))
	}

	legDTO := execs[0].Legs[0]
	if legDTO.Symbol != "ETHUSDT" {
		t.Errorf("Expected symbol 'ETHUSDT', got %q", legDTO.Symbol)
	}
	if legDTO.FilledQuantity != 10 {
		t.Errorf("Expected filled quantity 10, got %f", legDTO.FilledQuantity)
	}
}

func TestDeltaNeutralNoSecretsExposed(t *testing.T) {
	// Verify that the DTO structs do not include any credential fields
	// This is a compile-time check that sensitive fields are not in DTOs

	configPath, cleanup := setupDeltaNeutralTestEnv(t)
	defer cleanup()

	// Load config to get the workspace path
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}
	ws := cfg.WorkspacePath()

	store, err := deltaneutral.NewStore(ws)
	if err != nil {
		t.Fatalf("Failed to create test store: %v", err)
	}

	now := time.Now()
	testPlan := &deltaneutral.Plan{
		Name:            "test-plan",
		Asset:           "ETH",
		Status:          deltaneutral.PlanStatusActive,
		Mode:            deltaneutral.ExecutionModeApproval,
		SpotProvider:    "binance",
		SpotAccount:     "spot-1",
		SpotSymbol:      "ETHUSDT",
		FuturesProvider: "binance",
		FuturesAccount:  "futures-1",
		FuturesSymbol:   "ETHUSDT",
		CreatedAt:       now,
		UpdatedAt:       now,
	}

	_, saveErr := store.SavePlan(context.Background(), testPlan)
	if saveErr != nil {
		store.Close()
		t.Fatalf("Failed to save test plan: %v", saveErr)
	}

	store.Close()

	handler := NewHandler(configPath)
	mux := http.NewServeMux()
	handler.registerAgentDeltaNeutralRoutes(mux)

	req := httptest.NewRequest("GET", "/api/agent/delta-neutral/plans", nil)
	w := httptest.NewRecorder()

	mux.ServeHTTP(w, req)

	// Parse response and check that no secret-like fields are present
	var items []dnPlanListItem
	err2 := json.Unmarshal(w.Body.Bytes(), &items)
	if err2 != nil {
		t.Fatalf("Failed to unmarshal response: %v", err2)
	}

	// Verify that the DTO only contains expected fields
	// Check that fields like API keys, tokens, etc. are not present
	body := w.Body.String()

	// Verify sensitive strings are NOT in response
	sensitiveStrings := []string{"api_key", "secret", "password", "token"}
	for _, sensitive := range sensitiveStrings {
		if contains(body, sensitive) {
			t.Errorf("Response contains sensitive field: %s", sensitive)
		}
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && findSubstring(s, substr) >= 0
}

func findSubstring(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		match := true
		for j := 0; j < len(substr); j++ {
			if s[i+j] != substr[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

// setupPaperModeDNPlan writes a config with paper trading set as requested and
// stores one plan, returning the config path and the plan's id.
func setupPaperModeDNPlan(t *testing.T, paperMode bool) (string, int64) {
	t.Helper()

	configPath, _ := setupDeltaNeutralTestEnv(t)

	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig error: %v", err)
	}
	cfg.TradingRisk.PaperTradingMode = paperMode
	if err := config.SaveConfig(configPath, cfg); err != nil {
		t.Fatalf("SaveConfig error: %v", err)
	}

	store, err := deltaneutral.NewStore(cfg.WorkspacePath())
	if err != nil {
		t.Fatalf("NewStore error: %v", err)
	}

	now := time.Now()
	// Active status is what makes needsUnwind true. Without it the guard under
	// test is never reached and the assertions below would prove nothing.
	id, saveErr := store.SavePlan(context.Background(), &deltaneutral.Plan{
		Name:            "paper-mode-plan",
		Asset:           "ETH",
		Status:          deltaneutral.PlanStatusActive,
		Mode:            deltaneutral.ExecutionModeApproval,
		SpotProvider:    "binance",
		SpotAccount:     "spot-1",
		SpotSymbol:      "ETHUSDT",
		FuturesProvider: "binance",
		FuturesAccount:  "futures-1",
		FuturesSymbol:   "ETHUSDT",
		CreatedAt:       now,
		UpdatedAt:       now,
	})
	store.Close()
	if saveErr != nil {
		t.Fatalf("SavePlan error: %v", saveErr)
	}

	return configPath, id
}

func deleteDNPlanRequest(t *testing.T, configPath string, id int64, body string) *httptest.ResponseRecorder {
	t.Helper()

	handler := NewHandler(configPath)
	mux := http.NewServeMux()
	handler.registerAgentDeltaNeutralRoutes(mux)

	req := httptest.NewRequest(http.MethodDelete,
		fmt.Sprintf("/api/agent/delta-neutral/plans/%d", id), strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "same-origin")

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

// TestDeleteDeltaNeutralPlan_PaperModeRefusesForceUnwind covers H-6. The unwind
// tool places real reduce-only orders and does not check paper trading mode
// itself, so this handler is the only thing standing between a paper-mode
// deployment and live orders.
func TestDeleteDeltaNeutralPlan_PaperModeRefusesForceUnwind(t *testing.T) {
	configPath, id := setupPaperModeDNPlan(t, true)

	w := deleteDNPlanRequest(t, configPath, id, `{"force_unwind":true}`)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d (Conflict); body = %s",
			w.Code, http.StatusConflict, w.Body.String())
	}

	// The plan must survive the refusal. A 409 that deleted the plan anyway
	// would leave the legs open with nothing tracking them.
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig error: %v", err)
	}
	store, err := deltaneutral.NewStore(cfg.WorkspacePath())
	if err != nil {
		t.Fatalf("NewStore error: %v", err)
	}
	defer store.Close()

	if _, err := store.GetPlan(context.Background(), id); err != nil {
		t.Fatalf("plan %d was deleted despite the refusal: %v", id, err)
	}
}

// The operator keeps a way out: delete_without_unwind places no orders, so
// paper mode does not block it. Refusing force_unwind therefore traps nobody.
func TestDeleteDeltaNeutralPlan_PaperModeAllowsDeleteWithoutUnwind(t *testing.T) {
	configPath, id := setupPaperModeDNPlan(t, true)

	w := deleteDNPlanRequest(t, configPath, id, `{"delete_without_unwind":true}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp deleteDeltaNeutralPlanResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if !resp.DeletedWithoutUnwind {
		t.Errorf("deleted_without_unwind = false, want true")
	}
}

// With paper mode off the guard must not fire. This is what keeps the test
// above honest: it proves the refusal is gated on paper mode rather than
// rejecting every force_unwind.
func TestDeleteDeltaNeutralPlan_LiveModeDoesNotRefuseForceUnwind(t *testing.T) {
	configPath, id := setupPaperModeDNPlan(t, false)

	w := deleteDNPlanRequest(t, configPath, id, `{"force_unwind":true}`)

	if w.Code == http.StatusConflict {
		t.Fatalf("live mode returned %d (Conflict): the paper-mode guard fired when it should not; body = %s",
			w.Code, w.Body.String())
	}
}
