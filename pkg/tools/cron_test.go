package tools

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/cron"
)

type stubJobExecutor struct {
	response        string
	err             error
	alreadySent     bool // simulate message tool having already sent in this round
	lastPrompt      string
	lastKey         string
	lastChan        string
	lastChatID      string
	publishedResp   string
	publishedChan   string
	publishedChatID string
	publishedKey    string
}

func (s *stubJobExecutor) ProcessDirectWithChannel(
	_ context.Context,
	content, sessionKey, channel, chatID string,
	_ bool,
) (string, error) {
	s.lastPrompt = content
	s.lastKey = sessionKey
	s.lastChan = channel
	s.lastChatID = chatID
	return s.response, s.err
}

func (s *stubJobExecutor) PublishResponseIfNeeded(
	_ context.Context,
	channel, chatID, sessionKey, response string,
) {
	if s.alreadySent {
		return
	}
	s.publishedResp = response
	s.publishedChan = channel
	s.publishedChatID = chatID
	s.publishedKey = sessionKey
}

func newTestCronToolWithExecutorAndConfig(t *testing.T, executor JobExecutor, cfg *config.Config) *CronTool {
	t.Helper()
	storePath := filepath.Join(t.TempDir(), "cron.json")
	cronService := cron.NewCronService(storePath, nil)
	msgBus := bus.NewMessageBus()
	tool, err := NewCronTool(cronService, executor, msgBus, t.TempDir(), true, 0, cfg)
	if err != nil {
		t.Fatalf("NewCronTool() error: %v", err)
	}
	return tool
}

func newTestCronToolWithConfig(t *testing.T, cfg *config.Config) *CronTool {
	t.Helper()
	return newTestCronToolWithExecutorAndConfig(t, nil, cfg)
}

func newTestCronTool(t *testing.T) *CronTool {
	t.Helper()
	return newTestCronToolWithConfig(t, config.DefaultConfig())
}

// TestCronTool_CommandBlockedFromRemoteChannel verifies command scheduling is restricted to internal channels
func TestCronTool_CommandBlockedFromRemoteChannel(t *testing.T) {
	tool := newTestCronTool(t)
	ctx := WithToolContext(context.Background(), "telegram", "chat-1")
	result := tool.Execute(ctx, map[string]any{
		"action":          "add",
		"message":         "check disk",
		"command":         "df -h",
		"command_confirm": true,
		"at_seconds":      float64(60),
	})

	if !result.IsError {
		t.Fatal("expected command scheduling to be blocked from remote channel")
	}
	if !strings.Contains(result.ForLLM, "restricted to internal channels") {
		t.Errorf("expected 'restricted to internal channels', got: %s", result.ForLLM)
	}
}

// TestCronTool_CommandRequiresConfirm verifies command_confirm=true is required
func TestCronTool_CommandRequiresConfirm(t *testing.T) {
	tool := newTestCronTool(t)
	ctx := WithToolContext(context.Background(), "cli", "direct")
	result := tool.Execute(ctx, map[string]any{
		"action":     "add",
		"message":    "check disk",
		"command":    "df -h",
		"at_seconds": float64(60),
	})

	if !result.IsError {
		t.Fatal("expected error when command_confirm is missing")
	}
	if !strings.Contains(result.ForLLM, "command_confirm=true") {
		t.Errorf("expected 'command_confirm=true' message, got: %s", result.ForLLM)
	}
}

// TestCronTool_CommandAllowedFromInternalChannel verifies command scheduling works from internal channels
func TestCronTool_CommandAllowedFromInternalChannel(t *testing.T) {
	tool := newTestCronTool(t)
	ctx := WithToolContext(context.Background(), "cli", "direct")
	result := tool.Execute(ctx, map[string]any{
		"action":          "add",
		"message":         "check disk",
		"command":         "df -h",
		"command_confirm": true,
		"at_seconds":      float64(60),
	})

	if result.IsError {
		t.Fatalf("expected command scheduling to succeed from internal channel, got: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, "Cron job added") {
		t.Errorf("expected 'Cron job added', got: %s", result.ForLLM)
	}
}

// TestCronTool_AddJobRequiresSessionContext verifies fail-closed when channel/chatID missing
func TestCronTool_AddJobRequiresSessionContext(t *testing.T) {
	tool := newTestCronTool(t)
	result := tool.Execute(context.Background(), map[string]any{
		"action":     "add",
		"message":    "reminder",
		"at_seconds": float64(60),
	})

	if !result.IsError {
		t.Fatal("expected error when session context is missing")
	}
	if !strings.Contains(result.ForLLM, "no session context") {
		t.Errorf("expected 'no session context' message, got: %s", result.ForLLM)
	}
}

// TestCronTool_NonCommandJobAllowedFromRemoteChannel verifies regular reminders work from any channel
func TestCronTool_NonCommandJobAllowedFromRemoteChannel(t *testing.T) {
	tool := newTestCronTool(t)
	ctx := WithToolContext(context.Background(), "telegram", "chat-1")
	result := tool.Execute(ctx, map[string]any{
		"action":     "add",
		"message":    "time to stretch",
		"at_seconds": float64(600),
	})

	if result.IsError {
		t.Fatalf("expected non-command reminder to succeed from remote channel, got: %s", result.ForLLM)
	}
}

func TestCronTool_NonCommandJobDefaultsDeliverToFalse(t *testing.T) {
	tool := newTestCronTool(t)
	ctx := WithToolContext(context.Background(), "telegram", "chat-1")
	result := tool.Execute(ctx, map[string]any{
		"action":     "add",
		"message":    "send me a poem",
		"at_seconds": float64(600),
	})

	if result.IsError {
		t.Fatalf("expected non-command reminder to succeed, got: %s", result.ForLLM)
	}

	jobs := tool.cronService.ListJobs(false)
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs))
	}
	if jobs[0].Payload.Deliver {
		t.Fatal("expected deliver=false by default for non-command jobs")
	}
}

func TestCronTool_ExecuteJobPublishesErrorWhenExecDisabled(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Tools.Exec.Enabled = false

	tool := newTestCronToolWithConfig(t, cfg)
	job := &cron.CronJob{}
	job.Payload.Channel = "cli"
	job.Payload.To = "direct"
	job.Payload.Command = "df -h"

	if got := tool.ExecuteJob(context.Background(), job); got != "ok" {
		t.Fatalf("ExecuteJob() = %q, want ok", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	var msg bus.OutboundMessage
	select {
	case msg = <-tool.msgBus.OutboundChan():
		// got message
	case <-ctx.Done():
		t.Fatal("timeout waiting for outbound message")
	}
	if !strings.Contains(msg.Content, "command execution is disabled") {
		t.Fatalf("expected exec disabled message, got: %s", msg.Content)
	}
}

func TestCronTool_ExecuteJobPublishesAgentResponse(t *testing.T) {
	executor := &stubJobExecutor{response: "generated reply"}
	tool := newTestCronToolWithExecutorAndConfig(t, executor, config.DefaultConfig())

	job := &cron.CronJob{ID: "job-1"}
	job.Payload.Channel = "telegram"
	job.Payload.To = "chat-1"
	job.Payload.Message = "send me a poem"

	if got := tool.ExecuteJob(context.Background(), job); got != "ok" {
		t.Fatalf("ExecuteJob() = %q, want ok", got)
	}

	// Each execution uses a unique session key "cron-{jobID}-{timestamp}" so
	// conversation history does not accumulate across runs (upstream 36b9693d3).
	if !strings.HasPrefix(executor.lastKey, "cron-job-1-") {
		t.Fatalf("sessionKey = %q, want prefix cron-job-1-", executor.lastKey)
	}
	if executor.lastChan != "telegram" || executor.lastChatID != "chat-1" {
		t.Fatalf("executor target = %s/%s, want telegram/chat-1", executor.lastChan, executor.lastChatID)
	}
	if executor.lastPrompt != "send me a poem" {
		t.Fatalf("prompt = %q, want original message", executor.lastPrompt)
	}
	if executor.publishedResp != "generated reply" {
		t.Fatalf("published response = %q, want generated reply", executor.publishedResp)
	}
	if executor.publishedKey != executor.lastKey {
		t.Fatalf("published sessionKey = %q, want %q", executor.publishedKey, executor.lastKey)
	}
	if executor.publishedChan != "telegram" || executor.publishedChatID != "chat-1" {
		t.Fatalf("published target = %s/%s, want telegram/chat-1", executor.publishedChan, executor.publishedChatID)
	}
}

func TestCronTool_ExecuteJobSkipsEmptyAgentResponse(t *testing.T) {
	executor := &stubJobExecutor{}
	tool := newTestCronToolWithExecutorAndConfig(t, executor, config.DefaultConfig())

	job := &cron.CronJob{ID: "job-empty"}
	job.Payload.Channel = "telegram"
	job.Payload.To = "chat-1"
	job.Payload.Message = "say nothing"

	if got := tool.ExecuteJob(context.Background(), job); got != "ok" {
		t.Fatalf("ExecuteJob() = %q, want ok", got)
	}

	if executor.publishedResp != "" {
		t.Fatalf("unexpected published response: %q", executor.publishedResp)
	}
}

func TestCronTool_ExecuteJobSkipsWhenMessageToolAlreadySent(t *testing.T) {
	executor := &stubJobExecutor{response: "Sent.", alreadySent: true}
	tool := newTestCronToolWithExecutorAndConfig(t, executor, config.DefaultConfig())

	job := &cron.CronJob{ID: "job-msg-sent"}
	job.Payload.Channel = "telegram"
	job.Payload.To = "chat-1"
	job.Payload.Message = "send weather"

	if got := tool.ExecuteJob(context.Background(), job); got != "ok" {
		t.Fatalf("ExecuteJob() = %q, want ok", got)
	}

	if executor.publishedResp != "" {
		t.Fatalf("expected no published response when message tool already sent, got: %q", executor.publishedResp)
	}
}

func TestCronTool_ExecuteJobDirectiveAddsPromptPrefix(t *testing.T) {
	executor := &stubJobExecutor{response: "directive result"}
	tool := newTestCronToolWithExecutorAndConfig(t, executor, config.DefaultConfig())

	originalMsg := "check the weather and summarize"
	job := &cron.CronJob{ID: "job-dir-1"}
	job.Payload.Channel = "telegram"
	job.Payload.To = "chat-1"
	job.Payload.Message = originalMsg
	job.Payload.Type = "directive"

	if got := tool.ExecuteJob(context.Background(), job); got != "ok" {
		t.Fatalf("ExecuteJob() = %q, want ok", got)
	}

	wantPrompt := "Please execute the following directive and provide the result:\n\n" + originalMsg
	if executor.lastPrompt != wantPrompt {
		t.Fatalf("prompt = %q, want exact %q", executor.lastPrompt, wantPrompt)
	}
	if executor.publishedResp != "directive result" {
		t.Fatalf("published response = %q, want %q", executor.publishedResp, "directive result")
	}
}

func TestCronTool_ExecuteJobDirectiveWithDeliverRoutesToAgent(t *testing.T) {
	executor := &stubJobExecutor{response: "agent processed"}
	tool := newTestCronToolWithExecutorAndConfig(t, executor, config.DefaultConfig())

	job := &cron.CronJob{ID: "job-dir-deliver"}
	job.Payload.Channel = "telegram"
	job.Payload.To = "chat-1"
	job.Payload.Message = "generate daily report"
	job.Payload.Type = "directive"
	job.Payload.Deliver = true

	if got := tool.ExecuteJob(context.Background(), job); got != "ok" {
		t.Fatalf("ExecuteJob() = %q, want ok", got)
	}

	if executor.lastPrompt == "" {
		t.Fatal("expected agent to be called for directive+deliver, but ProcessDirectWithChannel was not invoked")
	}
	if executor.publishedResp != "agent processed" {
		t.Fatalf("published response = %q, want %q", executor.publishedResp, "agent processed")
	}

	// Verify no direct publish happened on the bus (agent path, not direct path)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	select {
	case msg := <-tool.msgBus.OutboundChan():
		t.Fatalf("unexpected direct bus message: %+v", msg)
	case <-ctx.Done():
		// expected: no direct bus message
	}
}

func TestCronTool_ExecuteJobDeliverMessageDirectlyToBus(t *testing.T) {
	executor := &stubJobExecutor{response: "should not be called"}
	tool := newTestCronToolWithExecutorAndConfig(t, executor, config.DefaultConfig())

	job := &cron.CronJob{ID: "job-deliver"}
	job.Payload.Channel = "telegram"
	job.Payload.To = "chat-1"
	job.Payload.Message = "hello world"
	job.Payload.Deliver = true

	if got := tool.ExecuteJob(context.Background(), job); got != "ok" {
		t.Fatalf("ExecuteJob() = %q, want ok", got)
	}

	if executor.lastPrompt != "" {
		t.Fatal("expected agent NOT to be invoked for deliver=true message type")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	select {
	case msg := <-tool.msgBus.OutboundChan():
		if msg.Content != "hello world" {
			t.Fatalf("bus content = %q, want %q", msg.Content, "hello world")
		}
	case <-ctx.Done():
		t.Fatal("timeout waiting for direct bus message")
	}
}

func TestCronTool_ExecuteJobReturnsErrorWithoutPublish(t *testing.T) {
	executor := &stubJobExecutor{
		response: "this response must not be published",
		err:      fmt.Errorf("agent failure"),
	}
	tool := newTestCronToolWithExecutorAndConfig(t, executor, config.DefaultConfig())

	job := &cron.CronJob{ID: "job-err"}
	job.Payload.Channel = "telegram"
	job.Payload.To = "chat-1"
	job.Payload.Message = "do something"

	got := tool.ExecuteJob(context.Background(), job)
	if !strings.Contains(got, "agent failure") {
		t.Fatalf("ExecuteJob() = %q, want error message", got)
	}

	if executor.publishedResp != "" {
		t.Fatalf("unexpected publish on error path: %q", executor.publishedResp)
	}
}

func TestCronTool_AddJobRejectsInvalidType(t *testing.T) {
	tool := newTestCronTool(t)
	ctx := WithToolContext(context.Background(), "cli", "direct")
	result := tool.Execute(ctx, map[string]any{
		"action":     "add",
		"message":    "test",
		"at_seconds": float64(60),
		"type":       "invalid_type",
	})

	if !result.IsError {
		t.Fatal("expected error for invalid type parameter")
	}
	if !strings.Contains(result.ForLLM, "invalid type") {
		t.Errorf("expected 'invalid type' error, got: %s", result.ForLLM)
	}
}

func TestCronTool_AddJobAcceptsValidTypes(t *testing.T) {
	for _, msgType := range []string{"", "message", "directive"} {
		t.Run("type="+msgType, func(t *testing.T) {
			tool := newTestCronTool(t)
			ctx := WithToolContext(context.Background(), "cli", "direct")
			args := map[string]any{
				"action":     "add",
				"message":    "test",
				"at_seconds": float64(60),
			}
			if msgType != "" {
				args["type"] = msgType
			}

			result := tool.Execute(ctx, args)
			if result.IsError {
				t.Fatalf("expected valid type %q to succeed, got: %s", msgType, result.ForLLM)
			}
		})
	}
}

func TestCronTool_Name(t *testing.T) {
	tool := newTestCronTool(t)
	if tool.Name() != NameCron {
		t.Errorf("Name() = %q, want %q", tool.Name(), NameCron)
	}
}

func TestCronTool_Description(t *testing.T) {
	tool := newTestCronTool(t)
	desc := tool.Description()
	if desc == "" {
		t.Fatal("Description() should not be empty")
	}
	if !strings.Contains(desc, "Schedule") {
		t.Errorf("Description should mention scheduling, got: %s", desc)
	}
}

func TestCronTool_Parameters(t *testing.T) {
	tool := newTestCronTool(t)
	params := tool.Parameters()

	if params == nil {
		t.Fatal("Parameters() should not return nil")
	}
	if params["type"] != "object" {
		t.Errorf("type should be 'object', got %q", params["type"])
	}

	props, ok := params["properties"].(map[string]any)
	if !ok {
		t.Fatal("expected properties to be a map")
	}

	expectedProps := []string{"action", "message", "command", "at_seconds", "every_seconds", "cron_expr", "job_id"}
	for _, prop := range expectedProps {
		if _, ok := props[prop]; !ok {
			t.Errorf("expected property %q in Parameters", prop)
		}
	}

	required, ok := params["required"].([]string)
	if !ok {
		t.Fatal("required should be a slice")
	}
	if len(required) == 0 || required[0] != "action" {
		t.Errorf("action should be required, got %v", required)
	}
}

func TestCronTool_ListJobs_Empty(t *testing.T) {
	tool := newTestCronTool(t)
	result := tool.Execute(context.Background(), map[string]any{
		"action": "list",
	})

	if result.IsError {
		t.Fatalf("unexpected error: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, "No scheduled jobs") {
		t.Errorf("expected 'No scheduled jobs', got: %s", result.ForLLM)
	}
}

func TestCronTool_ListJobs_WithJobs(t *testing.T) {
	tool := newTestCronTool(t)
	ctx := WithToolContext(context.Background(), "cli", "direct")

	// Add a job
	addResult := tool.Execute(ctx, map[string]any{
		"action":     "add",
		"message":    "test reminder",
		"at_seconds": float64(60),
	})
	if addResult.IsError {
		t.Fatalf("failed to add job: %s", addResult.ForLLM)
	}

	// List jobs
	listResult := tool.Execute(context.Background(), map[string]any{
		"action": "list",
	})

	if listResult.IsError {
		t.Fatalf("unexpected error: %s", listResult.ForLLM)
	}
	if !strings.Contains(listResult.ForLLM, "Scheduled jobs") {
		t.Errorf("expected 'Scheduled jobs' in list, got: %s", listResult.ForLLM)
	}
	if !strings.Contains(listResult.ForLLM, "test reminder") {
		t.Errorf("expected job name in list, got: %s", listResult.ForLLM)
	}
}

func TestCronTool_ListJobs_ShowsScheduleInfo(t *testing.T) {
	tool := newTestCronTool(t)
	ctx := WithToolContext(context.Background(), "cli", "direct")

	// Add a recurring job
	addResult := tool.Execute(ctx, map[string]any{
		"action":       "add",
		"message":      "recurring task",
		"every_seconds": float64(3600),
	})
	if addResult.IsError {
		t.Fatalf("failed to add job: %s", addResult.ForLLM)
	}

	// List jobs
	listResult := tool.Execute(context.Background(), map[string]any{
		"action": "list",
	})

	if listResult.IsError {
		t.Fatalf("unexpected error: %s", listResult.ForLLM)
	}
	// Should show schedule info like "every 3600s" or "every 1h"
	if !strings.Contains(listResult.ForLLM, "recurring task") {
		t.Errorf("expected job name in output, got: %s", listResult.ForLLM)
	}
}

func TestCronTool_RemoveJob_Missing(t *testing.T) {
	tool := newTestCronTool(t)
	result := tool.Execute(context.Background(), map[string]any{
		"action": "remove",
	})

	if !result.IsError {
		t.Fatal("expected error when job_id is missing")
	}
	if !strings.Contains(result.ForLLM, "job_id is required") {
		t.Errorf("expected 'job_id is required', got: %s", result.ForLLM)
	}
}

func TestCronTool_RemoveJob_Empty(t *testing.T) {
	tool := newTestCronTool(t)
	result := tool.Execute(context.Background(), map[string]any{
		"action": "remove",
		"job_id": "",
	})

	if !result.IsError {
		t.Fatal("expected error when job_id is empty")
	}
}

func TestCronTool_RemoveJob_NotFound(t *testing.T) {
	tool := newTestCronTool(t)
	result := tool.Execute(context.Background(), map[string]any{
		"action": "remove",
		"job_id": "nonexistent-job-id",
	})

	if !result.IsError {
		t.Fatal("expected error for non-existent job")
	}
	if !strings.Contains(result.ForLLM, "not found") {
		t.Errorf("expected 'not found' message, got: %s", result.ForLLM)
	}
}

func TestCronTool_RemoveJob_Success(t *testing.T) {
	tool := newTestCronTool(t)
	ctx := WithToolContext(context.Background(), "cli", "direct")

	// Add a job
	addResult := tool.Execute(ctx, map[string]any{
		"action":     "add",
		"message":    "job to remove",
		"at_seconds": float64(60),
	})
	if addResult.IsError {
		t.Fatalf("failed to add job: %s", addResult.ForLLM)
	}

	// Extract job ID from result
	jobs := tool.cronService.ListJobs(false)
	if len(jobs) == 0 {
		t.Fatal("expected job to be created")
	}
	jobID := jobs[0].ID

	// Remove the job
	removeResult := tool.Execute(context.Background(), map[string]any{
		"action": "remove",
		"job_id": jobID,
	})

	if removeResult.IsError {
		t.Fatalf("unexpected error: %s", removeResult.ForLLM)
	}
	if !strings.Contains(removeResult.ForLLM, "removed") {
		t.Errorf("expected 'removed' message, got: %s", removeResult.ForLLM)
	}

	// Verify job is gone
	jobs = tool.cronService.ListJobs(false)
	for _, j := range jobs {
		if j.ID == jobID {
			t.Fatal("job was not removed")
		}
	}
}

func TestCronTool_EnableJob_Missing(t *testing.T) {
	tool := newTestCronTool(t)
	result := tool.Execute(context.Background(), map[string]any{
		"action": "enable",
	})

	if !result.IsError {
		t.Fatal("expected error when job_id is missing")
	}
	if !strings.Contains(result.ForLLM, "job_id is required") {
		t.Errorf("expected 'job_id is required', got: %s", result.ForLLM)
	}
}

func TestCronTool_EnableJob_NotFound(t *testing.T) {
	tool := newTestCronTool(t)
	result := tool.Execute(context.Background(), map[string]any{
		"action": "enable",
		"job_id": "nonexistent-job-id",
	})

	if !result.IsError {
		t.Fatal("expected error for non-existent job")
	}
	if !strings.Contains(result.ForLLM, "not found") {
		t.Errorf("expected 'not found' message, got: %s", result.ForLLM)
	}
}

func TestCronTool_EnableJob_Success(t *testing.T) {
	tool := newTestCronTool(t)
	ctx := WithToolContext(context.Background(), "cli", "direct")

	// Add a job
	addResult := tool.Execute(ctx, map[string]any{
		"action":     "add",
		"message":    "job to enable",
		"at_seconds": float64(60),
	})
	if addResult.IsError {
		t.Fatalf("failed to add job: %s", addResult.ForLLM)
	}

	// Get the job ID
	jobs := tool.cronService.ListJobs(false)
	if len(jobs) == 0 {
		t.Fatal("expected job to be created")
	}
	jobID := jobs[0].ID

	// Enable the job
	enableResult := tool.Execute(context.Background(), map[string]any{
		"action": "enable",
		"job_id": jobID,
	})

	if enableResult.IsError {
		t.Fatalf("unexpected error: %s", enableResult.ForLLM)
	}
	if !strings.Contains(enableResult.ForLLM, "enabled") {
		t.Errorf("expected 'enabled' message, got: %s", enableResult.ForLLM)
	}
}

func TestCronTool_DisableJob_Success(t *testing.T) {
	tool := newTestCronTool(t)
	ctx := WithToolContext(context.Background(), "cli", "direct")

	// Add a job
	addResult := tool.Execute(ctx, map[string]any{
		"action":     "add",
		"message":    "job to disable",
		"at_seconds": float64(60),
	})
	if addResult.IsError {
		t.Fatalf("failed to add job: %s", addResult.ForLLM)
	}

	// Get the job ID
	jobs := tool.cronService.ListJobs(false)
	if len(jobs) == 0 {
		t.Fatal("expected job to be created")
	}
	jobID := jobs[0].ID

	// Disable the job
	disableResult := tool.Execute(context.Background(), map[string]any{
		"action": "disable",
		"job_id": jobID,
	})

	if disableResult.IsError {
		t.Fatalf("unexpected error: %s", disableResult.ForLLM)
	}
	if !strings.Contains(disableResult.ForLLM, "disabled") {
		t.Errorf("expected 'disabled' message, got: %s", disableResult.ForLLM)
	}
}
