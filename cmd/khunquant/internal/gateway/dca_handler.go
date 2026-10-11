package gateway

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/cron"
	"github.com/cryptoquantumwave/khunquant/pkg/dca"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

// handleDCAAutoJob is the cron dispatcher for DCA plans (job name prefix "dca:").
//
// It runs enabled/expiry/period-cap and indicator-trigger checks in pure Go before
// delegating to the LLM-driven cronTool.ExecuteJob. The LLM is only woken when all
// conditions pass, avoiding token waste on the common "RSI not oversold yet" case.
//
// The in-tool checks inside execute_dca_order are kept as defense-in-depth.
func handleDCAAutoJob(
	ctx context.Context,
	job *cron.CronJob,
	cfg *config.Config,
	dcaStore *dca.Store,
	cronTool *tools.CronTool,
) (string, error) {
	// Parse plan ID from "dca:<id>:<name>".
	parts := strings.SplitN(job.Name, ":", 3)
	if len(parts) < 2 {
		return "", fmt.Errorf("dca-gate: malformed job name %q", job.Name)
	}
	planID, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return "", fmt.Errorf("dca-gate: parse plan_id from %q: %w", job.Name, err)
	}

	plan, err := dcaStore.GetPlan(ctx, planID)
	if err != nil {
		logger.DebugCF("dca-gate", "Plan not found, skipping", map[string]any{
			"plan_id": planID, "job": job.Name, "error": err.Error(),
		})
		return "plan not found", nil
	}

	// The job's name says which plan it is for, and the turn gets that plan's
	// authority. Only the job the plan itself created may carry it: a job that
	// merely has a "dca:<id>:" name (a hand-made one, or a stale one) is not run.
	if plan.CronJobID == "" || plan.CronJobID != job.ID {
		logger.WarnCF("dca-gate", "Job is not the plan's own cron job, skipping", map[string]any{
			"plan_id": planID, "job": job.Name, "job_id": job.ID, "plan_job_id": plan.CronJobID,
		})
		return "job is not this plan's own cron job; not executed", nil
	}

	if !plan.Enabled {
		logger.DebugCF("dca-gate", "Plan disabled, skipping", map[string]any{"plan_id": planID})
		return "plan disabled", nil
	}

	if plan.EndDate != nil && time.Now().After(*plan.EndDate) {
		logger.DebugCF("dca-gate", "Plan expired, skipping", map[string]any{
			"plan_id": planID, "end_date": plan.EndDate.Format("2006-01-02"),
		})
		return "plan expired", nil
	}

	// Period-cap guardrail — mirrors dca_execute_order.go:74-85.
	if plan.ExecPeriod != "" && plan.MaxExecPerPeriod > 0 {
		count, err := dcaStore.CountExecutionsInPeriod(ctx, planID, plan.ExecPeriod)
		if err != nil {
			logger.WarnCF("dca-gate", "Period-cap check failed", map[string]any{
				"plan_id": planID, "error": err.Error(),
			})
			// Proceed to LLM on check failure; the tool will enforce the cap.
		} else if count >= plan.MaxExecPerPeriod {
			logger.DebugCF("dca-gate", "Period cap reached, skipping", map[string]any{
				"plan_id": planID, "count": count, "max": plan.MaxExecPerPeriod, "period": plan.ExecPeriod,
			})
			return "period cap reached", nil
		}
	}

	// Indicator trigger gate — mirrors dca_execute_order.go:98-109.
	if plan.Trigger != nil {
		p, err := broker.CreateProviderForAccount(plan.Provider, plan.Account, cfg)
		if err != nil {
			return "", fmt.Errorf("dca-gate: create provider %q: %w", plan.Provider, err)
		}
		md, ok := p.(broker.MarketDataProvider)
		if !ok {
			return "", fmt.Errorf("dca-gate: provider %q does not support market data", plan.Provider)
		}

		met, snap, err := dca.EvaluateTrigger(ctx, plan.Trigger, md, plan.Symbol)
		if err != nil {
			return "", fmt.Errorf("dca-gate: trigger evaluation for plan %d: %w", planID, err)
		}
		if !met {
			logger.DebugCF("dca-gate", "Trigger condition not met, skipping LLM", map[string]any{
				"plan_id":    planID,
				"expression": plan.Trigger.Expression,
				"snapshot":   snap.FormatSnapshot(),
			})
			return "condition not met", nil
		}
		logger.DebugCF("dca-gate", "Trigger condition met, delegating to agent", map[string]any{
			"plan_id":    planID,
			"expression": plan.Trigger.Expression,
		})
	}

	if cronTool == nil {
		return "", fmt.Errorf("dca-gate: no cronTool configured for plan %d", planID)
	}
	// The turn may execute this plan and no other: execute_dca_order checks the
	// plan_id it is given against this scope.
	ctx = tools.WithAutomationScope(ctx, tools.AutomationScope{JobName: job.Name, DCAPlanID: planID})
	return cronTool.ExecuteJob(ctx, job)
}
