package gateway

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/cron"
	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral"
	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral/fees"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

// handleDeltaNeutralMonitorJob is the cron dispatcher for delta-neutral monitor plans (job name prefix "dn:").
//
// It fetches live state for both legs (spot and futures), evaluates health deterministically in pure Go,
// saves a snapshot, and escalates to the LLM if thresholds are breached.
// Alerts are always delivered via the message bus if a breach occurs, even if cronTool is nil.
func handleDeltaNeutralMonitorJob(
	ctx context.Context,
	job *cron.CronJob,
	cfg *config.Config,
	dnStore *deltaneutral.Store,
	cronTool *tools.CronTool,
	msgBus *bus.MessageBus,
) (string, error) {
	// Parse plan ID from "dn:<id>:<name>".
	parts := strings.SplitN(job.Name, ":", 3)
	if len(parts) < 2 {
		return "", fmt.Errorf("dn-monitor: malformed job name %q", job.Name)
	}
	planID, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return "", fmt.Errorf("dn-monitor: parse plan_id from %q: %w", job.Name, err)
	}

	plan, err := dnStore.GetPlan(ctx, planID)
	if err != nil {
		logger.DebugCF("dn-monitor", "Plan not found, skipping", map[string]any{
			"plan_id": planID, "job": job.Name, "error": err.Error(),
		})
		return "plan not found", nil
	}

	// Skip plans that are not monitorable: disabled, not yet opened (draft, ready: no legs
	// exist on the exchange, so a run would produce 100% delta drift and bogus alerts),
	// permanently closed, or failed with no open exposure (see deltaneutral.Monitorable).
	if ok, why := deltaneutral.Monitorable(ctx, plan, dnStore); !ok {
		logger.DebugCF("dn-monitor", "Plan skipped: "+why, map[string]any{"plan_id": planID, "status": plan.Status})
		return why, nil
	}

	// Read both legs and evaluate their health (pkg/deltaneutral.Monitor).
	mon := &deltaneutral.Monitor{Providers: deltaneutral.BrokerResolver(cfg), Booked: bookedFromStore(dnStore)}
	res, err := mon.Check(ctx, *plan)
	if err != nil {
		return "", fmt.Errorf("dn-monitor: plan %d: %w", planID, err)
	}
	eval, now := res.Eval, res.Snapshot.CreatedAt

	// Always save snapshot.
	snapID, err := dnStore.SaveSnapshot(ctx, &eval.Snapshot)
	if err != nil {
		logger.ErrorCF("dn-monitor", "Failed to save snapshot", map[string]any{
			"plan_id": planID, "error": err.Error(),
		})
		// Continue; don't fail the job.
	}

	go refreshPlanFees(context.Background(), plan, dnStore, cfg)

	// If no breach, return early.
	if !eval.ThresholdBreached {
		logger.DebugCF("dn-monitor", "No breach detected", map[string]any{
			"plan_id": planID,
			"health":  eval.Snapshot.HealthLabel,
			"score":   eval.Snapshot.HealthScore,
		})
		return "ok: no breach", nil
	}

	// Check alert cooldown — skip alert + LLM if all breach codes are still silenced.
	silences, _ := dnStore.GetActiveAlertSilences(ctx, planID)
	unsilenced := filterUnsilencedCodes(eval.BreachCodes, silences)
	if len(unsilenced) == 0 {
		logger.DebugCF("dn-monitor", "All breach codes within cooldown, skipping alert", map[string]any{
			"plan_id": planID, "codes": eval.BreachCodes,
		})
		return "silenced: cooldown active for all breach codes", nil
	}

	// Breach detected: build and save alert.
	logger.WarnCF("dn-monitor", "Threshold breached", map[string]any{
		"plan_id":  planID,
		"codes":    eval.BreachCodes,
		"severity": eval.Severity,
	})

	alertCode := "breach"
	if len(eval.BreachCodes) > 0 {
		alertCode = eval.BreachCodes[0]
	}

	alertMsg := fmt.Sprintf("Delta-neutral plan %d (%s) health alert: %v", planID, plan.Name, eval.BreachCodes)

	alert := &deltaneutral.Alert{
		PlanID:            planID,
		SnapshotID:        &snapID,
		TriggeredAt:       now,
		Severity:          eval.Severity,
		Code:              alertCode,
		Message:           alertMsg,
		RecommendedAction: eval.RecommendedAction,
		AgentInvoked:      false,
		DeliveredChannel:  plan.NotifyChannel,
		DeliveredChatID:   plan.NotifyChatID,
		CreatedAt:         now,
	}

	if _, err := dnStore.SaveAlert(ctx, alert); err != nil {
		logger.ErrorCF("dn-monitor", "Failed to save alert", map[string]any{
			"plan_id": planID, "error": err.Error(),
		})
		// Continue; deliver it anyway.
	}

	// Deliver alert via message bus if a channel is configured.
	if plan.NotifyChannel != "" {
		// Use a short timeout to avoid blocking the cron job.
		deliverCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		err := msgBus.PublishOutbound(deliverCtx, bus.OutboundMessage{
			Channel: plan.NotifyChannel,
			ChatID:  plan.NotifyChatID,
			Content: alertMsg,
		})
		if err != nil {
			logger.WarnCF("dn-monitor", "Failed to deliver alert via message bus", map[string]any{
				"plan_id": planID, "channel": plan.NotifyChannel, "error": err.Error(),
			})
		} else {
			logger.DebugCF("dn-monitor", "Alert delivered", map[string]any{
				"plan_id": planID, "channel": plan.NotifyChannel,
			})
		}
	}

	// Auto-silence non-critical breach codes for the plan's AlertCooldownDuration so the
	// same condition doesn't invoke the LLM again until the window expires. Critical codes
	// (liquidation_distance_low, margin_critical, data_unavailable) are NEVER silenced —
	// they can keep worsening under the same code, so they must re-alert on every tick.
	var silenceable []string
	for _, c := range eval.BreachCodes {
		if !deltaneutral.IsCriticalBreachCode(c) {
			silenceable = append(silenceable, c)
		}
	}
	if len(silenceable) > 0 {
		silenceUntil := processNow().Add(parseSilenceDuration(plan.RiskPolicy.AlertCooldownDuration))
		if siErr := dnStore.UpsertAlertSilences(ctx, planID, silenceable, silenceUntil); siErr != nil {
			logger.WarnCF("dn-monitor", "Failed to set alert silences", map[string]any{
				"plan_id": planID, "error": siErr.Error(),
			})
		}
	}

	// If cronTool is available, escalate to LLM for agent explanation.
	if cronTool != nil {
		return cronTool.ExecuteJob(ctx, job)
	}

	// Alert delivered but no agent explanation.
	return "breach alerted (no agent)", nil
}

// bookedFromStore reads what a plan has booked so far (funding received plus trading
// fees, positive is income) from its latest fee snapshot, for the monitor's profit and
// loss. A plan with no snapshot yet (refreshPlanFees writes them from the plan's first
// run after it opened) has none, and its profit and loss stays unknown.
func bookedFromStore(s *deltaneutral.Store) func(ctx context.Context, planID int64) (float64, bool) {
	return func(ctx context.Context, planID int64) (float64, bool) {
		snap, err := s.GetLatestPlanFeeSnapshot(ctx, planID)
		if err != nil || snap == nil {
			return 0, false
		}
		return snap.TradingFeeUSDT + snap.FundingFeeUSDT, true
	}
}

// filterUnsilencedCodes returns codes not present in the active silences map.
func filterUnsilencedCodes(codes []string, silences map[string]time.Time) []string {
	var out []string
	for _, code := range codes {
		if _, silenced := silences[code]; !silenced {
			out = append(out, code)
		}
	}
	return out
}

// parseSilenceDuration converts a cooldown duration string to time.Duration.
// Valid values: "1h", "4h", "8h", "1d", "3d". Defaults to 1h.
func parseSilenceDuration(s string) time.Duration {
	switch s {
	case "4h":
		return 4 * time.Hour
	case "8h":
		return 8 * time.Hour
	case "1d":
		return 24 * time.Hour
	case "3d":
		return 3 * 24 * time.Hour
	default:
		return time.Hour
	}
}

// refreshPlanFees saves a fee snapshot for the plan. Skips if the last fetch was
// within 30 minutes or if the plan has not yet been executed (OpenedAt == nil).
//
// Trading fee source: our own execution leg records (fee_usdt column), summed per plan.
// This is immune to OKX's two failure modes:
//   - positions-history fee = full lifetime fee of that position, not window-scoped
//   - cross-margin positions accumulate fee across add/reduce cycles from before plan.OpenedAt
//
// Funding fee source: OKX positions API — funding fees are not captured in order
// responses so the exchange API is the only source.
func refreshPlanFees(ctx context.Context, plan *deltaneutral.Plan, store *deltaneutral.Store, cfg *config.Config) {
	if plan.OpenedAt == nil {
		return
	}

	const staleness = 30 * time.Minute
	if last, err := store.GetLatestPlanFeeSnapshot(ctx, plan.ID); err == nil && last != nil {
		if processNow().Sub(last.FetchedAt) < staleness {
			return
		}
	}

	// Trading fee: sum fee_usdt from all filled execution legs for this plan.
	// Negative values are costs (fees paid); positive are rebates.
	tradingFee, err := store.SumPlanExecutionFees(ctx, plan.ID)
	if err != nil {
		logger.WarnCF("dn-fees", "sum execution fees failed", map[string]any{"plan_id": plan.ID, "error": err.Error()})
		return
	}

	// Funding fee: fetch from exchange — the only reliable source.
	// Non-fatal: snapshot is saved with fundingFee=0 if the provider is unsupported.
	var fundingFee float64
	fetcher, fetcherErr := fees.NewFeesFetcher(plan.FuturesProvider, plan.FuturesAccount, cfg)
	if fetcherErr == nil {
		if pf, pfErr := fetcher.FetchFuturesPositionFees(ctx, fees.FetchFeesRequest{
			FuturesSymbol: plan.FuturesSymbol,
			Since:         *plan.OpenedAt,
			Until:         processNow().UTC(),
		}); pfErr == nil {
			fundingFee = pf.FundingFeeUSDT
		} else {
			logger.WarnCF("dn-fees", "funding fee fetch failed", map[string]any{"plan_id": plan.ID, "error": pfErr.Error()})
		}
	}

	now := processNow().UTC()
	until := now
	if _, err := store.SavePlanFeeSnapshot(ctx, &deltaneutral.PlanFeeSnapshot{
		PlanID:         plan.ID,
		TradingFeeUSDT: tradingFee,
		FundingFeeUSDT: fundingFee,
		PeriodStart:    plan.OpenedAt,
		PeriodEnd:      &until,
		FetchedAt:      now,
		CreatedAt:      now,
	}); err != nil {
		logger.WarnCF("dn-fees", "save fee snapshot failed", map[string]any{"plan_id": plan.ID, "error": err.Error()})
	}
}
