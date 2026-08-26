package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/constants"
	"github.com/cryptoquantumwave/khunquant/pkg/cron"
	"github.com/cryptoquantumwave/khunquant/pkg/providers"
	"github.com/cryptoquantumwave/khunquant/pkg/utils"
)

// JobExecutor is the interface for executing cron jobs through the agent
type JobExecutor interface {
	ProcessDirectWithChannel(ctx context.Context, content, sessionKey, channel, chatID string, noHistory bool) (string, error)
	// PublishResponseIfNeeded sends response to the outbound bus only when the
	// agent did not already deliver content through the message tool in this round.
	PublishResponseIfNeeded(ctx context.Context, channel, chatID, sessionKey, response string)
}

// CronTool provides scheduling capabilities for the agent
type CronTool struct {
	cronService *cron.CronService
	executor    JobExecutor
	msgBus      *bus.MessageBus
	execTool    *ExecTool
	cfg         *config.Config
}

// NewCronTool creates a new CronTool
// execTimeout: 0 means no timeout, >0 sets the timeout duration
func NewCronTool(
	cronService *cron.CronService, executor JobExecutor, msgBus *bus.MessageBus, workspace string, restrict bool,
	execTimeout time.Duration, cfg *config.Config,
) (*CronTool, error) {
	execTool, err := NewExecToolWithConfig(workspace, restrict, cfg)
	if err != nil {
		return nil, fmt.Errorf("unable to configure exec tool: %w", err)
	}

	execTool.SetTimeout(execTimeout)
	return &CronTool{
		cronService: cronService,
		executor:    executor,
		msgBus:      msgBus,
		execTool:    execTool,
		cfg:         cfg,
	}, nil
}

// Name returns the tool name
func (t *CronTool) Name() string {
	return NameCron
}

// Description returns the tool description
func (t *CronTool) Description() string {
	return "Schedule reminders, tasks, or system commands. IMPORTANT: When user asks to be reminded or scheduled, you MUST call this tool. Use 'at_seconds' for one-time reminders (e.g., 'remind me in 10 minutes' → at_seconds=600). Use 'every_seconds' ONLY for recurring tasks (e.g., 'every 2 hours' → every_seconds=7200). Use 'cron_expr' for complex recurring schedules. Use 'command' to execute shell commands directly. For **dynamic reports** (live prices, portfolio values, weather, etc.): set deliver=false and write message as a re-fetch instruction (e.g. \"Fetch and report current portfolio values from all exchanges\"). The agent will re-run tools on each trigger. For **static reminders**: set deliver=true and write the literal reminder text."
}

// Parameters returns the tool parameters schema
func (t *CronTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type":        "string",
				"enum":        []string{"add", "list", "remove", "enable", "disable"},
				"description": "Action to perform. Use 'add' when user wants to schedule a reminder or task.",
			},
			"message": map[string]any{
				"type":        "string",
				"description": "**Static reminder**: the literal text to show when triggered (use with deliver=true). **Dynamic task**: an agent instruction to execute on each trigger, e.g. \"Fetch and report current portfolio value from Binance and OKX\" (use with deliver=false). Do NOT embed current values — write the instruction so fresh data is fetched each time. If 'command' is used, this describes what the command does.",
			},
			"command": map[string]any{
				"type":        "string",
				"description": "Optional: Shell command to execute directly (e.g., 'df -h'). If set, the agent will run this command and report output instead of just showing the message. 'deliver' will be forced to false for commands.",
			},
			"command_confirm": map[string]any{
				"type":        "boolean",
				"description": "Required when using command=true. Must be true to explicitly confirm scheduling a shell command.",
			},
			"at_seconds": map[string]any{
				"type":        "integer",
				"description": "One-time reminder: seconds from now when to trigger (e.g., 600 for 10 minutes later). Use this for one-time reminders like 'remind me in 10 minutes'.",
			},
			"every_seconds": map[string]any{
				"type":        "integer",
				"description": "Recurring interval in seconds (e.g., 3600 for every hour). Use this ONLY for recurring tasks like 'every 2 hours' or 'daily reminder'.",
			},
			"cron_expr": map[string]any{
				"type":        "string",
				"description": "Cron expression for complex recurring schedules (e.g., '0 9 * * *' for daily at 9am). Use this for complex recurring schedules.",
			},
			"job_id": map[string]any{
				"type":        "string",
				"description": "Job ID (for remove/enable/disable)",
			},
			"type": map[string]any{
				"type":        "string",
				"enum":        []string{"message", "directive"},
				"description": "Message generation strategy. 'message' (default): content is sent directly as-is. 'directive': content is treated as instructions for an AI agent to execute before delivery.",
			},
			"deliver": map[string]any{
				"type":        "boolean",
				"description": "If true, send message directly to channel. If false, let agent process message (for complex tasks). Default: false",
			},
			"no_history": map[string]any{
				"type":        "boolean",
				"description": "If true, each run is stateless — no session history is loaded or accumulated between executions. Default: false.",
			},
		},
		"required": []string{"action"},
	}
}

// Execute runs the tool with the given arguments
func (t *CronTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	action, ok := args["action"].(string)
	if !ok {
		return ErrorResult("action is required")
	}

	switch action {
	case "add":
		return t.addJob(ctx, args)
	case "list":
		return t.listJobs(ctx)
	case "get":
		return t.getJob(ctx, args)
	case "update":
		return t.updateJob(ctx, args)
	case "remove":
		return t.removeJob(ctx, args)
	case "enable":
		return t.enableJob(ctx, args, true)
	case "disable":
		return t.enableJob(ctx, args, false)
	default:
		return ErrorResult(fmt.Sprintf("unknown action: %s", action))
	}
}

func (t *CronTool) addJob(ctx context.Context, args map[string]any) *ToolResult {
	channel := ToolChannel(ctx)
	chatID := ToolChatID(ctx)

	if channel == "" || chatID == "" {
		return ErrorResult("no session context (channel/chat_id not set). Use this tool in an active conversation.")
	}

	message, ok := args["message"].(string)
	if !ok || message == "" {
		return ErrorResult("message is required for add")
	}

	var schedule cron.CronSchedule

	// Check for at_seconds (one-time), every_seconds (recurring), or cron_expr
	atSeconds, hasAt := args["at_seconds"].(float64)
	everySeconds, hasEvery := args["every_seconds"].(float64)
	cronExpr, hasCron := args["cron_expr"].(string)

	// Fix: type assertions return true for zero values, need additional validity checks
	// This prevents LLMs that fill unused optional parameters with defaults (0) from triggering wrong type
	hasAt = hasAt && atSeconds > 0
	hasEvery = hasEvery && everySeconds > 0
	hasCron = hasCron && cronExpr != ""

	// Priority: at_seconds > every_seconds > cron_expr
	if hasAt {
		atMS := time.Now().UnixMilli() + int64(atSeconds)*1000
		schedule = cron.CronSchedule{
			Kind: "at",
			AtMS: &atMS,
		}
	} else if hasEvery {
		everyMS := int64(everySeconds) * 1000
		schedule = cron.CronSchedule{
			Kind:    "every",
			EveryMS: &everyMS,
		}
	} else if hasCron {
		schedule = cron.CronSchedule{
			Kind: "cron",
			Expr: cronExpr,
		}
	} else {
		return ErrorResult("one of at_seconds, every_seconds, or cron_expr is required")
	}

	// Read deliver parameter, default to false so scheduled tasks execute through the agent
	deliver := false
	if d, ok := args["deliver"].(bool); ok {
		deliver = d
	}

	// Validate type parameter (server-side whitelist, not just LLM schema hint)
	msgType, _ := args["type"].(string)
	if msgType != "" && msgType != "message" && msgType != "directive" {
		return ErrorResult(fmt.Sprintf("invalid type %q, must be 'message' or 'directive'", msgType))
	}

	// GHSA-pv8c-p6jf-3fpp: command scheduling requires internal channel. When
	// allow_command is disabled, explicit confirmation is required as an override.
	// Non-command reminders remain open to all channels.
	command, _ := args["command"].(string)
	commandConfirm, _ := args["command_confirm"].(bool)
	if command != "" {
		if !constants.IsInternalChannel(channel) {
			return ErrorResult("scheduling command execution is restricted to internal channels")
		}
		if !commandConfirm {
			return ErrorResult("command_confirm=true is required to schedule command execution")
		}
		deliver = false
	}

	// Truncate message for job name (max 30 chars)
	messagePreview := utils.Truncate(message, 30)

	job, err := t.cronService.AddJob(
		messagePreview,
		schedule,
		message,
		deliver,
		channel,
		chatID,
	)
	if err != nil {
		return ErrorResult(fmt.Sprintf("Error adding job: %v", err))
	}

	// Apply optional payload fields and persist in a single UpdateJob call
	needsUpdate := false
	if command != "" {
		job.Payload.Command = command
		needsUpdate = true
	}
	if msgType != "" {
		job.Payload.Type = msgType
		needsUpdate = true
	}
	if noHistory, ok := args["no_history"].(bool); ok && noHistory {
		job.Payload.NoHistory = true
		needsUpdate = true
	}
	if needsUpdate {
		t.cronService.UpdateJob(job)
	}

	return SilentResult(fmt.Sprintf("Cron job added: %s (id: %s)", job.Name, job.ID))
}

func (t *CronTool) listJobs(ctx context.Context) *ToolResult {
	jobs := t.cronService.ListJobs(false)

	// Filter jobs by access control
	var accessible []*cron.CronJob
	for i := range jobs {
		if t.canAccessJob(ctx, &jobs[i]) {
			accessible = append(accessible, &jobs[i])
		}
	}

	if len(accessible) == 0 {
		return SilentResult("No scheduled jobs")
	}

	var result strings.Builder
	result.WriteString("Scheduled jobs:\n")
	for _, j := range accessible {
		var scheduleInfo string
		if j.Schedule.Kind == "every" && j.Schedule.EveryMS != nil {
			scheduleInfo = fmt.Sprintf("every %ds", *j.Schedule.EveryMS/1000)
		} else if j.Schedule.Kind == "cron" {
			scheduleInfo = j.Schedule.Expr
		} else if j.Schedule.Kind == "at" {
			scheduleInfo = "one-time"
		} else {
			scheduleInfo = "unknown"
		}
		result.WriteString(fmt.Sprintf("- %s (id: %s, %s)\n", j.Name, j.ID, scheduleInfo))
	}

	return SilentResult(result.String())
}

func (t *CronTool) removeJob(ctx context.Context, args map[string]any) *ToolResult {
	jobID, ok := args["job_id"].(string)
	if !ok || jobID == "" {
		return ErrorResult("job_id is required for remove")
	}

	job := t.cronService.GetJob(jobID)
	if job == nil {
		return ErrorResult(fmt.Sprintf("Job %s not found", jobID))
	}
	if !t.canAccessJob(ctx, job) {
		return ErrorResult(fmt.Sprintf("Job %s is not accessible from this channel", jobID))
	}

	if t.cronService.RemoveJob(jobID) {
		return SilentResult(fmt.Sprintf("Cron job removed: %s", jobID))
	}
	return ErrorResult(fmt.Sprintf("Job %s not found", jobID))
}

func (t *CronTool) enableJob(ctx context.Context, args map[string]any, enable bool) *ToolResult {
	jobID, ok := args["job_id"].(string)
	if !ok || jobID == "" {
		return ErrorResult("job_id is required for enable/disable")
	}

	job := t.cronService.GetJob(jobID)
	if job == nil {
		return ErrorResult(fmt.Sprintf("Job %s not found", jobID))
	}
	if !t.canAccessJob(ctx, job) {
		return ErrorResult(fmt.Sprintf("Job %s is not accessible from this channel", jobID))
	}

	updatedJob := t.cronService.EnableJob(jobID, enable)
	if updatedJob == nil {
		return ErrorResult(fmt.Sprintf("Job %s not found", jobID))
	}

	status := "enabled"
	if !enable {
		status = "disabled"
	}
	return SilentResult(fmt.Sprintf("Cron job '%s' %s", updatedJob.Name, status))
}

// ExecuteJob executes a cron job through the agent
func (t *CronTool) ExecuteJob(ctx context.Context, job *cron.CronJob) string {
	// Get channel/chatID from job payload
	channel := job.Payload.Channel
	chatID := job.Payload.To

	// Default values if not set
	if channel == "" {
		channel = "cli"
	}
	if chatID == "" {
		chatID = "direct"
	}

	// Execute command if present
	if job.Payload.Command != "" {
		if t.cfg != nil && !t.cfg.Tools.Exec.Enabled {
			pubCtx, pubCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer pubCancel()
			_ = t.msgBus.PublishOutbound(pubCtx, bus.OutboundMessage{
				Channel: channel,
				ChatID:  chatID,
				Content: fmt.Sprintf("Scheduled command '%s' skipped: command execution is disabled", job.Payload.Command),
			})
			return "ok"
		}

		args := map[string]any{
			"action":    "run",
			"command":   job.Payload.Command,
			"__channel": channel,
			"__chat_id": chatID,
		}

		result := t.execTool.Execute(ctx, args)
		var output string
		if result.IsError {
			output = fmt.Sprintf("Error executing scheduled command: %s", result.ForLLM)
		} else {
			output = fmt.Sprintf("Scheduled command '%s' executed:\n%s", job.Payload.Command, result.ForLLM)
		}

		pubCtx, pubCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer pubCancel()
		if err := t.msgBus.PublishOutbound(pubCtx, bus.OutboundMessage{
			Channel: channel,
			ChatID:  chatID,
			Content: output,
		}); err != nil {
			return err.Error()
		}
		return "ok"
	}

	// Determine message generation strategy
	// Type="directive": treat message as instructions for AI agent to execute
	// Type="" or "message" (default): static message content
	isDirective := job.Payload.Type == "directive"

	// If deliver=true and not directive, send message directly without agent processing
	if job.Payload.Deliver && !isDirective {
		pubCtx, pubCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer pubCancel()
		if err := t.msgBus.PublishOutbound(pubCtx, bus.OutboundMessage{
			Channel: channel,
			ChatID:  chatID,
			Content: job.Payload.Message,
		}); err != nil {
			return err.Error()
		}
		return "ok"
	}

	// For deliver=false OR directive mode, process through agent.
	// Use a unique key per execution ("cron-{jobID}-{timestamp}") so conversation
	// history does not accumulate across runs of the same job (upstream 36b9693d3).
	sessionKey := fmt.Sprintf("cron-%s-%d", job.ID, time.Now().UnixMilli())

	// Prepare the prompt based on type
	prompt := job.Payload.Message
	if isDirective {
		// For directive type, prefix to clarify this is an instruction
		prompt = fmt.Sprintf(
			"Please execute the following directive and provide the result:\n\n%s",
			job.Payload.Message,
		)
	}

	// Call agent with the prepared prompt
	response, err := t.executor.ProcessDirectWithChannel(
		ctx,
		prompt,
		sessionKey,
		channel,
		chatID,
		job.Payload.NoHistory,
	)
	if err != nil {
		// Return a friendly, classified message as the job result instead of a
		// raw error dump. We deliberately do NOT publish to the channel here —
		// scheduled-job failures are surfaced via the job result, not pushed to
		// the user's chat (see TestCronTool_ExecuteJobReturnsErrorWithoutPublish).
		return providers.FriendlyError(err)
	}

	if response != "" {
		t.executor.PublishResponseIfNeeded(ctx, channel, chatID, sessionKey, response)
	}
	return "ok"
}

func (t *CronTool) getJob(ctx context.Context, args map[string]any) *ToolResult {
	jobID, errResult := requiredCronJobID(args, "get")
	if errResult != nil {
		return errResult
	}

	job := t.cronService.GetJob(jobID)
	if job == nil {
		return ErrorResult(fmt.Sprintf("Job %s not found", jobID))
	}

	if !t.canAccessJob(ctx, job) {
		return ErrorResult(fmt.Sprintf("Job %s is not accessible from this channel", jobID))
	}

	return SilentResult(formatCronJobJSON(job))
}

func (t *CronTool) updateJob(ctx context.Context, args map[string]any) *ToolResult {
	jobID, errResult := requiredCronJobID(args, "update")
	if errResult != nil {
		return errResult
	}

	job := t.cronService.GetJob(jobID)
	if job == nil {
		return ErrorResult(fmt.Sprintf("Job %s not found", jobID))
	}

	if !t.canAccessJob(ctx, job) {
		return ErrorResult(fmt.Sprintf("Job %s is not accessible from this channel", jobID))
	}

	patches := 0

	if name, present, errResult := optionalNonEmptyString(args, "name"); errResult != nil {
		return errResult
	} else if present {
		job.Name = name
		patches++
	}

	if message, present, errResult := optionalNonEmptyString(args, "message"); errResult != nil {
		return errResult
	} else if present {
		job.Payload.Message = message
		patches++
	}

	if schedule, present, errResult := schedulePatch(args); errResult != nil {
		return errResult
	} else if present {
		job.Schedule = schedule
		patches++
	}

	if patches == 0 {
		return ErrorResult("at least one of name, message, or schedule parameters is required")
	}

	if err := t.cronService.UpdateJob(job); err != nil {
		return ErrorResult(fmt.Sprintf("update failed: %v", err))
	}

	return SilentResult(fmt.Sprintf("Cron job '%s' updated", job.Name))
}

func (t *CronTool) canAccessJob(ctx context.Context, job *cron.CronJob) bool {
	channel := ToolChannel(ctx)
	chatID := ToolChatID(ctx)

	// If no channel/chatID context, allow access (internal call)
	if channel == "" && chatID == "" {
		return true
	}

	// If channel/chatID are set, apply strict access control
	if channel == "" || chatID == "" {
		return false
	}
	if job.Payload.Channel != channel || job.Payload.To != chatID {
		return false
	}
	if job.Payload.Command != "" {
		return isCommandAllowedRemote(channel, chatID, t.cfg.Tools.Cron.CommandAllowedRemotes)
	}
	return true
}

// Helper functions for cron tool

func requiredCronJobID(args map[string]any, action string) (string, *ToolResult) {
	jobID, ok := args["job_id"].(string)
	if !ok || jobID == "" {
		return "", ErrorResult(fmt.Sprintf("job_id is required for %s", action))
	}
	return jobID, nil
}

func optionalString(args map[string]any, key string) (string, bool, *ToolResult) {
	value, present := args[key]
	if !present {
		return "", false, nil
	}
	text, ok := value.(string)
	if !ok {
		return "", false, ErrorResult(fmt.Sprintf("%s must be a string", key))
	}
	return text, true, nil
}

func optionalNonEmptyString(args map[string]any, key string) (string, bool, *ToolResult) {
	_, present := args[key]
	if !present {
		return "", false, nil
	}
	text, _, errResult := optionalString(args, key)
	if errResult != nil {
		return "", false, errResult
	}
	if strings.TrimSpace(text) == "" {
		return "", false, ErrorResult(fmt.Sprintf("%s cannot be empty", key))
	}
	return text, true, nil
}

func schedulePatch(args map[string]any) (cron.CronSchedule, bool, *ToolResult) {
	var schedule cron.CronSchedule
	patches := 0

	if atSeconds, present, errResult := optionalUint64(args, "at_seconds"); errResult != nil {
		return schedule, false, errResult
	} else if present {
		atMS := int64(atSeconds * 1000)
		schedule = cron.CronSchedule{Kind: "at", AtMS: &atMS}
		patches++
	}

	if everySeconds, present, errResult := optionalUint64(args, "every_seconds"); errResult != nil {
		return schedule, false, errResult
	} else if present {
		everyMS := int64(everySeconds * 1000)
		schedule = cron.CronSchedule{Kind: "every", EveryMS: &everyMS}
		patches++
	}

	if cronExpr, present, errResult := optionalNonEmptyString(args, "cron_expr"); errResult != nil {
		return schedule, false, errResult
	} else if present {
		schedule = cron.CronSchedule{Kind: "cron", Expr: cronExpr}
		patches++
	}

	if patches > 1 {
		return schedule, false, ErrorResult("only one of at_seconds, every_seconds, or cron_expr may be patched")
	}

	return schedule, patches == 1, nil
}

func optionalUint64(args map[string]any, key string) (uint64, bool, *ToolResult) {
	value, present := args[key]
	if !present {
		return 0, false, nil
	}

	switch v := value.(type) {
	case float64:
		if v < 0 {
			return 0, false, ErrorResult(fmt.Sprintf("%s must be non-negative", key))
		}
		return uint64(v), true, nil
	case int:
		if v < 0 {
			return 0, false, ErrorResult(fmt.Sprintf("%s must be non-negative", key))
		}
		return uint64(v), true, nil
	case int64:
		if v < 0 {
			return 0, false, ErrorResult(fmt.Sprintf("%s must be non-negative", key))
		}
		return uint64(v), true, nil
	default:
		return 0, false, ErrorResult(fmt.Sprintf("%s must be a number", key))
	}
}

func formatCronJobJSON(job *cron.CronJob) string {
	data, err := json.Marshal(job)
	if err != nil {
		return fmt.Sprintf("%+v", *job)
	}
	return string(data)
}

func isCommandAllowedRemote(channel, chatID string, allowed []string) bool {
	if channel == "" {
		return false
	}

	target := channel
	if chatID != "" {
		target = channel + ":" + chatID
	}

	for _, entry := range allowed {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if entry == "*" || entry == channel || entry == target {
			return true
		}
	}

	return false
}
