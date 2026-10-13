// KhunQuant - Ultra-lightweight personal AI agent
// Inspired by and based on nanobot: https://github.com/HKUDS/nanobot
// License: MIT
//
// Copyright (c) 2026 KhunQuant contributors

package heartbeat

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/constants"
	"github.com/cryptoquantumwave/khunquant/pkg/fileutil"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/state"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

const (
	minIntervalMinutes     = 30
	defaultIntervalMinutes = 120
	userTasksMarker        = "Add your heartbeat tasks below this line:"

	// minFirstDelay is the shortest wait before the first run after a start or
	// a reload: long enough for the rest of the gateway to come up.
	minFirstDelay = time.Second

	// stopWait bounds how long Stop waits for a governed run in flight to
	// notice its cancelled context. A legacy run cannot be cancelled and is not
	// waited for.
	stopWait = 5 * time.Second
)

// HeartbeatHandler is the function type for handling heartbeat.
// It returns a ToolResult that can indicate async operations.
// channel and chatID are derived from the last active user channel.
type HeartbeatHandler func(prompt, channel, chatID string) *tools.ToolResult

// ContextHandler is a HeartbeatHandler that is given the service's context. The
// context ends when the service is stopped, so a handler that can be cancelled
// (a governed turn) stops with it. Under ContractGoverned channel and chatID are
// always empty: a governed heartbeat is not addressed to the last active chat.
type ContextHandler func(ctx context.Context, prompt, channel, chatID string) *tools.ToolResult

// clock is the service's view of time, so that scheduling can be tested without
// real waiting.
type clock interface {
	Now() time.Time
	NewTimer(d time.Duration) timer
}

// timer is the part of *time.Timer the schedule uses.
type timer interface {
	C() <-chan time.Time
	Stop() bool
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) NewTimer(d time.Duration) timer { return realTimer{time.NewTimer(d)} }

type realTimer struct{ t *time.Timer }

func (r realTimer) C() <-chan time.Time { return r.t.C }

func (r realTimer) Stop() bool { return r.t.Stop() }

// HeartbeatService manages periodic heartbeat checks
type HeartbeatService struct {
	workspace string
	bus       *bus.MessageBus
	state     *state.Manager
	handler   ContextHandler
	interval  time.Duration
	enabled   bool
	contract  Contract
	clock     clock
	mu        sync.RWMutex
	stopChan  chan struct{}

	// Set by Start, ended by Stop.
	done   chan struct{}      // closed when the schedule loop has returned
	wake   chan struct{}      // nudges the loop after SetInterval
	ctx    context.Context    // ends on Stop; what a ContextHandler is given
	cancel context.CancelFunc // ends ctx

	running atomic.Bool // a run is in flight

	// runWithoutTasks: a run happens even when HEARTBEAT.md lists no task of the
	// owner's (guarded by mu). A sweep heartbeat has work that does not depend on
	// the file: it reconciles the sources and looks at what changed, and the file
	// only tells the model what the owner cares about.
	runWithoutTasks bool
}

// normalizeInterval applies the minimum and the default to a configured
// interval in minutes.
func normalizeInterval(intervalMinutes int) time.Duration {
	if intervalMinutes < minIntervalMinutes && intervalMinutes != 0 {
		intervalMinutes = minIntervalMinutes
	}
	if intervalMinutes == 0 {
		intervalMinutes = defaultIntervalMinutes
	}
	return time.Duration(intervalMinutes) * time.Minute
}

// NewHeartbeatService creates a new heartbeat service. stateMgr is the shared
// state manager (normally the agent loop's), so the service sees the last
// channel as it is now rather than as it was when the service was built, and
// keeps the time of its last run where a restart or a reload can find it. A nil
// manager is allowed: it reads as "no last channel" and "never ran", and the
// time of a run is not kept.
func NewHeartbeatService(
	workspace string,
	intervalMinutes int,
	enabled bool,
	stateMgr *state.Manager,
) *HeartbeatService {
	return &HeartbeatService{
		workspace: workspace,
		interval:  normalizeInterval(intervalMinutes),
		enabled:   enabled,
		state:     stateMgr,
		clock:     realClock{},
	}
}

// lastChannel returns the last active channel from the shared state manager,
// or "" when the service was built without one.
func (hs *HeartbeatService) lastChannel() string {
	if hs.state == nil {
		return ""
	}
	return hs.state.GetLastChannel()
}

// LastDestination is the channel and chat a heartbeat would be addressed to
// right now, resolved from the shared state manager on every call. Both are
// empty when nothing usable has been recorded.
func (hs *HeartbeatService) LastDestination() (channel, chatID string) {
	return hs.parseLastChannel(hs.lastChannel())
}

// SetBus sets the message bus for delivering heartbeat results.
func (hs *HeartbeatService) SetBus(msgBus *bus.MessageBus) {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	hs.bus = msgBus
}

// SetHandler sets the heartbeat handler.
func (hs *HeartbeatService) SetHandler(handler HeartbeatHandler) {
	if handler == nil {
		hs.SetContextHandler(nil)
		return
	}
	hs.SetContextHandler(func(_ context.Context, prompt, channel, chatID string) *tools.ToolResult {
		return handler(prompt, channel, chatID)
	})
}

// SetContextHandler sets a handler that is given the service's context.
func (hs *HeartbeatService) SetContextHandler(handler ContextHandler) {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	hs.handler = handler
}

// SetContract sets how the heartbeat reports. Set it before Start; it decides
// which default HEARTBEAT.md is written, which prompt the file is wrapped in, and
// whether the last active chat is consulted (governed: never).
func (hs *HeartbeatService) SetContract(c Contract) {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	hs.contract = c
}

// SetRunWithoutTasks makes the service run the handler on every tick, also when
// HEARTBEAT.md is missing or holds no task of the owner's (the prompt is then the
// file as it is, or the default for the contract). Set it before Start. It is for a
// sweep heartbeat, whose reconciliation must not depend on the owner having written
// a task.
func (hs *HeartbeatService) SetRunWithoutTasks(v bool) {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	hs.runWithoutTasks = v
}

// Interval is the current interval, after the minimum and the default.
func (hs *HeartbeatService) Interval() time.Duration { return hs.currentInterval() }

// Contract reports how the heartbeat reports.
func (hs *HeartbeatService) Contract() Contract {
	hs.mu.RLock()
	defer hs.mu.RUnlock()
	return hs.contract
}

// SetInterval changes the interval, in minutes, without firing a heartbeat.
// The same minimum and default apply as at construction. On a running service
// that has run before, the next run moves to the last run's start plus the new
// interval; if that is already past it is one full new interval from now. A
// change never causes a run by itself. A service that has not run yet keeps its
// pending first run.
func (hs *HeartbeatService) SetInterval(intervalMinutes int) {
	d := normalizeInterval(intervalMinutes)
	hs.mu.Lock()
	defer hs.mu.Unlock()
	hs.interval = d
	if hs.wake != nil {
		select {
		case hs.wake <- struct{}{}:
		default: // a nudge is already pending; the loop re-reads the interval
		}
	}
}

func (hs *HeartbeatService) currentInterval() time.Duration {
	hs.mu.RLock()
	defer hs.mu.RUnlock()
	return hs.interval
}

// Start begins the heartbeat service
func (hs *HeartbeatService) Start() error {
	hs.mu.Lock()
	defer hs.mu.Unlock()

	if hs.stopChan != nil {
		logger.InfoC("heartbeat", "Heartbeat service already running")
		return nil
	}

	if !hs.enabled {
		logger.InfoC("heartbeat", "Heartbeat service disabled")
		return nil
	}

	hs.warnTemplateMismatch()

	hs.stopChan = make(chan struct{})
	hs.done = make(chan struct{})
	hs.wake = make(chan struct{}, 1)
	hs.ctx, hs.cancel = context.WithCancel(context.Background())
	go hs.runLoop(hs.stopChan, hs.wake, hs.done)

	logger.InfoCF("heartbeat", "Heartbeat service started", map[string]any{
		"interval_minutes": hs.interval.Minutes(),
		"contract":         hs.contract.String(),
	})

	return nil
}

// warnTemplateMismatch logs the advisory for a HEARTBEAT.md that does not match
// the contract, once per Start (the gateway builds a new service for every start
// and reload). The caller holds hs.mu.
func (hs *HeartbeatService) warnTemplateMismatch() {
	data, err := os.ReadFile(filepath.Join(hs.workspace, "HEARTBEAT.md"))
	if err != nil {
		return
	}
	if msg := TemplateAdvisory(string(data), hs.contract); msg != "" {
		warnLog(msg, map[string]any{"contract": hs.contract.String()})
	}
}

// Stop gracefully stops the heartbeat service. It cancels the context a
// ContextHandler was given, stops the timer, and lets the schedule goroutine
// return. For a governed run in flight it waits, up to stopWait, for the run to
// end, so that whatever it decided has been handed over before the caller stops
// what that is handed to; a legacy run cannot be cancelled and is not waited for.
func (hs *HeartbeatService) Stop() {
	hs.mu.Lock()
	if hs.stopChan == nil {
		hs.mu.Unlock()
		return
	}

	logger.InfoC("heartbeat", "Stopping heartbeat service")
	close(hs.stopChan)
	hs.stopChan = nil
	if hs.cancel != nil {
		hs.cancel()
	}
	done, wait := hs.done, hs.contract == ContractGoverned
	hs.mu.Unlock()

	if done == nil || !wait {
		return
	}
	select {
	case <-done:
	case <-time.After(stopWait):
		logger.WarnC("heartbeat", "A heartbeat run did not stop in time; its decisions will be dropped")
	}
}

// IsRunning returns whether the service is running
func (hs *HeartbeatService) IsRunning() bool {
	hs.mu.RLock()
	defer hs.mu.RUnlock()
	return hs.stopChan != nil
}

// firstRunDelay is how long a service that has just started waits for its first
// run, given when the last run started.
//
//   - Never ran (zero): minFirstDelay, so a first-ever start gets its first look
//     almost at once.
//   - Ran at some time in the past: what is left of the interval, but not less
//     than minFirstDelay. A run that is already overdue also waits minFirstDelay.
//   - Ran "in the future" (the clock went backwards, or the file came from a
//     machine with a later clock): the elapsed time cannot be known, so wait one
//     full interval. That is the most a heartbeat can be late; the first run
//     overwrites the bad timestamp.
func firstRunDelay(last, now time.Time, interval time.Duration) time.Duration {
	switch {
	case last.IsZero():
		return minFirstDelay
	case last.After(now):
		return interval
	}
	return max(interval-now.Sub(last), minFirstDelay)
}

// nextRun is when the run after the one that started at fired is due. Ticks that
// came due while a run was still in flight are skipped, not queued: the next one
// is the first multiple of interval after fired that is still ahead of now.
func nextRun(fired, now time.Time, interval time.Duration) (next time.Time, skipped int) {
	next = fired.Add(interval)
	for !next.After(now) {
		next = next.Add(interval)
		skipped++
	}
	return next, skipped
}

// runLoop is the schedule: a first run after firstRunDelay, then one every
// interval from that run's start. It is a single goroutine and runs the
// heartbeat inline, so two runs never overlap. It returns when stop is closed.
func (hs *HeartbeatService) runLoop(stop <-chan struct{}, wake <-chan struct{}, done chan<- struct{}) {
	defer close(done)

	now := hs.clock.Now()
	last := hs.lastRun()
	interval := hs.currentInterval()
	delay := firstRunDelay(last, now, interval)
	// anchor is the start of the last run, the base an interval change counts
	// from. A missing or future timestamp is no base.
	anchor := last
	if last.After(now) {
		anchor = time.Time{}
	}

	t := hs.clock.NewTimer(delay)
	defer func() { t.Stop() }()
	rearm := func(d time.Duration) {
		t.Stop()
		t = hs.clock.NewTimer(d)
	}

	for {
		select {
		case <-stop:
			return
		case <-wake:
			interval = hs.currentInterval()
			if anchor.IsZero() {
				continue // nothing has run yet; the pending first run stands
			}
			now = hs.clock.Now()
			next := anchor.Add(interval)
			if !next.After(now) {
				next = now.Add(interval)
			}
			rearm(next.Sub(now))
		case <-t.C():
			fired := hs.clock.Now()
			hs.executeHeartbeat()
			anchor = fired
			interval = hs.currentInterval()
			now = hs.clock.Now()
			next, skipped := nextRun(fired, now, interval)
			if skipped > 0 {
				hs.logInfof("Skipped %d heartbeat tick(s) that came due while a heartbeat was still running", skipped)
			}
			rearm(next.Sub(now))
		}
	}
}

// lastRun is when the last heartbeat run started, zero when unknown.
func (hs *HeartbeatService) lastRun() time.Time {
	if hs.state == nil {
		return time.Time{}
	}
	return hs.state.GetLastHeartbeatAt()
}

// recordRunStart persists that a run is starting. It is done before the handler
// is called, so a crash in the middle of a turn does not make the next start run
// again at once.
func (hs *HeartbeatService) recordRunStart() {
	if hs.state == nil {
		return
	}
	if err := hs.state.SetLastHeartbeatAt(hs.clock.Now()); err != nil {
		hs.logErrorf("Failed to record the heartbeat start time: %v", err)
	}
}

// runContext is the context for a handler: it ends on Stop. A service whose
// loop was not started (a test calling executeHeartbeat directly) gets a
// background context.
func (hs *HeartbeatService) runContext() context.Context {
	hs.mu.RLock()
	defer hs.mu.RUnlock()
	if hs.ctx != nil {
		return hs.ctx
	}
	return context.Background()
}

// executeHeartbeat performs a single heartbeat check
func (hs *HeartbeatService) executeHeartbeat() {
	hs.mu.RLock()
	enabled := hs.enabled
	handler := hs.handler
	contract := hs.contract
	if !hs.enabled || hs.stopChan == nil {
		hs.mu.RUnlock()
		return
	}
	hs.mu.RUnlock()

	if !enabled {
		return
	}

	// One run at a time: a run still in flight when the next is due makes that
	// one skip rather than pile up behind it.
	if !hs.running.CompareAndSwap(false, true) {
		hs.logInfof("Heartbeat skipped: the previous run is still in flight")
		return
	}
	defer hs.running.Store(false)

	logger.DebugC("heartbeat", "Executing heartbeat")

	prompt := hs.buildPrompt()
	if prompt == "" {
		logger.InfoC("heartbeat", "No heartbeat prompt (HEARTBEAT.md empty or missing)")
		return
	}

	if handler == nil {
		hs.logErrorf("Heartbeat handler not configured")
		return
	}

	// A governed heartbeat is not addressed to anyone: its decisions go to the
	// owner through attention, so the last active chat is not even looked up.
	var channel, chatID string
	if contract != ContractGoverned {
		// Get last channel info for context
		lastChannel := hs.lastChannel()
		channel, chatID = hs.parseLastChannel(lastChannel)

		// Debug log for channel resolution
		hs.logInfof("Resolved channel: %s, chatID: %s (from lastChannel: %s)", channel, chatID, lastChannel)
	}

	hs.recordRunStart()
	result := handler(hs.runContext(), prompt, channel, chatID)

	if result == nil {
		hs.logInfof("Heartbeat handler returned nil result")
		return
	}

	// Handle different result types
	if result.IsError {
		hs.logErrorf("Heartbeat error: %s", result.ForLLM)
		return
	}

	if result.Async {
		hs.logInfof("Async task started: %s", result.ForLLM)
		logger.InfoCF("heartbeat", "Async heartbeat task started",
			map[string]any{
				"message": result.ForLLM,
			})
		return
	}

	// Check if silent
	if result.Silent {
		hs.logInfof("Heartbeat OK - silent")
		return
	}

	// A governed heartbeat delivers through attention and nothing else: whatever
	// the handler returned, it is not sent to a chat from here.
	if contract == ContractGoverned {
		hs.logInfof("Heartbeat completed (governed, result not sent): %s", result.ForLLM)
		return
	}

	// Send result to user
	if result.ForUser != "" {
		hs.sendResponse(result.ForUser)
	} else if result.ForLLM != "" {
		hs.sendResponse(result.ForLLM)
	}

	hs.logInfof("Heartbeat completed: %s", result.ForLLM)
}

// GovernedPrompt is the prompt a governed heartbeat gives its proactive turn:
// the time and HEARTBEAT.md's content, with no request for HEARTBEAT_OK or for
// actions, because the proactive turn puts its own contract (respond_proactive,
// silence by default) ahead of it. now is formatted by the caller. It is
// exported so that the size of a governed turn can be measured on the real text.
func GovernedPrompt(now, content string) string {
	return fmt.Sprintf(`# Heartbeat Check

Current time: %s

This is a scheduled heartbeat check. Review the following tasks, look at the data
with your read tools, and report through respond_proactive.

%s
`, now, content)
}

// buildPrompt builds the heartbeat prompt from HEARTBEAT.md
func (hs *HeartbeatService) buildPrompt() string {
	heartbeatPath := filepath.Join(hs.workspace, "HEARTBEAT.md")

	hs.mu.RLock()
	contract := hs.contract
	always := hs.runWithoutTasks
	hs.mu.RUnlock()

	var content string
	data, err := os.ReadFile(heartbeatPath)
	switch {
	case err == nil:
		content = string(data)
	case os.IsNotExist(err):
		hs.createDefaultHeartbeatTemplate()
		if !always {
			return ""
		}
		content = defaultTemplate(contract)
	default:
		hs.logErrorf("Error reading HEARTBEAT.md: %v", err)
		return ""
	}

	if !always && !heartbeatHasUserTasks(content) {
		return ""
	}

	now := hs.clock.Now().Format("2006-01-02 15:04:05")
	if contract == ContractGoverned {
		// The proactive turn puts its own contract (respond_proactive, silence
		// by default) ahead of this text, so it must not ask for HEARTBEAT_OK or
		// for actions.
		return GovernedPrompt(now, content)
	}
	return fmt.Sprintf(`# Heartbeat Check

Current time: %s

You are a proactive AI assistant. This is a scheduled heartbeat check.
Review the following tasks and execute any necessary actions using available skills.
If there is nothing that requires attention, respond ONLY with: HEARTBEAT_OK

%s
`, now, content)
}

// createDefaultHeartbeatTemplate creates the default HEARTBEAT.md file: the one
// that matches the service's contract.
func (hs *HeartbeatService) createDefaultHeartbeatTemplate() {
	heartbeatPath := filepath.Join(hs.workspace, "HEARTBEAT.md")

	hs.mu.RLock()
	contract := hs.contract
	hs.mu.RUnlock()

	if err := fileutil.WriteFileAtomic(heartbeatPath, []byte(defaultTemplate(contract)), 0o644); err != nil {
		hs.logErrorf("Failed to create default HEARTBEAT.md: %v", err)
	} else {
		hs.logInfof("Created default HEARTBEAT.md template")
	}
}

func heartbeatHasUserTasks(content string) bool {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return false
	}

	markerIdx := strings.Index(content, userTasksMarker)
	if markerIdx < 0 {
		return true
	}

	tasksSection := content[markerIdx+len(userTasksMarker):]
	for _, line := range strings.Split(tasksSection, "\n") {
		trimmedLine := strings.TrimSpace(line)
		if trimmedLine == "" {
			continue
		}
		if strings.HasPrefix(trimmedLine, "#") {
			continue
		}
		return true
	}

	return false
}

// sendResponse sends the heartbeat response to the last channel
func (hs *HeartbeatService) sendResponse(response string) {
	hs.mu.RLock()
	msgBus := hs.bus
	hs.mu.RUnlock()

	if msgBus == nil {
		hs.logInfof("No message bus configured, heartbeat result not sent")
		return
	}

	// Get last channel from state
	lastChannel := hs.lastChannel()
	if lastChannel == "" {
		hs.logInfof("No last channel recorded, heartbeat result not sent")
		return
	}

	platform, userID := hs.parseLastChannel(lastChannel)

	// Skip internal channels that can't receive messages
	if platform == "" || userID == "" {
		return
	}

	pubCtx, pubCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer pubCancel()
	msgBus.PublishOutbound(pubCtx, bus.OutboundMessage{
		Channel: platform,
		ChatID:  userID,
		Content: response,
	})

	hs.logInfof("Heartbeat result sent to %s", platform)
}

// parseLastChannel parses the last channel string into platform and userID.
// Returns empty strings for invalid or internal channels.
func (hs *HeartbeatService) parseLastChannel(lastChannel string) (platform, userID string) {
	if lastChannel == "" {
		return "", ""
	}

	// Parse channel format: "platform:user_id" (e.g., "telegram:123456")
	parts := strings.SplitN(lastChannel, ":", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		hs.logErrorf("Invalid last channel format: %s", lastChannel)
		return "", ""
	}

	platform, userID = parts[0], parts[1]

	// Skip internal channels
	if constants.IsInternalChannel(platform) {
		hs.logInfof("Skipping internal channel: %s", platform)
		return "", ""
	}

	return platform, userID
}

// logInfof logs an informational message to the heartbeat log
func (hs *HeartbeatService) logInfof(format string, args ...any) {
	hs.logf("INFO", format, args...)
}

// logErrorf logs an error message to the heartbeat log
func (hs *HeartbeatService) logErrorf(format string, args ...any) {
	hs.logf("ERROR", format, args...)
}

// logf writes a message to the heartbeat log file
func (hs *HeartbeatService) logf(level, format string, args ...any) {
	logFile := filepath.Join(hs.workspace, "heartbeat.log")
	f, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()

	timestamp := time.Now().Format("2006-01-02 15:04:05")
	fmt.Fprintf(f, "[%s] [%s] %s\n", timestamp, level, fmt.Sprintf(format, args...))
}
