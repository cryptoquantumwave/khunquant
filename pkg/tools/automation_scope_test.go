package tools

import (
	"context"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/cron"
)

// ExecuteJob records which job the turn runs for, so the class ceiling can tell
// a dca: job from any other; a scope the caller attached first (the DCA handler
// adds the plan ID) is the one that survives.
func TestCronTool_ExecuteJobAttachesAutomationScope(t *testing.T) {
	executor := &stubJobExecutor{response: "ok"}
	tool := newTestCronToolWithExecutorAndConfig(t, executor, config.DefaultConfig())

	job := &cron.CronJob{ID: "job-1", Name: "weekly buy"}
	job.Payload.Channel, job.Payload.To, job.Payload.Message = "telegram", "chat-1", "buy"
	if _, err := tool.ExecuteJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if !executor.lastScopeOK || executor.lastScope != (AutomationScope{JobName: "weekly buy"}) {
		t.Fatalf("scope = %+v (%v), want the job name and no plan", executor.lastScope, executor.lastScopeOK)
	}

	dca := &cron.CronJob{ID: "job-2", Name: "dca:7:weekly"}
	dca.Payload.Channel, dca.Payload.To, dca.Payload.Message = "telegram", "chat-1", "buy"
	ctx := WithAutomationScope(context.Background(), AutomationScope{JobName: dca.Name, DCAPlanID: 7})
	if _, err := tool.ExecuteJob(ctx, dca); err != nil {
		t.Fatal(err)
	}
	if executor.lastScope != (AutomationScope{JobName: "dca:7:weekly", DCAPlanID: 7}) {
		t.Fatalf("scope = %+v, want the handler's plan to survive", executor.lastScope)
	}
}
