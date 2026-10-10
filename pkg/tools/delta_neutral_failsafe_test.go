package tools

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral"
)

// seedDNOpenLegs records a filled futures-sell leg and a filled spot-buy leg for a plan
// under an execution in the given state, i.e. what a completed (or half-completed) open
// leaves behind.
func seedDNOpenLegs(t *testing.T, store *deltaneutral.Store, planID int64, state deltaneutral.ExecutionState, futuresBase, spotQty float64) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	execID, err := store.SaveExecution(ctx, &deltaneutral.Execution{
		PlanID: planID, AttemptID: "seed-" + string(state), State: string(state),
		RequestedAt: now, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("seed execution: %v", err)
	}
	for _, leg := range []*deltaneutral.ExecutionLeg{
		{LegType: string(deltaneutral.LegTypeFutures), Symbol: "BTC/USDT:USDT", Side: "sell", FilledQuantity: futuresBase},
		{LegType: string(deltaneutral.LegTypeSpot), Symbol: "BTC/USDT", Side: "buy", FilledQuantity: spotQty},
	} {
		if leg.FilledQuantity <= 0 {
			continue
		}
		leg.ExecutionID = execID
		leg.Provider = "dnfake"
		leg.OrderType = "market"
		leg.State = string(deltaneutral.LegStateFilled)
		leg.CreatedAt, leg.UpdatedAt = now, now
		if _, err := store.SaveExecutionLeg(ctx, leg); err != nil {
			t.Fatalf("seed leg: %v", err)
		}
	}
}

// failLegSaves makes saveExecutionLegFn fail for every leg matching match, and returns a
// counter of how many matching save attempts were made. Backoff is disabled.
func failLegSaves(t *testing.T, match func(*deltaneutral.ExecutionLeg) bool) *int {
	t.Helper()
	calls := 0
	origFn, origBackoff := saveExecutionLegFn, legSaveBackoff
	legSaveBackoff = 0
	saveExecutionLegFn = func(ctx context.Context, s *deltaneutral.Store, leg *deltaneutral.ExecutionLeg) (int64, error) {
		if match(leg) {
			calls++
			return 0, errors.New("simulated disk failure")
		}
		return origFn(ctx, s, leg)
	}
	t.Cleanup(func() { saveExecutionLegFn, legSaveBackoff = origFn, origBackoff })
	return &calls
}

func filledLegOf(legType deltaneutral.LegType) func(*deltaneutral.ExecutionLeg) bool {
	return func(l *deltaneutral.ExecutionLeg) bool {
		return l.LegType == string(legType) && l.State == string(deltaneutral.LegStateFilled)
	}
}

func planStatusOf(t *testing.T, store *deltaneutral.Store, id int64) string {
	t.Helper()
	p, err := store.GetPlan(context.Background(), id)
	if err != nil {
		t.Fatalf("get plan: %v", err)
	}
	return p.Status
}

func execsOf(t *testing.T, store *deltaneutral.Store, planID int64) []deltaneutral.Execution {
	t.Helper()
	execs, err := store.ListExecutions(context.Background(), planID, 50, 0)
	if err != nil {
		t.Fatalf("list executions: %v", err)
	}
	return execs
}

// latestExec returns the most recently created execution of a plan whose attempt id has
// the given prefix.
func latestExec(t *testing.T, store *deltaneutral.Store, planID int64, prefix string) deltaneutral.Execution {
	t.Helper()
	var best *deltaneutral.Execution
	execs := execsOf(t, store, planID)
	for i := range execs {
		if strings.HasPrefix(execs[i].AttemptID, prefix) && (best == nil || execs[i].ID > best.ID) {
			best = &execs[i]
		}
	}
	if best == nil {
		t.Fatalf("no %q execution found for plan %d", prefix, planID)
	}
	return *best
}

func assertNoUnwindingExecution(t *testing.T, store *deltaneutral.Store, planID int64) {
	t.Helper()
	for _, e := range execsOf(t, store, planID) {
		if e.State == string(deltaneutral.ExecutionStateUnwinding) {
			t.Fatalf("execution %d (%s) left in 'unwinding'", e.ID, e.AttemptID)
		}
	}
}

// ── Resize ───────────────────────────────────────────────────────────────────

func TestResize_FirstLegFails_PlanStaysActive(t *testing.T) {
	store := setupTempDeltaNeutralStore(t)
	defer store.Close()
	spot := &dnFakeSpot{id: "dnfake", baseFree: 10}
	fut := &dnFakeFutures{mockFuturesProvider: &mockFuturesProvider{}, orderErr: true}
	installDNFakes(t, spot, fut)
	id := seedDNFakePlan(t, store, "resize-firstfail", "active")

	res := NewResizeDeltaNeutralPositionTool(leverageOnCfg(), store).
		Execute(context.Background(), map[string]any{"plan_id": float64(id), "delta_notional_usdt": 1000.0, "confirm": true})
	if !res.IsError {
		t.Fatal("expected an error result when the first leg fails")
	}
	if got := planStatusOf(t, store, id); got != "active" {
		t.Fatalf("plan must stay active (original position is still open), got %q", got)
	}
	e := latestExec(t, store, id, "resize_")
	if e.State != string(deltaneutral.ExecutionStateFailed) || e.ErrorMsg == "" {
		t.Fatalf("resize execution must be failed with the error message, got state=%q msg=%q", e.State, e.ErrorMsg)
	}
	if len(spot.orders) != 0 {
		t.Fatalf("second leg must not be placed, got %d spot orders", len(spot.orders))
	}
}

func TestResize_SecondLegFails_PlanRecoveryRequired(t *testing.T) {
	store := setupTempDeltaNeutralStore(t)
	defer store.Close()
	spot := &dnFakeSpot{id: "dnfake", baseFree: 10, orderErr: true}
	fut := &dnFakeFutures{mockFuturesProvider: &mockFuturesProvider{}}
	installDNFakes(t, spot, fut)
	id := seedDNFakePlan(t, store, "resize-secondfail", "active")

	res := NewResizeDeltaNeutralPositionTool(leverageOnCfg(), store).
		Execute(context.Background(), map[string]any{"plan_id": float64(id), "delta_notional_usdt": 1000.0, "confirm": true})
	if !res.IsError {
		t.Fatal("expected an error result when the second leg fails")
	}
	if got := planStatusOf(t, store, id); got != "recovery_required" {
		t.Fatalf("first leg resized + second failed must be recovery_required, got %q", got)
	}
	if e := latestExec(t, store, id, "resize_"); e.State != string(deltaneutral.ExecutionStateRecoveryRequired) {
		t.Fatalf("resize execution must be recovery_required, got %q", e.State)
	}
	if len(fut.orders) != 1 {
		t.Fatalf("first leg should have been placed once, got %d", len(fut.orders))
	}
}

func TestResize_LegSaveFailsAfterFill_PlanRecoveryRequired(t *testing.T) {
	store := setupTempDeltaNeutralStore(t)
	defer store.Close()
	spot := &dnFakeSpot{id: "dnfake", baseFree: 10}
	fut := &dnFakeFutures{mockFuturesProvider: &mockFuturesProvider{}}
	installDNFakes(t, spot, fut)
	calls := failLegSaves(t, filledLegOf(deltaneutral.LegTypeFutures))
	id := seedDNFakePlan(t, store, "resize-savefail", "active")

	res := NewResizeDeltaNeutralPositionTool(leverageOnCfg(), store).
		Execute(context.Background(), map[string]any{"plan_id": float64(id), "delta_notional_usdt": 1000.0, "confirm": true})
	if !res.IsError {
		t.Fatal("expected an error result when a filled leg cannot be recorded")
	}
	if *calls != legSaveMaxAttempts {
		t.Fatalf("leg save should be attempted %d times, got %d", legSaveMaxAttempts, *calls)
	}
	if got := planStatusOf(t, store, id); got != "recovery_required" {
		t.Fatalf("plan must be recovery_required, got %q", got)
	}
	e := latestExec(t, store, id, "resize_")
	if e.State != string(deltaneutral.ExecutionStateRecoveryRequired) ||
		!strings.Contains(e.ErrorMsg, deltaneutral.UnrecordedFillMarker) || !strings.Contains(e.ErrorMsg, "fut-order-1") {
		t.Fatalf("execution must be recovery_required and carry the unrecorded-fill trace, got state=%q msg=%q", e.State, e.ErrorMsg)
	}
	if len(spot.orders) != 1 {
		t.Fatalf("second leg is still placed after a first-leg bookkeeping failure, got %d spot orders", len(spot.orders))
	}
}

// ── Open ─────────────────────────────────────────────────────────────────────

func TestOpen_LegSaveFailsAfterFill_PlanRecoveryRequired_NoReopen(t *testing.T) {
	store := setupTempDeltaNeutralStore(t)
	defer store.Close()
	spot := &dnFakeSpot{id: "dnfake", baseFree: 10}
	fut := &dnFakeFutures{mockFuturesProvider: &mockFuturesProvider{}}
	installDNFakes(t, spot, fut)
	calls := failLegSaves(t, filledLegOf(deltaneutral.LegTypeFutures))
	id := seedDNFakePlan(t, store, "open-orphan", "ready")
	tool := NewOpenDeltaNeutralPositionTool(leverageOnCfg(), store)

	res := tool.Execute(context.Background(), map[string]any{"plan_id": float64(id), "confirm": true})
	if !res.IsError {
		t.Fatal("expected an error result when a filled leg cannot be recorded")
	}
	if *calls != legSaveMaxAttempts {
		t.Fatalf("leg save should be attempted %d times, got %d", legSaveMaxAttempts, *calls)
	}
	if got := planStatusOf(t, store, id); got != "recovery_required" {
		t.Fatalf("a plan with a filled order must be recovery_required, never failed; got %q", got)
	}
	e := latestExec(t, store, id, "attempt_")
	if e.State != string(deltaneutral.ExecutionStateRecoveryRequired) {
		t.Fatalf("execution must be recovery_required, got %q", e.State)
	}
	for _, want := range []string{deltaneutral.UnrecordedFillMarker, "fut-order-1", "BTC/USDT:USDT", "sell"} {
		if !strings.Contains(e.ErrorMsg, want) {
			t.Fatalf("trace must contain %q, got %q", want, e.ErrorMsg)
		}
	}
	if len(fut.orders) != 1 || len(spot.orders) != 1 {
		t.Fatalf("expected exactly one order per leg, got fut=%d spot=%d", len(fut.orders), len(spot.orders))
	}

	// A retry must be refused even if the status were reset to a re-openable one.
	if err := store.UpdatePlanStatus(context.Background(), id, "failed"); err != nil {
		t.Fatalf("reset status: %v", err)
	}
	res = tool.Execute(context.Background(), map[string]any{"plan_id": float64(id), "confirm": true})
	if !res.IsError || !strings.Contains(res.ForLLM, "exposure") {
		t.Fatalf("retry on a plan with an unrecorded fill must be refused for exposure, got: %v", res.ForLLM)
	}
	if len(fut.orders) != 1 || len(spot.orders) != 1 {
		t.Fatalf("retry must not place orders, got fut=%d spot=%d", len(fut.orders), len(spot.orders))
	}
}

func TestOpen_RefusedWhenPlanHasOpenExposure(t *testing.T) {
	for _, status := range []string{"failed", "ready", "draft"} {
		t.Run(status, func(t *testing.T) {
			store := setupTempDeltaNeutralStore(t)
			defer store.Close()
			spot := &dnFakeSpot{id: "dnfake", baseFree: 10}
			fut := &dnFakeFutures{mockFuturesProvider: &mockFuturesProvider{}}
			installDNFakes(t, spot, fut)
			id := seedDNFakePlan(t, store, "open-exposed-"+status, status)
			seedDNOpenLegs(t, store, id, deltaneutral.ExecutionStateRecoveryRequired, 10, 10)
			tool := NewOpenDeltaNeutralPositionTool(leverageOnCfg(), store)

			for _, confirm := range []bool{true, false} {
				res := tool.Execute(context.Background(), map[string]any{"plan_id": float64(id), "confirm": confirm})
				if !res.IsError || !strings.Contains(res.ForLLM, "unwind") {
					t.Fatalf("confirm=%v: open must be refused with an unwind hint, got error=%v msg=%q", confirm, res.IsError, res.ForLLM)
				}
			}
			if len(fut.orders) != 0 || len(spot.orders) != 0 {
				t.Fatalf("no orders may be placed, got fut=%d spot=%d", len(fut.orders), len(spot.orders))
			}
			if got := planStatusOf(t, store, id); got != status {
				t.Fatalf("a refused open must not change the plan status, got %q", got)
			}
		})
	}
}

// ── Unwind ───────────────────────────────────────────────────────────────────

func TestUnwind_CloseFails_ExecutionNotStuckUnwinding(t *testing.T) {
	type tc struct {
		name         string
		spot         *dnFakeSpot
		fut          *dnFakeFutures
		failSaveOf   deltaneutral.LegType // "" = saves succeed
		wantExec     deltaneutral.ExecutionState
		wantPlan     string
		wantSpotSold bool
	}
	cases := []tc{
		{
			name:     "futures close fails",
			spot:     &dnFakeSpot{id: "dnfake", baseFree: 10},
			fut:      &dnFakeFutures{mockFuturesProvider: &mockFuturesProvider{}, orderErr: true},
			wantExec: deltaneutral.ExecutionStateFailed,
			wantPlan: "active", // nothing closed: plan unchanged
		},
		{
			name:     "spot close fails after futures closed",
			spot:     &dnFakeSpot{id: "dnfake", baseFree: 10, orderErr: true},
			fut:      &dnFakeFutures{mockFuturesProvider: &mockFuturesProvider{}},
			wantExec: deltaneutral.ExecutionStateRecoveryRequired,
			wantPlan: "recovery_required",
		},
		{
			name:         "futures leg save fails after a successful close",
			spot:         &dnFakeSpot{id: "dnfake", baseFree: 10},
			fut:          &dnFakeFutures{mockFuturesProvider: &mockFuturesProvider{}},
			failSaveOf:   deltaneutral.LegTypeFutures,
			wantExec:     deltaneutral.ExecutionStateRecoveryRequired,
			wantPlan:     "recovery_required",
			wantSpotSold: true,
		},
		{
			name:       "spot leg save fails after a successful close",
			spot:       &dnFakeSpot{id: "dnfake", baseFree: 10},
			fut:        &dnFakeFutures{mockFuturesProvider: &mockFuturesProvider{}},
			failSaveOf: deltaneutral.LegTypeSpot,
			wantExec:   deltaneutral.ExecutionStateRecoveryRequired,
			wantPlan:   "recovery_required",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := setupTempDeltaNeutralStore(t)
			defer store.Close()
			installDNFakes(t, c.spot, c.fut)
			id := seedDNFakePlan(t, store, "unwind-fail", "active")
			seedDNOpenLegs(t, store, id, deltaneutral.ExecutionStateBothLegsFilled, 10, 10)
			var saveCalls *int
			if c.failSaveOf != "" {
				// Only fail the unwind's own leg rows (opposite side), not the seeded ones.
				saveCalls = failLegSaves(t, func(l *deltaneutral.ExecutionLeg) bool {
					return filledLegOf(c.failSaveOf)(l) && (l.Side == "sell" && l.LegType == string(deltaneutral.LegTypeSpot) ||
						l.Side == "buy" && l.LegType == string(deltaneutral.LegTypeFutures))
				})
			}

			res := NewUnwindDeltaNeutralPositionTool(leverageOnCfg(), store).
				Execute(context.Background(), map[string]any{"plan_id": float64(id), "confirm": true})
			if !res.IsError {
				t.Fatalf("expected an error result, got: %v", res.ForUser)
			}

			assertNoUnwindingExecution(t, store, id)
			e := latestExec(t, store, id, "unwind_")
			if e.State != string(c.wantExec) {
				t.Fatalf("unwind execution state = %q, want %q", e.State, c.wantExec)
			}
			if got := planStatusOf(t, store, id); got != c.wantPlan {
				t.Fatalf("plan status = %q, want %q", got, c.wantPlan)
			}
			if saveCalls != nil {
				if *saveCalls != legSaveMaxAttempts {
					t.Fatalf("leg save attempts = %d, want %d", *saveCalls, legSaveMaxAttempts)
				}
				if !strings.Contains(e.ErrorMsg, deltaneutral.UnrecordedFillMarker) {
					t.Fatalf("execution must carry the unrecorded-fill trace, got %q", e.ErrorMsg)
				}
			}
			if c.wantSpotSold && len(c.spot.orders) != 1 {
				t.Fatalf("spot leg should still be closed after a futures bookkeeping failure, got %d orders", len(c.spot.orders))
			}
			if c.name == "futures close fails" && len(c.spot.orders) != 0 {
				t.Fatalf("spot must not be touched when the futures close fails, got %d orders", len(c.spot.orders))
			}
		})
	}
}

func TestUnwind_RecoveryRequired_UsesNetLegs_NotAccountBalance(t *testing.T) {
	store := setupTempDeltaNeutralStore(t)
	defer store.Close()
	// The account holds far more BTC (25) than this plan bought (10): unrelated holdings.
	spot := &dnFakeSpot{id: "dnfake", baseFree: 25}
	fut := &dnFakeFutures{mockFuturesProvider: &mockFuturesProvider{}} // live position: 50 contracts
	installDNFakes(t, spot, fut)
	id := seedDNFakePlan(t, store, "unwind-net", "recovery_required")
	seedDNOpenLegs(t, store, id, deltaneutral.ExecutionStateRecoveryRequired, 10, 10)
	tool := NewUnwindDeltaNeutralPositionTool(leverageOnCfg(), store)

	// Dry-run shows the recorded quantities.
	dry := tool.Execute(context.Background(), map[string]any{"plan_id": float64(id)})
	if dry.IsError || !strings.Contains(dry.ForUser, "10 base (recorded)") || !strings.Contains(dry.ForUser, "10 tokens (recorded)") {
		t.Fatalf("dry-run must show the recorded quantities:\n%s", dry.ForUser)
	}

	res := tool.Execute(context.Background(), map[string]any{"plan_id": float64(id), "confirm": true})
	if res.IsError {
		t.Fatalf("unwind failed: %v", res.ForLLM)
	}
	if len(spot.orders) != 1 || spot.orders[0].amount != 10 || spot.orders[0].side != "sell" {
		t.Fatalf("spot sell must equal the plan's net leg (10), not the account balance (25): %+v", spot.orders)
	}
	if len(fut.orders) != 1 || fut.orders[0].Amount != 10 || !fut.orders[0].ReduceOnly || fut.orders[0].Side != "buy" {
		t.Fatalf("futures close must equal the plan's net leg (10 of the 50 live contracts), buy, reduce-only: %+v", fut.orders)
	}
	if got := planStatusOf(t, store, id); got != "closed" {
		t.Fatalf("plan status = %q, want closed", got)
	}
	if e := latestExec(t, store, id, "unwind_"); e.State != string(deltaneutral.ExecutionStateUnwound) {
		t.Fatalf("unwind execution = %q, want unwound", e.State)
	}
	// The unwind legs net the plan flat, so it no longer counts as exposed.
	if has, err := store.HasOpenExposure(context.Background(), id); err != nil || has {
		t.Fatalf("after a full unwind HasOpenExposure = %v (err %v), want false", has, err)
	}
}

func TestUnwind_RetryAfterPartialClose_OnlyClosesRemainingLeg(t *testing.T) {
	store := setupTempDeltaNeutralStore(t)
	defer store.Close()
	spot := &dnFakeSpot{id: "dnfake", baseFree: 10, orderErr: true}
	fut := &dnFakeFutures{mockFuturesProvider: &mockFuturesProvider{}}
	installDNFakes(t, spot, fut)
	id := seedDNFakePlan(t, store, "unwind-retry", "active")
	seedDNOpenLegs(t, store, id, deltaneutral.ExecutionStateBothLegsFilled, 10, 10)
	tool := NewUnwindDeltaNeutralPositionTool(leverageOnCfg(), store)

	if res := tool.Execute(context.Background(), map[string]any{"plan_id": float64(id), "confirm": true}); !res.IsError {
		t.Fatal("first unwind should fail on the spot leg")
	}
	if got := planStatusOf(t, store, id); got != "recovery_required" {
		t.Fatalf("plan = %q, want recovery_required", got)
	}

	spot.orderErr = false
	res := tool.Execute(context.Background(), map[string]any{"plan_id": float64(id), "confirm": true})
	if res.IsError {
		t.Fatalf("retry should succeed: %v", res.ForLLM)
	}
	if len(fut.orders) != 1 {
		t.Fatalf("futures leg is already flat and must not be closed again, got %d futures orders", len(fut.orders))
	}
	if len(spot.orders) != 1 || spot.orders[0].amount != 10 {
		t.Fatalf("spot leg should be sold once for 10, got %+v", spot.orders)
	}
	if got := planStatusOf(t, store, id); got != "closed" {
		t.Fatalf("plan = %q, want closed", got)
	}
	assertNoUnwindingExecution(t, store, id)
}

// TestUnwind_FuturesAlreadyClosedExternally_PlanClosedAndFlat: the futures position was
// liquidated/closed by hand, so no close order is placed. The plan may only be marked
// closed if its recorded net really returns to zero, and then it must be deletable.
func TestUnwind_FuturesAlreadyClosedExternally_PlanClosedAndFlat(t *testing.T) {
	store := setupTempDeltaNeutralStore(t)
	defer store.Close()
	spot := &dnFakeSpot{id: "dnfake", baseFree: 10}
	fut := &dnFakeFutures{mockFuturesProvider: &mockFuturesProvider{}, noPosition: true}
	installDNFakes(t, spot, fut)
	id := seedDNFakePlan(t, store, "unwind-futgone", "active")
	seedDNOpenLegs(t, store, id, deltaneutral.ExecutionStateBothLegsFilled, 10, 10)

	res := NewUnwindDeltaNeutralPositionTool(leverageOnCfg(), store).
		Execute(context.Background(), map[string]any{"plan_id": float64(id), "confirm": true})
	if res.IsError {
		t.Fatalf("unwind failed: %v", res.ForLLM)
	}
	if len(fut.orders) != 0 {
		t.Fatalf("no futures order may be placed without a live position, got %d", len(fut.orders))
	}
	if got := planStatusOf(t, store, id); got != "closed" {
		t.Fatalf("plan = %q, want closed", got)
	}
	if has, err := store.HasOpenExposure(context.Background(), id); err != nil || has {
		t.Fatalf("a closed plan must not report open exposure: has=%v err=%v", has, err)
	}
	assertNoUnwindingExecution(t, store, id)
	if res := NewDeleteDeltaNeutralPlanTool(store, newTestCronServiceForDN(t)).
		Execute(context.Background(), map[string]any{"plan_id": float64(id)}); res.IsError {
		t.Fatalf("closed, flat plan must be deletable: %v", res.ForLLM)
	}
}

// TestUnwind_SpotCappedByFreeBalance_PlanStaysRecoveryRequired: only 4 of the recorded 10
// are free (the rest may sit in Earn). The 4 are sold, but the plan must not be closed
// while 6 remain unhedged and unmonitored.
func TestUnwind_SpotCappedByFreeBalance_PlanStaysRecoveryRequired(t *testing.T) {
	store := setupTempDeltaNeutralStore(t)
	defer store.Close()
	spot := &dnFakeSpot{id: "dnfake", baseFree: 4}
	fut := &dnFakeFutures{mockFuturesProvider: &mockFuturesProvider{}}
	installDNFakes(t, spot, fut)
	id := seedDNFakePlan(t, store, "unwind-capped", "active")
	seedDNOpenLegs(t, store, id, deltaneutral.ExecutionStateBothLegsFilled, 10, 10)

	res := NewUnwindDeltaNeutralPositionTool(leverageOnCfg(), store).
		Execute(context.Background(), map[string]any{"plan_id": float64(id), "confirm": true})
	if !res.IsError || !strings.Contains(res.ForLLM, "spot 6") {
		t.Fatalf("expected an 'unwind incomplete' error naming the 6 left over, got error=%v msg=%q", res.IsError, res.ForLLM)
	}
	if len(spot.orders) != 1 || spot.orders[0].amount != 4 {
		t.Fatalf("spot sale must be capped at the 4 free, got %+v", spot.orders)
	}
	if got := planStatusOf(t, store, id); got != "recovery_required" {
		t.Fatalf("plan = %q, want recovery_required", got)
	}
	if e := latestExec(t, store, id, "unwind_"); e.State != string(deltaneutral.ExecutionStateRecoveryRequired) {
		t.Fatalf("unwind execution = %q, want recovery_required", e.State)
	}
	if has, err := store.HasOpenExposure(context.Background(), id); err != nil || !has {
		t.Fatalf("HasOpenExposure = %v (err %v), want true", has, err)
	}
	assertNoUnwindingExecution(t, store, id)
}

func TestUnwind_NoRecordedExposure_RefusesWithManualCloseHint(t *testing.T) {
	for _, status := range []string{"active", "recovery_required"} {
		t.Run(status, func(t *testing.T) {
			store := setupTempDeltaNeutralStore(t)
			defer store.Close()
			spot := &dnFakeSpot{id: "dnfake", baseFree: 25}
			fut := &dnFakeFutures{mockFuturesProvider: &mockFuturesProvider{}}
			installDNFakes(t, spot, fut)
			id := seedDNFakePlan(t, store, "unwind-noexpo", status)
			tool := NewUnwindDeltaNeutralPositionTool(leverageOnCfg(), store)

			res := tool.Execute(context.Background(), map[string]any{"plan_id": float64(id), "confirm": true})
			if !res.IsError {
				t.Fatal("unwind with no recorded exposure must be refused")
			}
			for _, want := range []string{"No recorded exposure", "futures_close_position", "create_order"} {
				if !strings.Contains(res.ForLLM, want) {
					t.Fatalf("refusal must mention %q, got: %s", want, res.ForLLM)
				}
			}
			if len(spot.orders) != 0 || len(fut.orders) != 0 {
				t.Fatalf("no orders may be placed, got fut=%d spot=%d", len(fut.orders), len(spot.orders))
			}
			if len(execsOf(t, store, id)) != 0 {
				t.Fatal("no unwind execution row may be created for a refused unwind")
			}
			if got := planStatusOf(t, store, id); got != status {
				t.Fatalf("plan status changed to %q", got)
			}

			// Dry-run shows the same warning and does not claim confirm=true will work.
			dry := tool.Execute(context.Background(), map[string]any{"plan_id": float64(id)})
			if dry.IsError {
				t.Fatalf("dry-run must be a review, not an error: %v", dry.ForLLM)
			}
			if !strings.Contains(dry.ForUser, "No recorded exposure") || !strings.Contains(dry.ForUser, "futures_close_position") ||
				!strings.Contains(dry.ForUser, "REFUSED") {
				t.Fatalf("dry-run must carry the same warning:\n%s", dry.ForUser)
			}
		})
	}
}

func TestUnwind_FailedPlanWithExposure_Allowed(t *testing.T) {
	store := setupTempDeltaNeutralStore(t)
	defer store.Close()
	spot := &dnFakeSpot{id: "dnfake", baseFree: 10}
	fut := &dnFakeFutures{mockFuturesProvider: &mockFuturesProvider{}}
	installDNFakes(t, spot, fut)
	id := seedDNFakePlan(t, store, "unwind-failed-expo", "failed")
	seedDNOpenLegs(t, store, id, deltaneutral.ExecutionStateRecoveryRequired, 10, 10)

	res := NewUnwindDeltaNeutralPositionTool(leverageOnCfg(), store).
		Execute(context.Background(), map[string]any{"plan_id": float64(id), "confirm": true})
	if res.IsError {
		t.Fatalf("a failed plan with recorded exposure must be unwindable: %v", res.ForLLM)
	}
	if got := planStatusOf(t, store, id); got != "closed" {
		t.Fatalf("plan = %q, want closed", got)
	}
	if len(spot.orders) != 1 || len(fut.orders) != 1 {
		t.Fatalf("both legs should be closed once, got fut=%d spot=%d", len(fut.orders), len(spot.orders))
	}
}

func TestUnwind_FailedPlanWithoutExposure_Rejected(t *testing.T) {
	store := setupTempDeltaNeutralStore(t)
	defer store.Close()
	spot := &dnFakeSpot{id: "dnfake", baseFree: 10}
	fut := &dnFakeFutures{mockFuturesProvider: &mockFuturesProvider{}}
	installDNFakes(t, spot, fut)
	id := seedDNFakePlan(t, store, "unwind-failed-flat", "failed")

	res := NewUnwindDeltaNeutralPositionTool(leverageOnCfg(), store).
		Execute(context.Background(), map[string]any{"plan_id": float64(id), "confirm": true})
	if !res.IsError || !strings.Contains(res.ForLLM, "Cannot unwind") {
		t.Fatalf("a failed plan without exposure must be rejected, got error=%v msg=%q", res.IsError, res.ForLLM)
	}
	if len(spot.orders) != 0 || len(fut.orders) != 0 {
		t.Fatal("no orders may be placed")
	}
}

func TestUnwind_UnrecordedFill_RefusesToActOnUnreliableRecord(t *testing.T) {
	store := setupTempDeltaNeutralStore(t)
	defer store.Close()
	spot := &dnFakeSpot{id: "dnfake", baseFree: 10}
	fut := &dnFakeFutures{mockFuturesProvider: &mockFuturesProvider{}}
	installDNFakes(t, spot, fut)
	failLegSaves(t, filledLegOf(deltaneutral.LegTypeFutures))
	id := seedDNFakePlan(t, store, "unwind-unrecorded", "ready")

	// Open: futures fills but is never recorded; spot is recorded.
	if res := NewOpenDeltaNeutralPositionTool(leverageOnCfg(), store).
		Execute(context.Background(), map[string]any{"plan_id": float64(id), "confirm": true}); !res.IsError {
		t.Fatal("expected the open to report the unrecorded fill")
	}
	futOrders, spotOrders := len(fut.orders), len(spot.orders)

	res := NewUnwindDeltaNeutralPositionTool(leverageOnCfg(), store).
		Execute(context.Background(), map[string]any{"plan_id": float64(id), "confirm": true})
	if !res.IsError || !strings.Contains(res.ForLLM, "never recorded") || !strings.Contains(res.ForLLM, "fut-order-1") {
		t.Fatalf("unwind must refuse and name the unrecorded order, got: %v", res.ForLLM)
	}
	if len(fut.orders) != futOrders || len(spot.orders) != spotOrders {
		t.Fatal("a refused unwind must not place orders")
	}
	if got := planStatusOf(t, store, id); got != "recovery_required" {
		t.Fatalf("plan must stay recovery_required, got %q", got)
	}
	assertNoUnwindingExecution(t, store, id)
}

// ── Delete ───────────────────────────────────────────────────────────────────

func TestDeletePlan_RefusedWithOpenExposure(t *testing.T) {
	store := setupTempDeltaNeutralStore(t)
	defer store.Close()
	cronSvc := newTestCronServiceForDN(t)
	tool := NewDeleteDeltaNeutralPlanTool(store, cronSvc)

	for _, status := range []string{"recovery_required", "failed"} {
		id := seedDNPlan(t, store, "del-expo-"+status, status)
		seedDNOpenLegs(t, store, id, deltaneutral.ExecutionStateRecoveryRequired, 10, 10)
		res := tool.Execute(context.Background(), map[string]any{"plan_id": float64(id)})
		if !res.IsError || !strings.Contains(res.ForLLM, "unwind") {
			t.Fatalf("%s plan with exposure must not be deletable (unwind hint), got error=%v msg=%q", status, res.IsError, res.ForLLM)
		}
		if _, err := store.GetPlan(context.Background(), id); err != nil {
			t.Fatalf("%s plan must still exist after a refused delete: %v", status, err)
		}
		if execs := execsOf(t, store, id); len(execs) == 0 {
			t.Fatalf("%s plan's leg history must be intact", status)
		}
	}

	// closed / archived plans are exempt even when their legs do not net to zero (legacy
	// data written before unwind recorded offsets).
	for _, status := range []string{"closed", "archived"} {
		id := seedDNPlan(t, store, "del-legacy-"+status, status)
		seedDNOpenLegs(t, store, id, deltaneutral.ExecutionStateBothLegsFilled, 10, 10)
		if res := tool.Execute(context.Background(), map[string]any{"plan_id": float64(id)}); res.IsError {
			t.Fatalf("legacy %s plan must be deletable: %v", status, res.ForLLM)
		}
	}

	// Flat plans (exposure fully unwound) remain deletable.
	flat := seedDNPlan(t, store, "del-flat", "closed")
	seedDNOpenLegs(t, store, flat, deltaneutral.ExecutionStateBothLegsFilled, 10, 10)
	now := time.Now().UTC()
	uid, err := store.SaveExecution(context.Background(), &deltaneutral.Execution{
		PlanID: flat, AttemptID: "unwind_seed", State: string(deltaneutral.ExecutionStateUnwound),
		RequestedAt: now, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("seed unwind exec: %v", err)
	}
	for _, l := range []*deltaneutral.ExecutionLeg{
		{LegType: "futures", Side: "buy", FilledQuantity: 10},
		{LegType: "spot", Side: "sell", FilledQuantity: 9.99}, // fee dust: sold slightly less than bought
	} {
		l.ExecutionID, l.State, l.OrderType, l.CreatedAt, l.UpdatedAt = uid, "filled", "market", now, now
		if _, err := store.SaveExecutionLeg(context.Background(), l); err != nil {
			t.Fatalf("seed unwind leg: %v", err)
		}
	}
	if res := tool.Execute(context.Background(), map[string]any{"plan_id": float64(flat)}); res.IsError {
		t.Fatalf("a fully unwound plan must be deletable: %v", res.ForLLM)
	}
}

// TestUnwind_RecordsRealisedLoss: the futures realised-loss feed (daily loss limit) must
// run whenever a futures close order actually filled - including when its leg record
// cannot be saved - and never when no order was placed.
func TestUnwind_RecordsRealisedLoss(t *testing.T) {
	loss := -100.0
	cases := []struct {
		name     string
		noPos    bool
		failSave bool
		want     float64
	}{
		{"filled and recorded", false, false, 20}, // 10 of 50 live contracts: 100 * 10/50
		{"filled but leg record unsaved", false, true, 20},
		{"no order placed (position gone)", true, false, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tr := swapLossTracker(t)
			store := setupTempDeltaNeutralStore(t)
			defer store.Close()
			installDNFakes(t, &dnFakeSpot{id: "dnfake", baseFree: 10},
				&dnFakeFutures{mockFuturesProvider: &mockFuturesProvider{}, pnl: &loss, noPosition: c.noPos})
			if c.failSave {
				failLegSaves(t, filledLegOf(deltaneutral.LegTypeFutures))
			}
			id := seedDNFakePlan(t, store, "unwind-loss", "active")
			seedDNOpenLegs(t, store, id, deltaneutral.ExecutionStateBothLegsFilled, 10, 10)

			NewUnwindDeltaNeutralPositionTool(leverageOnCfg(), store).
				Execute(context.Background(), map[string]any{"plan_id": float64(id), "confirm": true})
			if got := tr.TodayLossUSD(); got != c.want {
				t.Fatalf("recorded loss = %v, want %v", got, c.want)
			}
		})
	}
}
