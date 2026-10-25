package tools

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/cron"
	"github.com/cryptoquantumwave/khunquant/pkg/dca"
	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
	"github.com/cryptoquantumwave/khunquant/pkg/snapshot"
)

const ownerOnlyMarker = "changed only in the owner's own conversation"

// ownerWriteCase is one mutating tool action of F26 with a store it can be checked
// against: run executes it, state returns everything it could have changed.
type ownerWriteCase struct {
	name     string
	refusal  string
	run      func(ctx context.Context) *ToolResult
	state    func() string
	mustPass bool // the owner's call succeeds on these args (not only "is not refused")
}

func jsonState(t *testing.T, v ...any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// ownerWriteCases builds a fresh set of stores and the cases over them.
func ownerWriteCases(t *testing.T) []ownerWriteCase {
	t.Helper()
	ctx := context.Background()
	cfg := config.DefaultConfig()
	// Other tests re-register "bitkub"; this one has a provider of its own.
	broker.RegisterFactory("f26fake", func(*config.Config) (broker.Provider, error) {
		return mockTradingProvider{id: "f26fake", category: broker.CategoryCrypto}, nil
	})
	t.Cleanup(func() { broker.RegisterFactory("f26fake", nil) })

	cronSvc := cron.NewCronService(filepath.Join(t.TempDir(), "cron.json"), nil)
	dcaStore := newTestDCAStore(t)
	planID, _ := seedPlanWithJob(t, dcaStore, cronSvc)
	dnStore, err := deltaneutral.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dnStore.Close() })
	dnID := seedDNPlan(t, dnStore, "seed-dn", deltaneutral.PlanStatusDraft)
	prepID := seedDNPlan(t, dnStore, "seed-prepare", deltaneutral.PlanStatusDraft) // prepare_delta_neutral_plan promotes it
	snapStore := newTestSnapshotStore(t)
	snapID, err := snapStore.SaveSnapshot(ctx, &snapshot.Snapshot{TakenAt: time.Now().UTC(), Quote: "USDT", TotalValue: 1, Label: "seed"})
	if err != nil {
		t.Fatal(err)
	}

	jobs := func() any { return cronSvc.ListJobs(true) }
	dcaState := func() string {
		p, _ := dcaStore.ListPlans(ctx, dca.QueryFilter{})
		return jsonState(t, p, jobs())
	}
	dnState := func() string {
		p, _ := dnStore.ListPlans(ctx, deltaneutral.QueryFilter{})
		return jsonState(t, p, jobs())
	}
	snapState := func() string {
		s, _ := snapStore.QuerySnapshots(ctx, snapshot.QueryFilter{Limit: 100})
		return jsonState(t, s)
	}
	jobState := func() string { return jsonState(t, jobs()) }

	price := NewSetPriceAlertTool(cfg, cronSvc)
	indicator := NewSetIndicatorAlertTool(cfg, cronSvc)
	alertCreate := map[string]any{
		"action": "create", "provider": "bitkub", "symbol": "BTC/THB", "condition": "above", "threshold": 100.0,
		"channel": "telegram", "chat_id": "999", "message": "text the caller wrote",
	}
	indicatorCreate := map[string]any{
		"action": "create", "provider": "bitkub", "symbol": "BTC/THB", "indicator": "rsi", "condition": "above",
		"threshold": 70.0, "channel": "telegram", "chat_id": "999",
	}
	alertCancel := map[string]any{"action": "cancel", "alert_id": "nope"}

	const (
		dcaR = "DCA plans are " + ownerOnlyMarker
		dnR  = "delta-neutral plans are " + ownerOnlyMarker
		alR  = "alerts are " + ownerOnlyMarker
	)
	return []ownerWriteCase{
		{"create_dca_plan", dcaR, func(c context.Context) *ToolResult {
			return NewCreateDCAPlanTool(cfg, dcaStore, cronSvc).Execute(c, map[string]any{
				"plan_name": "p", "provider": "f26fake", "symbol": "BTC/THB", "amount_per_order": 100.0,
				"side": "buy", "schedule": map[string]any{"cron": "0 9 * * 1"},
			})
		}, dcaState, true},
		{"update_dca_plan", dcaR, func(c context.Context) *ToolResult {
			return NewUpdateDCAPlanTool(dcaStore, cronSvc).Execute(c, map[string]any{"plan_id": float64(planID), "amount_per_order": 250.0})
		}, dcaState, true},
		{"update_dca_plan/revoke_mandate", dcaR, func(c context.Context) *ToolResult {
			return NewUpdateDCAPlanTool(dcaStore, cronSvc).Execute(c, map[string]any{"plan_id": float64(planID), "revoke_mandate": true})
		}, dcaState, false},
		{"create_delta_neutral_plan", dnR, func(c context.Context) *ToolResult {
			return NewCreateDeltaNeutralPlanTool(cfg, dnStore, cronSvc).Execute(c, map[string]any{
				"plan_name": "p", "asset": "BTC", "spot_provider": "binance", "spot_symbol": "BTC/USDT",
				"futures_provider": "okx", "futures_symbol": "BTC/USDT:USDT", "capital_usdt": 1000.0,
			})
		}, dnState, false},
		{"update_delta_neutral_plan", dnR, func(c context.Context) *ToolResult {
			return NewUpdateDeltaNeutralPlanTool(cfg, dnStore, cronSvc).Execute(c, map[string]any{"plan_id": float64(dnID), "leverage": 2.0})
		}, dnState, false},
		{"delete_delta_neutral_plan", dnR, func(c context.Context) *ToolResult {
			return NewDeleteDeltaNeutralPlanTool(dnStore, cronSvc).Execute(c, map[string]any{"plan_id": float64(dnID)})
		}, dnState, false},
		{"set_price_alert/create", alR, func(c context.Context) *ToolResult { return price.Execute(c, alertCreate) }, jobState, true},
		{"set_price_alert/cancel", alR, func(c context.Context) *ToolResult { return price.Execute(c, alertCancel) }, jobState, false},
		{"set_indicator_alert/create", alR, func(c context.Context) *ToolResult { return indicator.Execute(c, indicatorCreate) }, jobState, false},
		{"set_indicator_alert/cancel", alR, func(c context.Context) *ToolResult { return indicator.Execute(c, alertCancel) }, jobState, false},
		{"delete_snapshots", "snapshots are " + ownerOnlyMarker, func(c context.Context) *ToolResult {
			return NewDeleteSnapshotsTool(snapStore).Execute(c, map[string]any{"id": float64(snapID)})
		}, snapState, true},
		{"webull_reconnect", "account sessions are " + ownerOnlyMarker, func(c context.Context) *ToolResult {
			return NewWebullReconnectTool(cfg).Execute(c, map[string]any{})
		}, func() string { return "" }, false},
		// F27: take_snapshot cannot succeed here (no exchange), so it is only "not refused".
		{"take_snapshot", "snapshots are " + ownerOnlyMarker, func(c context.Context) *ToolResult {
			return NewTakeSnapshotTool(cfg, snapStore).Execute(c, map[string]any{"label": "x"})
		}, snapState, false},
		{"prepare_delta_neutral_plan", dnR, func(c context.Context) *ToolResult {
			return NewPrepareDeltaNeutralPlanTool(cfg, dnStore).Execute(c, map[string]any{"plan_id": float64(prepID)})
		}, dnState, true},
		{"delete_dca_plan", dcaR, func(c context.Context) *ToolResult {
			return NewDeleteDCAPlanTool(dcaStore, cronSvc).Execute(c, map[string]any{"plan_id": float64(planID)})
		}, dcaState, true},
	}
}

func TestOwnerOnlyWrites_PlansAlertsSessionsAreOwnerOnly(t *testing.T) {
	armWebGate(t, true)
	armOwner(t, true)
	allowed, refused := ownerOnlyTurns()

	for name, ctx := range allowed {
		for _, tc := range ownerWriteCases(t) {
			t.Run("allowed/"+name+"/"+tc.name, func(t *testing.T) {
				res := tc.run(ctx)
				if strings.Contains(res.ForLLM, ownerOnlyMarker) {
					t.Fatalf("refused to the owner: %s", res.ForLLM)
				}
				if tc.mustPass && res.IsError {
					t.Fatalf("the owner's call failed: %s", res.ForLLM)
				}
			})
		}
	}
	for name, ctx := range refused {
		for _, tc := range ownerWriteCases(t) {
			t.Run("refused/"+name+"/"+tc.name, func(t *testing.T) {
				before := tc.state()
				res := tc.run(ctx)
				if !res.IsError || !strings.Contains(res.ForLLM, tc.refusal) {
					t.Fatalf("not refused: %+v", res)
				}
				if !strings.Contains(res.ForLLM, "Nothing was changed.") {
					t.Fatalf("refusal does not say nothing changed: %s", res.ForLLM)
				}
				if after := tc.state(); after != before {
					t.Fatalf("the store changed:\nbefore %s\nafter  %s", before, after)
				}
			})
		}
	}
}

func TestOwnerOnlyWrites_NoOwnerConfigured_NothingChanges(t *testing.T) {
	armOwner(t, false)
	ctx := remoteTurn("telegram:999", "telegram")
	for _, tc := range ownerWriteCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			res := tc.run(ctx)
			if strings.Contains(res.ForLLM, ownerOnlyMarker) {
				t.Fatalf("refused with no owner configured: %s", res.ForLLM)
			}
			if tc.mustPass && res.IsError {
				t.Fatalf("failed with no owner configured: %s", res.ForLLM)
			}
		})
	}
}

// Reading stays open: listing alerts is not gated for any turn.
func TestOwnerOnlyWrites_AlertListStaysOpen(t *testing.T) {
	armWebGate(t, true)
	armOwner(t, true)
	allowed, refused := ownerOnlyTurns()
	cronSvc := cron.NewCronService(filepath.Join(t.TempDir(), "cron.json"), nil)
	listers := []Tool{
		NewSetPriceAlertTool(config.DefaultConfig(), cronSvc),
		NewSetIndicatorAlertTool(config.DefaultConfig(), cronSvc),
	}
	for _, turns := range []map[string]context.Context{allowed, refused} {
		for name, ctx := range turns {
			t.Run(name, func(t *testing.T) {
				for _, tool := range listers {
					if res := tool.Execute(ctx, map[string]any{"action": "list"}); res.IsError {
						t.Fatalf("%s list refused: %s", tool.Name(), res.ForLLM)
					}
				}
			})
		}
	}
}

// take_snapshot keeps the autonomous origins the class ceiling gives it: the legacy
// heartbeat (as a proactive turn of that kind, or the older "heartbeat" origin) and a
// scheduled job have no sender to check. A proactive kind the ceiling denies never
// reaches the tool at all.
func TestOwnerOnlyWrites_TakeSnapshotHeartbeatStaysAllowed(t *testing.T) {
	armWebGate(t, true)
	armOwner(t, true)
	tool := NewTakeSnapshotTool(config.DefaultConfig(), newTestSnapshotStore(t))
	for name, ctx := range map[string]context.Context{
		"legacy heartbeat (proactive)": WithProactiveTurn(context.Background(), proactive.TurnHeartbeatLegacy),
		"heartbeat origin":             WithToolOrigin(context.Background(), OriginHeartbeat),
		"scheduled job":                WithToolOrigin(context.Background(), OriginCron),
	} {
		t.Run(name, func(t *testing.T) {
			if res := tool.Execute(ctx, map[string]any{"label": "heartbeat"}); strings.Contains(res.ForLLM, ownerOnlyMarker) {
				t.Fatalf("refused: %s", res.ForLLM)
			}
		})
	}
	if ctx := WithProactiveTurn(context.Background(), proactive.TurnProactive); TurnMayCall(ctx, NameTakeSnapshot) {
		t.Fatal("the ceiling lets a candidate-driven proactive turn take a snapshot")
	}
	if !TurnMayCall(WithProactiveTurn(context.Background(), proactive.TurnHeartbeatLegacy), NameTakeSnapshot) {
		t.Fatal("the ceiling no longer lets the legacy heartbeat take a snapshot")
	}
}

// Validation of a draft is a read and stays open to every turn; only the promotion to
// "ready" is the owner's.
func TestOwnerOnlyWrites_PrepareValidationStaysOpen(t *testing.T) {
	armWebGate(t, true)
	armOwner(t, true)
	store, err := deltaneutral.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	id := seedDNPlan(t, store, "bad", deltaneutral.PlanStatusDraft)
	p, _ := store.GetPlan(context.Background(), id)
	p.CapitalUSDT = 0
	if err := store.UpdatePlan(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	res := NewPrepareDeltaNeutralPlanTool(config.DefaultConfig(), store).Execute(remoteTurn("telegram:999", "telegram"), map[string]any{"plan_id": float64(id)})
	if strings.Contains(res.ForLLM, ownerOnlyMarker) || !strings.Contains(res.ForLLM, "FAILED") {
		t.Fatalf("validation report: %s", res.ForLLM)
	}
}
