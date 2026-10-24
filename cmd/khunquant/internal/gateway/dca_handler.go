package gateway

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/cron"
	"github.com/cryptoquantumwave/khunquant/pkg/dca"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

// errDCAJobNotPlanOwned is what handleDCAAutoJob returns for a "dca:" job that is
// not the cron job its plan records. It is wrapped by dcaJobNotPlanOwnedError,
// which carries the details; the dispatcher matches on this sentinel to tell the
// user (see notifyDCAJobNotPlanOwned).
var errDCAJobNotPlanOwned = errors.New("dca job is not the plan's own cron job")

type dcaJobNotPlanOwnedError struct {
	PlanID                                  int64
	PlanName, JobName, JobID, RecordedJobID string
}

func (e *dcaJobNotPlanOwnedError) Error() string {
	return fmt.Sprintf(
		"dca-gate: job %q (%s) is not the cron job recorded for DCA plan %d (recorded job id %q); "+
			"nothing was executed. Run update_dca_plan on the plan to re-link its schedule",
		e.JobName, e.JobID, e.PlanID, e.RecordedJobID)
}

func (e *dcaJobNotPlanOwnedError) Unwrap() error { return errDCAJobNotPlanOwned }

// dcaNotPlanOwnedNotified remembers the jobs already reported to their chat. It
// is package-level on purpose: setupCronTool, and so the dispatcher closure, is
// rebuilt on every config reload, and a reload must not repeat the message.
var dcaNotPlanOwnedNotified sync.Map // job ID -> struct{}

// dcaNoticePublishTimeout bounds the publish of the notice.
const dcaNoticePublishTimeout = 5 * time.Second

// notifyDCAJobNotPlanOwned tells the owner of a DCA job, once per process, that
// the plan is not running because its schedule is not linked to it. The log line
// and the recorded job error do not reach the chat, and a recurring buy that has
// stopped must not go unnoticed. A job with no delivery target is skipped, and a
// failed publish is retried on the next tick.
func notifyDCAJobNotPlanOwned(pub ceilingNoticePublisher, job *cron.CronJob, err error) {
	var owned *dcaJobNotPlanOwnedError
	if pub == nil || !errors.As(err, &owned) {
		return
	}
	if job.Payload.Channel == "" || job.Payload.To == "" {
		return
	}
	if _, already := dcaNotPlanOwnedNotified.LoadOrStore(job.ID, struct{}{}); already {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), dcaNoticePublishTimeout)
	defer cancel()
	if perr := pub.PublishOutbound(ctx, bus.OutboundMessage{
		Channel: job.Payload.Channel,
		ChatID:  job.Payload.To,
		Content: fmt.Sprintf(
			"DCA plan %d (%s) is not running: its schedule is not linked to the plan. "+
				"Ask me to update the plan's schedule (the same schedule is fine) to re-link it.",
			owned.PlanID, owned.PlanName),
	}); perr != nil {
		dcaNotPlanOwnedNotified.Delete(job.ID)
		logger.WarnCF("dca-gate", "failed to publish the unlinked-plan notice; will retry", map[string]any{
			"plan_id": owned.PlanID, "job_id": job.ID, "error": perr.Error(),
		})
	}
}

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
		// An error, not a quiet success: the scheduler records it as the job's
		// LastError, so a recurring buy that has stopped is visible rather than
		// looking like a run that went fine.
		return "", &dcaJobNotPlanOwnedError{
			PlanID: planID, PlanName: plan.Name, JobName: job.Name, JobID: job.ID, RecordedJobID: plan.CronJobID,
		}
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
