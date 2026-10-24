package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"sync/atomic"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral"
	"github.com/cryptoquantumwave/khunquant/pkg/journal"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// OpenDeltaNeutralPositionTool opens a delta-neutral position: it checks the plan,
// shows the dry-run, and on confirm starts the journaled two-leg workflow dn.open
// (futures hedge + spot buy; see pkg/deltaneutral/workflows.go).
type OpenDeltaNeutralPositionTool struct {
	cfg   *config.Config
	store *deltaneutral.Store
}

func NewOpenDeltaNeutralPositionTool(cfg *config.Config, store *deltaneutral.Store) *OpenDeltaNeutralPositionTool {
	return &OpenDeltaNeutralPositionTool{cfg: cfg, store: store}
}

func (t *OpenDeltaNeutralPositionTool) Name() string { return NameOpenDeltaNeutralPosition }

func (t *OpenDeltaNeutralPositionTool) Description() string {
	return "Open a delta-neutral position via a two-leg execution: futures hedge (short) + spot buy. " +
		"Requires explicit approval (confirm=true). Runs comprehensive safety gates (leverage opt-in, permission, daily-loss, rate limit). " +
		"Dry-run mode (confirm=false) shows execution review without placing orders. HIGHEST-RISK tool — cross-exchange introduces slippage risk."
}

func (t *OpenDeltaNeutralPositionTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"plan_id": map[string]any{
				"type":        "integer",
				"description": "ID of the delta-neutral plan to open. Accepts draft, ready, or failed (retry).",
			},
			"confirm": map[string]any{
				"type":        "boolean",
				"description": "Must be true to execute. Use false for dry-run review.",
			},
		},
		"required": []string{"plan_id"},
	}
}

func (t *OpenDeltaNeutralPositionTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	// Opening runs the owner's plan with real money: the owner's own turn only, and before
	// the plan is read, so a refused turn learns nothing and the dry-run is the owner's too.
	if res := refuseNonOwnerExecution(ctx, NameOpenDeltaNeutralPosition, "open a delta-neutral position"); res != nil {
		return res
	}
	planID, _ := args["plan_id"].(float64)
	confirm, _ := args["confirm"].(bool)

	if planID <= 0 {
		return ErrorResult("plan_id must be a positive integer")
	}

	// Load the plan
	plan, err := t.store.GetPlan(ctx, int64(planID))
	if err != nil {
		return ErrorResult(fmt.Sprintf("cannot load plan %d: %v", int64(planID), err))
	}

	// Allow opening from draft, ready, or failed (retry/restart). Other states are invalid.
	switch plan.Status {
	case deltaneutral.PlanStatusDraft, deltaneutral.PlanStatusReady, deltaneutral.PlanStatusFailed:
		// ok
	default:
		return ErrorResult(fmt.Sprintf("plan status is %q; only draft, ready, or failed plans can be opened.", plan.Status))
	}

	// Never open on top of an existing position, whatever the plan status says: a plan
	// that failed or was left half-open can still hold real exposure, and a retry would
	// stack a second position on it. The check fails closed.
	hasExposure, err := t.store.HasOpenExposure(ctx, plan.ID)
	if err != nil {
		return ErrorResult(fmt.Sprintf("cannot verify that plan %d has no open exposure: %v. Refusing to open.", plan.ID, err))
	}
	if hasExposure {
		return ErrorResult(fmt.Sprintf(
			"plan %d (status %q) still has open or unreconciled exposure from a previous attempt. "+
				"Refusing to open another position on top of it. Run unwind_delta_neutral_position first "+
				"(or close the position manually on the exchange if unwind reports no recorded exposure).",
			plan.ID, plan.Status))
	}

	// --- Safety gates (sequence from futures.go §225-290) ---

	// Gate 1: leverage opt-in
	if err := broker.CheckLeverage(t.cfg, "open delta-neutral futures leg", broker.RiskIncrease); err != nil {
		return ErrorResult(err.Error())
	}

	// Gate 2: permission for futures leg
	if err := broker.CheckPermission(t.cfg, plan.FuturesProvider, plan.FuturesAccount, config.ScopeTrade); err != nil {
		return ErrorResult(fmt.Sprintf("futures leg permission denied: %v", err))
	}

	// Gate 2b: permission for spot leg
	if err := broker.CheckPermission(t.cfg, plan.SpotProvider, plan.SpotAccount, config.ScopeTrade); err != nil {
		return ErrorResult(fmt.Sprintf("spot leg permission denied: %v", err))
	}

	// Gate 3: daily loss limit
	lossLimit := t.cfg.TradingRisk.DailyLossLimitUSD
	for _, key := range []string{
		broker.AccountKey(t.cfg, plan.FuturesProvider, plan.FuturesAccount),
		broker.AccountKey(t.cfg, plan.SpotProvider, plan.SpotAccount),
	} {
		if err := broker.GlobalLossTracker.CheckDailyLossFor(lossLimit, key); err != nil {
			return ErrorResult(err.Error())
		}
	}

	// Gate 4: per-order value cap, applied to each leg's notional. Opening adds exposure,
	// so this is a risk-adding gate and sits before the dry-run so a preview tells the truth.
	legNotional := math.Max(plan.SpotNotionalUSDT, plan.FuturesNotionalUSDT)
	if err := broker.CheckRiskNotional(t.cfg, legNotional); err != nil {
		return ErrorResult(fmt.Sprintf("delta-neutral leg: %v", err))
	}

	// The rate limit is taken after the dry-run return, right before the first order.

	// --- Entry spread gate ---
	// Fetch the live spread once and reuse it for both the gate and the dry-run
	// review. When a minimum is configured the gate fails CLOSED: if the live
	// spread cannot be determined we block rather than silently allowing entry.
	liveSpread, _, _, spreadOK, spreadDetail := t.fetchLiveEntrySpread(ctx, plan)
	if plan.EntryRules.MinEntrySpreadPct != 0 {
		if !spreadOK {
			return ErrorResult(fmt.Sprintf(
				"⛔ Entry spread gate: a minimum spread of %.4f%% is required but the live spread "+
					"could not be determined (%s). Refusing to open — retry once market data is available, "+
					"or clear entry_rules.min_entry_spread_pct to disable the gate.",
				plan.EntryRules.MinEntrySpreadPct, spreadDetail))
		}
		if liveSpread < plan.EntryRules.MinEntrySpreadPct {
			return ErrorResult(fmt.Sprintf(
				"⛔ Entry spread gate: current spread %.4f%% is below minimum %.4f%%.\n"+
					"Use update_delta_neutral_plan to adjust entry_rules.min_entry_spread_pct, or wait for the spread to improve.",
				liveSpread, plan.EntryRules.MinEntrySpreadPct))
		}
	}

	// --- Dry-run gate (§7.8) ---
	if !confirm {
		review := formatExecutionReview(plan)

		// Show live spread in dry-run output (reusing the value fetched above).
		minStr := "disabled"
		if plan.EntryRules.MinEntrySpreadPct != 0 {
			minStr = fmt.Sprintf("%.4f%%", plan.EntryRules.MinEntrySpreadPct)
		}
		if spreadOK {
			review += fmt.Sprintf("\nLive Entry Spread: %.4f%% (min required: %s)", liveSpread, minStr)
		} else {
			review += fmt.Sprintf("\nLive Entry Spread: unavailable (%s) (min required: %s)", spreadDetail, minStr)
		}

		if plan.CrossExchange {
			review += "\n[CROSS-EXCHANGE WARNING] Spot and futures on different exchanges — slippage and timing risk."
		}
		review += "\n\nSet confirm=true to execute."
		return UserResult(review)
	}

	// --- On confirm: start the journaled workflow dn.open (spec MR-309) ---
	//
	// The open is a durable workflow, not a function of this call: a crash between the
	// legs resumes without a second order. The tool starts it, waits up to dnOpenWait
	// and reports; the validation, the gates, the legs and the bookkeeping all
	// happen in the workflow, which re-checks the plan and the exposure itself.
	w := currentDNWorkflows()
	if w == nil {
		return ErrorResult("the workflow engine is not running, so a two-leg open cannot be made durable. " +
			"Refusing to open: a crash between the legs could not be resumed. Nothing was placed.")
	}
	id, err := w.Start(ctx, deltaneutral.KindOpen, deltaneutral.OpenInput{
		PlanID: plan.ID, FirstLeg: deltaneutral.FirstLegType(false),
		Origin: ToolOrigin(ctx), Sender: ToolSender(ctx),
	})
	if err != nil {
		return ErrorResult(fmt.Sprintf("could not start the open workflow: %v. Nothing was placed.", err))
	}
	wctx, cancel := context.WithTimeout(ctx, dnOpenWait)
	defer cancel()
	wf, err := w.Wait(wctx, id)
	if err != nil {
		return UserResult(fmt.Sprintf("Opening plan %d is in progress (workflow %s); you will get a message when it finishes.", plan.ID, id))
	}
	return t.openResult(ctx, w, plan, id, wf)
}

// openResult turns the finished workflow into the tool's answer.
func (t *OpenDeltaNeutralPositionTool) openResult(ctx context.Context, w DNWorkflows, plan *deltaneutral.Plan, id string, wf journal.Workflow) *ToolResult {
	var res deltaneutral.OpenResult
	steps, err := w.Steps(ctx, id)
	for _, s := range steps {
		if s.Name == "finalize" && len(s.Output) > 0 {
			_ = json.Unmarshal(s.Output, &res)
		}
	}
	if err != nil || res.Result == "" {
		return ErrorResult(fmt.Sprintf(
			"The open workflow %s ended %s without a result%s. Check the plan's executions and the exchange positions before doing anything else; "+
				"the plan may be recovery_required.", id, wf.Status, mapJoin(": ", wf.Error)))
	}
	switch res.Result {
	case deltaneutral.ResultOpened:
		return UserResult(fmt.Sprintf(
			"Delta-neutral position successfully opened:\n"+
				"  Plan:       %s (ID %d)\n"+
				"  Attempt:    %s\n"+
				"  Futures:    %s %s on %s\n"+
				"  Spot:       %s %s on %s\n"+
				"  Status:     active\n",
			plan.Name, plan.ID, res.AttemptID,
			plan.FuturesSide, plan.FuturesSymbol, plan.FuturesProvider,
			plan.SpotSide, plan.SpotSymbol, plan.SpotProvider))
	case deltaneutral.ResultRefused:
		out := ErrorResult(res.Message + ". Nothing was placed.")
		if strings.Contains(res.Message, broker.ErrRateLimited.Error()) {
			out = out.WithError(broker.ErrRateLimited)
		}
		return out
	case deltaneutral.ResultNotOpened:
		return ErrorResult(fmt.Sprintf("%s. Second leg not placed. Position not opened.", res.Message))
	case deltaneutral.ResultUnrecorded:
		return ErrorResult(fmt.Sprintf(
			"CRITICAL: both legs were filled on the exchange but a leg record could not be saved (%s). "+
				"Plan set to recovery_required and will NOT be re-opened. Verify the positions on the exchange; "+
				"unwind cannot size an unrecorded leg, so close it manually (futures_close_position / create_order).", res.Message))
	}
	return ErrorResult(fmt.Sprintf(
		"CRITICAL: the open did not complete and a position may be UNHEDGED: %s. "+
			"Plan set to recovery_required (it will NOT be re-opened). A recovery has started: it reads what is really open on the exchange "+
			"and asks you to approve closing exactly the orphaned quantity (reduce-only). Nothing is closed without your approval; "+
			"you can also run unwind_delta_neutral_position.", res.Message))
}

func mapJoin(sep, s string) string {
	if s == "" {
		return ""
	}
	return sep + s
}

// dnOpenWait is how long the tool waits for the workflow before it answers "in progress".
var dnOpenWait = deltaneutral.OpenWait

// DNWorkflows is the part of the journal engine the delta-neutral open tool uses.
// *journal.Engine satisfies it.
type DNWorkflows interface {
	Start(ctx context.Context, kind string, input any) (string, error)
	Wait(ctx context.Context, id string) (journal.Workflow, error)
	Steps(ctx context.Context, id string) ([]journal.StepRecord, error)
}

// dnWorkflowsHolder is the late-bound engine, like the approval proposer: the tool is
// registered before the engine exists, the gateway sets it on every build and reload
// and clears it when the services stop.
var dnWorkflowsHolder atomic.Pointer[DNWorkflows]

// SetDNWorkflows attaches the engine to open_delta_neutral_position; nil detaches it.
func SetDNWorkflows(w DNWorkflows) {
	if w == nil {
		dnWorkflowsHolder.Store(nil)
		return
	}
	dnWorkflowsHolder.Store(&w)
}

func currentDNWorkflows() DNWorkflows {
	if w := dnWorkflowsHolder.Load(); w != nil {
		return *w
	}
	return nil
}

// formatExecutionReview formats the execution review for dry-run output (§7.8).
// fetchLiveEntrySpread fetches the current entry spread (futures mark vs spot last)
// for a plan. Returns the spread %, the underlying prices, ok=true on success, and a
// human-readable detail string describing the failure when ok=false.
func (t *OpenDeltaNeutralPositionTool) fetchLiveEntrySpread(ctx context.Context, plan *deltaneutral.Plan) (spread, markPrice, spotPrice float64, ok bool, detail string) {
	sp, spErr := broker.CreateProviderForAccount(plan.SpotProvider, plan.SpotAccount, t.cfg)
	if spErr != nil {
		return 0, 0, 0, false, fmt.Sprintf("spot provider: %v", spErr)
	}
	md, isMD := sp.(broker.MarketDataProvider)
	if !isMD {
		return 0, 0, 0, false, "spot provider does not support market data"
	}
	ticker, tickerErr := md.FetchTicker(ctx, plan.SpotSymbol)
	if tickerErr != nil || ticker.Last == nil || *ticker.Last <= 0 {
		return 0, 0, 0, false, fmt.Sprintf("spot price unavailable: %v", tickerErr)
	}
	spotPrice = *ticker.Last
	fp, fpErr := futuresProvider(ctx, t.cfg, plan.FuturesProvider, plan.FuturesAccount)
	if fpErr != nil {
		return 0, 0, 0, false, fmt.Sprintf("futures provider: %v", fpErr)
	}
	mp, mpErr := fp.FetchFuturesMarkPrice(ctx, plan.FuturesSymbol)
	if mpErr != nil || mp <= 0 {
		return 0, 0, 0, false, fmt.Sprintf("futures mark price unavailable: %v", mpErr)
	}
	markPrice = mp
	spread = deltaneutral.EntrySpreadPct(markPrice, spotPrice)
	return spread, markPrice, spotPrice, true, ""
}

func formatExecutionReview(plan *deltaneutral.Plan) string {
	return fmt.Sprintf(
		"Execution review (DRY-RUN):\n"+
			"  Plan:                    %s (ID %d)\n"+
			"  Status:                  %s\n\n"+
			"Leg 1 (Futures Hedge):\n"+
			"  Provider/Account:        %s / %s\n"+
			"  Symbol:                  %s\n"+
			"  Side:                    %s\n"+
			"  Notional (USDT):         %.2f\n"+
			"  Leverage:                %d\n"+
			"  Order Type:              market\n\n"+
			"Leg 2 (Spot Buy):\n"+
			"  Provider/Account:        %s / %s\n"+
			"  Symbol:                  %s\n"+
			"  Side:                    %s\n"+
			"  Notional (USDT):         %.2f\n"+
			"  Order Type:              market\n\n"+
			"Estimated Costs:\n"+
			"  Entry Cost (USDT):       %.2f\n"+
			"  Slippage Buffer:         (market orders)\n"+
			"  Delta Target:             0.00 (fully hedged)\n"+
			"  Liquidation Buffer:      %.2f%%\n",
		plan.Name, plan.ID, plan.Status,
		plan.FuturesProvider, plan.FuturesAccount,
		plan.FuturesSymbol, plan.FuturesSide, plan.FuturesNotionalUSDT, plan.FuturesLeverage,
		plan.SpotProvider, plan.SpotAccount,
		plan.SpotSymbol, plan.SpotSide, plan.SpotNotionalUSDT,
		plan.EstimatedEntryCostUSDT,
		plan.RiskPolicy.MinLiquidationDistancePct)
}
