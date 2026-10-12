package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/providers"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

// SteeringMode controls how queued steering messages are dequeued.
type SteeringMode string

const (
	// SteeringOneAtATime dequeues only the first queued message per poll.
	SteeringOneAtATime SteeringMode = "one-at-a-time"
	// SteeringAll drains the entire queue in a single poll.
	SteeringAll SteeringMode = "all"
	// MaxQueueSize number of possible messages in the Steering Queue
	MaxQueueSize = 10
)

// parseSteeringMode normalizes a config string into a SteeringMode.
func parseSteeringMode(s string) SteeringMode {
	switch s {
	case "all":
		return SteeringAll
	default:
		return SteeringOneAtATime
	}
}

// steeringQueue is a thread-safe queue of user messages that can be injected
// into a running agent loop to interrupt it between tool calls.
type steeringQueue struct {
	mu    sync.Mutex
	queue []providers.Message
	mode  SteeringMode
}

func newSteeringQueue(mode SteeringMode) *steeringQueue {
	return &steeringQueue{
		mode: mode,
	}
}

// push enqueues a steering message.
func (sq *steeringQueue) push(msg providers.Message) error {
	sq.mu.Lock()
	defer sq.mu.Unlock()
	if len(sq.queue) >= MaxQueueSize {
		return fmt.Errorf("steering queue is full")
	}
	sq.queue = append(sq.queue, msg)
	return nil
}

// dequeue removes and returns pending steering messages according to the
// configured mode. Returns nil when the queue is empty.
func (sq *steeringQueue) dequeue() []providers.Message {
	sq.mu.Lock()
	defer sq.mu.Unlock()

	if len(sq.queue) == 0 {
		return nil
	}

	switch sq.mode {
	case SteeringAll:
		msgs := sq.queue
		sq.queue = nil
		return msgs
	default: // one-at-a-time
		msg := sq.queue[0]
		sq.queue[0] = providers.Message{} // Clear reference for GC
		sq.queue = sq.queue[1:]
		return []providers.Message{msg}
	}
}

// len returns the number of queued messages.
func (sq *steeringQueue) len() int {
	sq.mu.Lock()
	defer sq.mu.Unlock()
	return len(sq.queue)
}

// setMode updates the steering mode.
func (sq *steeringQueue) setMode(mode SteeringMode) {
	sq.mu.Lock()
	defer sq.mu.Unlock()
	sq.mode = mode
}

// getMode returns the current steering mode.
func (sq *steeringQueue) getMode() SteeringMode {
	sq.mu.Lock()
	defer sq.mu.Unlock()
	return sq.mode
}

// --- AgentLoop steering API ---

// Steer enqueues a user message to be injected into the currently running
// agent loop. The message will be picked up after the current tool finishes
// executing, causing any remaining tool calls in the batch to be skipped.
func (al *AgentLoop) Steer(msg providers.Message) error {
	if al.steering == nil {
		return fmt.Errorf("steering queue is not initialized")
	}
	if err := al.steering.push(msg); err != nil {
		logger.WarnCF("agent", "Failed to enqueue steering message", map[string]any{
			"error": err.Error(),
			"role":  msg.Role,
		})
		return err
	}
	logger.DebugCF("agent", "Steering message enqueued", map[string]any{
		"role":        msg.Role,
		"content_len": len(msg.Content),
		"queue_len":   al.steering.len(),
	})

	return nil
}

// SteeringMode returns the current steering mode.
func (al *AgentLoop) SteeringMode() SteeringMode {
	if al.steering == nil {
		return SteeringOneAtATime
	}
	return al.steering.getMode()
}

// SetSteeringMode updates the steering mode.
func (al *AgentLoop) SetSteeringMode(mode SteeringMode) {
	if al.steering == nil {
		return
	}
	al.steering.setMode(mode)
}

// dequeueSteeringMessages is the internal method called by the agent loop
// to poll for steering messages. Returns nil when no messages are pending.
func (al *AgentLoop) dequeueSteeringMessages() []providers.Message {
	if al.steering == nil {
		return nil
	}
	return al.steering.dequeue()
}

// Continue resumes an idle agent by dequeuing any pending steering messages
// and running them through the agent loop. This is used when the agent's last
// message was from the assistant (i.e., it has stopped processing) and the
// user has since enqueued steering messages.
//
// If no steering messages are pending, it returns an empty string.
func (al *AgentLoop) Continue(ctx context.Context, sessionKey, channel, chatID string) (string, error) {
	steeringMsgs := al.dequeueSteeringMessages()
	if len(steeringMsgs) == 0 {
		return "", nil
	}

	agent := al.GetRegistry().GetDefaultAgent()
	if agent == nil {
		return "", fmt.Errorf("no default agent available")
	}

	// Build a combined user message from the steering messages.
	var contents []string
	for _, msg := range steeringMsgs {
		contents = append(contents, msg.Content)
	}
	combinedContent := strings.Join(contents, "\n")

	return al.runAgentLoop(ctx, agent, processOptions{
		SessionKey:              sessionKey,
		Channel:                 channel,
		ChatID:                  chatID,
		UserMessage:             combinedContent,
		DefaultResponse:         defaultResponse,
		EnableSummary:           true,
		SendResponse:            false,
		SkipInitialSteeringPoll: true,
	})
}

// ====================== SubTurn Result Polling ======================

// dequeuePendingSubTurnResults polls the SubTurn result channel for the given
// session and returns all available results without blocking.
// Returns nil if no active turn state exists for this session.
//
// A running turn drains its own turnState directly (drainPendingResults); a
// session can have more than one registered turn, so a lookup by session key
// is only right for callers that have nothing but the key.
func (al *AgentLoop) dequeuePendingSubTurnResults(sessionKey string) []*tools.ToolResult {
	ts := al.activeTurnStates.activeForSession(sessionKey)
	if ts == nil {
		return nil
	}
	return ts.drainPendingResults()
}

// ====================== Hard Abort ======================

// HardAbort immediately cancels the running agent loop for the given session,
// cascading the cancellation to all child SubTurns. This is a destructive operation
// that terminates execution without waiting for graceful cleanup.
//
// Use this when the user explicitly requests immediate termination (e.g., "stop now", "abort").
// For graceful interruption that allows the agent to finish the current tool and summarize,
// use Steer() instead.
//
// HardAbort is CancelTurnsForSession plus a rollback: the messages the aborted
// turn added to the session are removed, and nothing that was there before the
// turn is touched. Every root turn registered for the session is aborted - the
// running one and any still waiting for the session lock. Only the running one
// has added anything, so only it is rolled back. Sub-turns left running by a
// turn of this session that has already returned are cancelled too; that turn
// finished normally, and what it wrote stays.
func (al *AgentLoop) HardAbort(sessionKey string) error {
	turns, orphaned := al.activeTurnStates.cancelTargets(sessionKey, false)
	if len(turns) == 0 && len(orphaned) == 0 {
		return fmt.Errorf("no active turn state found for session %s", sessionKey)
	}

	for _, ts := range turns {
		logger.InfoCF("agent", "Hard abort triggered", map[string]any{
			"session_key": sessionKey,
			"turn_id":     ts.turnID,
			"depth":       ts.depth,
		})

		// A turn's goroutine does not stop the instant its context is
		// cancelled: a tool call in flight still returns, and its result is
		// still appended to the session. Ask each turn to roll back again on
		// its way out, while it holds the session lock, so the rollback below
		// cannot be undone by a late write. Set before the cancellation, so a
		// turn that exits because of it is sure to see the request.
		ts.mu.Lock()
		ts.rollbackOnExit = true
		ts.mu.Unlock()
	}

	// IMPORTANT: Trigger cascading cancellation FIRST to stop all child SubTurns
	// from adding more messages to the session. This prevents race conditions
	// where rollback happens while children are still writing.
	abortTurns(turns, orphaned)

	// Roll back session history to the state before each turn started.
	for _, ts := range turns {
		ts.rollbackHistory()
	}

	return nil
}

// abortTurns hard-aborts turn trees: registered root turns (running or
// queued) and orphaned ones (returned, with sub-turns still running). Aborting
// a root cancels every sub-turn under it and refuses any spawned afterwards.
//
// Among the registered turns, the ones still waiting for their session lock
// go first. Cancelled in that order, a queued turn cannot inherit the session
// from the running one in the instant between the two cancellations and start
// work that is about to be cancelled anyway.
func abortTurns(registered, orphaned []*turnState) {
	for _, ts := range registered {
		if !ts.hasStarted() {
			ts.Finish(true)
		}
	}
	for _, ts := range registered {
		ts.Finish(true) // idempotent for the ones already aborted above
	}
	for _, ts := range orphaned {
		ts.Finish(true)
	}
}

// ====================== Turn Cancellation ======================

// CancelTurnsForSession cancels every turn of the session, together with its
// sub-turns, and returns how many turns it stopped. That covers:
//
//   - the root turn that is running and any waiting for the session lock;
//   - turns that have already returned but left async sub-turns running. The
//     sub-turns are what is cancelled; each such turn counts once.
//
// It returns without waiting for anything to stop. A cancelled turn returns
// from its provider or tool call with context.Canceled and releases the
// session lock on its way out. Session history is left as it is; use HardAbort
// to also roll back what the turn wrote.
func (al *AgentLoop) CancelTurnsForSession(sessionKey string) int {
	turns, orphaned := al.activeTurnStates.cancelTargets(sessionKey, false)
	abortTurns(turns, orphaned)
	stopped := len(turns) + len(orphaned)
	if stopped > 0 {
		logger.InfoCF("agent", "Cancelled turns for session", map[string]any{
			"session_key": sessionKey,
			"turns":       len(turns),
			"orphaned":    len(orphaned),
		})
	}
	return stopped
}

// CancelAllTurns is CancelTurnsForSession for every session at once: every
// running or queued root turn, and the sub-turns still running under turns
// that have already returned. It returns how many turns it stopped, counted
// the same way. reason is recorded in the log. It does not wait and does not
// roll back.
func (al *AgentLoop) CancelAllTurns(reason string) int {
	turns, orphaned := al.activeTurnStates.cancelTargets("", true)
	abortTurns(turns, orphaned)
	logger.InfoCF("agent", "Cancelled all turns", map[string]any{
		"reason":   reason,
		"turns":    len(turns),
		"orphaned": len(orphaned),
	})
	return len(turns) + len(orphaned)
}

// ====================== Follow-Up Injection ======================

// InjectFollowUp enqueues a message to be automatically processed after the current
// turn completes. Unlike Steer(), which interrupts the current execution, InjectFollowUp
// waits for the current turn to finish naturally before processing the message.
//
// This is useful for:
// - Automated workflows that need to chain multiple turns
// - Background tasks that should run after the main task completes
// - Scheduled follow-up actions
//
// The message will be processed via Continue() when the agent becomes idle.
func (al *AgentLoop) InjectFollowUp(msg providers.Message) error {
	// InjectFollowUp uses the same steering queue mechanism as Steer(),
	// but the semantic difference is in when it's called:
	// - Steer() is called during active execution to interrupt
	// - InjectFollowUp() is called when planning future work
	//
	// Both end up in the same queue and are processed by Continue()
	// when the agent is idle.
	return al.Steer(msg)
}

// ====================== API Aliases for Design Document Compatibility ======================

// InterruptGraceful is an alias for Steer() to match the design document naming.
// It gracefully interrupts the current execution by injecting a user message
// that will be processed after the current tool finishes.
func (al *AgentLoop) InterruptGraceful(msg providers.Message) error {
	return al.Steer(msg)
}

// InterruptHard is an alias for HardAbort() to match the design document naming.
// It immediately terminates execution and rolls back the session state.
func (al *AgentLoop) InterruptHard(sessionKey string) error {
	return al.HardAbort(sessionKey)
}

// InjectSteering is an alias for Steer() to match the design document naming.
// It injects a steering message into the currently running agent loop.
func (al *AgentLoop) InjectSteering(msg providers.Message) error {
	return al.Steer(msg)
}

// enqueueSteeringMessage enqueues a message into the steering queue for the given scope.
// Our fork uses a single global steering queue (no scope sharding), so the scope
// argument is ignored; messages always go into the shared queue.
func (al *AgentLoop) enqueueSteeringMessage(scope, _ string, msg providers.Message) error {
	if al.steering == nil {
		return fmt.Errorf("steering queue is not initialized")
	}
	if err := al.steering.push(msg); err != nil {
		logger.WarnCF("agent", "Failed to enqueue steering message", map[string]any{
			"error": err.Error(),
			"role":  msg.Role,
			"scope": scope,
		})
		return err
	}
	return nil
}

// resolveSteeringTarget determines the scope and agent for an inbound message.
// Returns (scopeKey, agentID, ok). Returns ok=false for system messages or
// messages that cannot be routed.
func (al *AgentLoop) resolveSteeringTarget(msg bus.InboundMessage) (string, string, bool) {
	if msg.Channel == "system" {
		return "", "", false
	}
	route, agent, err := al.resolveMessageRoute(msg)
	if err != nil || agent == nil {
		return "", "", false
	}
	return resolveScopeKey(route, msg.SessionKey), agent.ID, true
}

// requeueInboundMessage puts an inbound message back on the inbound bus so it
// is processed as a turn of its own later. The message goes back whole - sender,
// session key, media and metadata included - because that later turn routes on
// them.
//
// It must be the inbound bus. Publishing outbound, as this once did, sends the
// user's own message back to them as if the agent had said it, and the message
// is never processed.
func (al *AgentLoop) requeueInboundMessage(msg bus.InboundMessage) error {
	if al.bus == nil {
		return nil
	}
	pubCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	return al.bus.PublishInbound(pubCtx, msg)
}
