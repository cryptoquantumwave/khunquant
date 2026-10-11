package gateway

import (
	"context"
	"fmt"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/cron"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
)

// missedJobPublishTimeout bounds the one notification sent for a missed job.
const missedJobPublishTimeout = 5 * time.Second

// cronOptionsFromConfig maps tools.cron.* onto the scheduler options.
func cronOptionsFromConfig(cfg *config.Config) cron.Options {
	c := cfg.Tools.Cron
	opts := cron.DefaultOptions()
	if c.Workers > 0 {
		opts.Workers = c.Workers
	}
	opts.MisfireGrace = time.Duration(max(c.MisfireGraceMinutes, 0)) * time.Minute
	opts.JobTimeout = time.Duration(max(c.JobTimeoutMinutes, 0)) * time.Minute
	return opts
}

// missedJobNotifier returns the cron.SetOnMissed callback: one templated message
// to the job's own channel and chat. Jobs without a delivery target are only logged.
func missedJobNotifier(msgBus *bus.MessageBus) func(job cron.CronJob) {
	return func(job cron.CronJob) {
		logger.WarnCF("cron", "scheduled task missed while offline", map[string]any{
			"job_id": job.ID, "name": job.Name,
		})
		if msgBus == nil || job.Payload.Channel == "" || job.Payload.To == "" {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), missedJobPublishTimeout)
		defer cancel()
		if err := msgBus.PublishOutbound(ctx, bus.OutboundMessage{
			Channel: job.Payload.Channel,
			ChatID:  job.Payload.To,
			Content: missedJobMessage(job),
		}); err != nil {
			logger.WarnCF("cron", "failed to publish missed-task notice", map[string]any{
				"job_id": job.ID, "error": err.Error(),
			})
		}
	}
}

// missedJobMessage renders the notice for a one-shot job that was due while the
// assistant was offline and is now disabled.
func missedJobMessage(job cron.CronJob) string {
	due := "unknown time"
	if job.Schedule.AtMS != nil {
		loc := time.Local
		if job.Schedule.TZ != "" {
			if l, err := time.LoadLocation(job.Schedule.TZ); err == nil {
				loc = l
			}
		}
		due = time.UnixMilli(*job.Schedule.AtMS).In(loc).Format("2006-01-02 15:04 MST")
	}
	return fmt.Sprintf(
		"Scheduled task '%s' was missed while the assistant was offline (due %s) and has been disabled.",
		job.Name, due,
	)
}
