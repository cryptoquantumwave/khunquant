package gateway

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/cron"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

// ceilingNoticePublishTimeout bounds the publish of each notice.
const ceilingNoticePublishTimeout = 5 * time.Second

// ceilingNoticePublisher is the part of the message bus the notice needs.
type ceilingNoticePublisher interface {
	PublishOutbound(ctx context.Context, msg bus.OutboundMessage) error
}

// ceilingNoticeStore is the part of the cron service the notice needs.
type ceilingNoticeStore interface {
	ListJobs(includeDisabled bool) []cron.CronJob
	MarkCeilingNotified(jobID string) error
}

// ceilingNoticeMessage is the one templated message a generic scheduled agent
// job gets when the autonomous tool ceiling takes effect.
func ceilingNoticeMessage(jobName string) string {
	return fmt.Sprintf(
		"Scheduled task '%s' now runs with read-and-notify tools only: it can no longer trade, "+
			"change plans or schedules, run commands or use MCP tools. "+
			"For recurring buys, create a DCA plan (ask me \"create a DCA plan\").",
		jobName)
}

// isGenericAgentJob reports whether a job is an ordinary scheduled agent turn:
// one that runs through the model (the same test CronTool.ExecuteJob applies:
// no command, and either not deliver-only or a directive) and that no code-driven
// handler owns. Alerts, DCA and delta-neutral jobs are dispatched by name and
// are not affected by what the ceiling takes away.
func isGenericAgentJob(job cron.CronJob) bool {
	for _, prefix := range tools.ReservedJobNamePrefixes {
		if strings.HasPrefix(job.Name, prefix) {
			return false
		}
	}
	if job.Payload.Command != "" {
		return false
	}
	return !job.Payload.Deliver || job.Payload.Type == "directive"
}

// sendCeilingNotices tells the owner of each enabled generic scheduled agent job,
// once, that the job now runs under the autonomous tool ceiling. The send is
// recorded in the job's persisted state, so it is never repeated; a failed
// publish (or a failed save of the record) leaves the job unmarked and the
// notice is retried on the next start. Jobs with no delivery target cannot be
// told anything and are left for a later start.
func sendCeilingNotices(pub ceilingNoticePublisher, store ceilingNoticeStore) {
	if pub == nil || store == nil {
		return
	}
	for _, job := range store.ListJobs(false) {
		if job.State.NotifiedCeilingV1 || !isGenericAgentJob(job) {
			continue
		}
		if job.Payload.Channel == "" || job.Payload.To == "" {
			continue
		}

		ctx, cancel := context.WithTimeout(context.Background(), ceilingNoticePublishTimeout)
		err := pub.PublishOutbound(ctx, bus.OutboundMessage{
			Channel: job.Payload.Channel,
			ChatID:  job.Payload.To,
			Content: ceilingNoticeMessage(job.Name),
		})
		cancel()
		if err != nil {
			logger.WarnCF("cron", "failed to publish tool-ceiling notice; will retry on next start", map[string]any{
				"job_id": job.ID, "error": err.Error(),
			})
			continue
		}
		if err := store.MarkCeilingNotified(job.ID); err != nil {
			logger.WarnCF("cron", "failed to record tool-ceiling notice", map[string]any{
				"job_id": job.ID, "error": err.Error(),
			})
		}
	}
}
