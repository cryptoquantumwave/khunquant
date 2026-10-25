package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/providers"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
	"github.com/cryptoquantumwave/khunquant/pkg/utils"
)

// ====================== Config & Constants ======================
const (
	maxSubTurnDepth       = 3
	maxConcurrentSubTurns = 5
	// concurrencyTimeout is the maximum time to wait for a concurrency slot.
	// This prevents indefinite blocking when all slots are occupied by slow sub-turns.
	concurrencyTimeout = 30 * time.Second
	// maxEphemeralHistorySize limits the number of messages stored in ephemeral sessions.
	// This prevents memory accumulation in long-running sub-turns.
	maxEphemeralHistorySize = 50
	// defaultSubTurnTimeout is the default maximum duration for a SubTurn.
	// SubTurns that run longer than this will be cancelled.
	defaultSubTurnTimeout = 5 * time.Minute
)

var (
	ErrDepthLimitExceeded   = errors.New("sub-turn depth limit exceeded")
	ErrInvalidSubTurnConfig = errors.New("invalid sub-turn config")
	ErrConcurrencyTimeout   = errors.New("timeout waiting for concurrency slot")
)

// ====================== SubTurn Config ======================

// SubTurnConfig configures the execution of a child sub-turn.
//
// Usage Examples:
//
// Synchronous sub-turn (Async=false):
//
//	cfg := SubTurnConfig{
//	    Model: "gpt-4o-mini",
//	    SystemPrompt: "Analyze this code",
//	    Async: false,  // Result returned immediately
//	}
//	result, err := SpawnSubTurn(ctx, cfg)
//	// Use result directly here
//	processResult(result)
//
// Asynchronous sub-turn (Async=true):
//
//	cfg := SubTurnConfig{
//	    Model: "gpt-4o-mini",
//	    SystemPrompt: "Background analysis",
//	    Async: true,  // Result delivered to channel
//	}
//	result, err := SpawnSubTurn(ctx, cfg)
//	// Result also available in parent's pendingResults channel
//	// Parent turn will poll and process it in a later iteration
type SubTurnConfig struct {
	Model        string
	Tools        []tools.Tool
	SystemPrompt string
	MaxTokens    int

	// Async controls the result delivery mechanism:
	//
	// When Async = false (synchronous sub-turn):
	//   - The caller blocks until the sub-turn completes
	//   - The result is ONLY returned via the function return value
	//   - The result is NOT delivered to the parent's pendingResults channel
	//   - This prevents double delivery: caller gets result immediately, no need for channel
	//   - Use case: When the caller needs the result immediately to continue execution
	//   - Example: A tool that needs to process the sub-turn result before returning
	//
	// When Async = true (asynchronous sub-turn):
	//   - The sub-turn runs in the background (still blocks the caller, but semantically async)
	//   - The result is delivered to the parent's pendingResults channel
	//   - The result is ALSO returned via the function return value (for consistency)
	//   - The parent turn can poll pendingResults in later iterations to process results
	//   - Use case: Fire-and-forget operations, or when results are processed in batches
	//   - Example: Spawning multiple sub-turns in parallel and collecting results later
	//
	// IMPORTANT: The Async flag does NOT make the call non-blocking. It only controls
	// whether the result is delivered via the channel. For true non-blocking execution,
	// the caller must spawn the sub-turn in a separate goroutine.
	Async bool

	// Critical indicates this SubTurn's result is important and should continue
	// running even after the parent turn finishes gracefully.
	//
	// When parent finishes gracefully (Finish(false)):
	//   - Critical=true: SubTurn continues running, delivers result as orphan
	//   - Critical=false: SubTurn exits gracefully without error
	//
	// When parent finishes with hard abort (Finish(true)):
	//   - All SubTurns are cancelled regardless of Critical flag
	Critical bool

	// Timeout is the maximum duration for this SubTurn.
	// If the SubTurn runs longer than this, it will be cancelled.
	// Default is 5 minutes (defaultSubTurnTimeout) if not specified.
	Timeout time.Duration

	// MaxContextRunes limits the context size (in runes) passed to the SubTurn.
	// This prevents context window overflow by truncating message history before LLM calls.
	//
	// Values:
	//   0  = Auto-calculate based on model's ContextWindow * 0.75 (default, recommended)
	//   -1 = No limit (disable soft truncation, rely only on hard context errors)
	//   >0 = Use specified rune limit
	//
	// The soft limit acts as a first line of defense before hitting the provider's
	// hard context window limit. When exceeded, older messages are intelligently
	// truncated while preserving system messages and recent context.
	MaxContextRunes int

	// AgentID names the agent the child is built from: its tools, provider
	// and context builder. Empty means the default agent, which is only right
	// for callers with no agent of their own (the package-level SpawnSubTurn).
	AgentID string

	// Temperature overrides the sampling temperature of the child. Zero means
	// "not set": the child uses the temperature of the agent it is built from.
	Temperature float64

	// NoPendingDelivery stops an Async result from also being queued on the
	// parent's pendingResults (and so from being appended to the parent's final
	// response). It is for callers that deliver the result themselves, such as
	// the spawn tool's callback: delivering it twice would tell the user twice.
	// It does not change the session-lock rule for Async children.
	NoPendingDelivery bool

	// Can be extended with topP, etc.
}

// Sub-turn events (SubTurnSpawnEvent and the others) are defined in eventsink.go.

// ====================== Context Keys ======================
type agentLoopKeyType struct{}

var agentLoopKey = agentLoopKeyType{}

// WithAgentLoop injects AgentLoop into context for tool access
func WithAgentLoop(ctx context.Context, al *AgentLoop) context.Context {
	return context.WithValue(ctx, agentLoopKey, al)
}

// AgentLoopFromContext retrieves AgentLoop from context
func AgentLoopFromContext(ctx context.Context) *AgentLoop {
	al, _ := ctx.Value(agentLoopKey).(*AgentLoop)
	return al
}

// ====================== Helper Functions ======================

func (al *AgentLoop) generateSubTurnID() string {
	return fmt.Sprintf("subturn-%d", al.subTurnCounter.Add(1))
}

// ====================== Core Function: spawnSubTurn ======================

// AgentLoopSpawner implements tools.SubTurnSpawner interface.
// This allows tools to spawn sub-turns without circular dependency.
type AgentLoopSpawner struct {
	al *AgentLoop
	// agentID is the agent whose spawn/subagent/delegate tools own this
	// spawner. A child is built from it - never from the default agent, which
	// would hand a restricted agent's subagent the default agent's tools.
	agentID string
}

// SpawnSubTurn implements tools.SubTurnSpawner interface.
func (s *AgentLoopSpawner) SpawnSubTurn(ctx context.Context, cfg tools.SubTurnConfig) (*tools.ToolResult, error) {
	parentTS := turnStateFromContext(ctx)
	if parentTS == nil {
		return nil, errors.New("parent turnState not found in context - cannot spawn sub-turn outside of a turn")
	}

	// The child runs as the delegation target when there is one (M-6),
	// otherwise as the agent that spawned it.
	baseID := s.agentID
	if cfg.TargetAgentID != "" {
		baseID = cfg.TargetAgentID
	}
	model := cfg.Model
	if baseID != "" {
		base, ok := s.al.GetRegistry().GetAgent(baseID)
		if !ok {
			return nil, fmt.Errorf("unknown agent %q", baseID)
		}
		// delegate names an agent, not a model: use the agent's own.
		if model == "" {
			model = base.Model
		}
	}

	// Convert tools.SubTurnConfig to agent.SubTurnConfig
	agentCfg := SubTurnConfig{
		AgentID:         baseID,
		Model:           model,
		Tools:           cfg.Tools,
		SystemPrompt:    cfg.SystemPrompt,
		MaxTokens:       cfg.MaxTokens,
		Async:           cfg.Async,
		Critical:        cfg.Critical,
		Timeout:         cfg.Timeout,
		MaxContextRunes: cfg.MaxContextRunes,
		Temperature:     cfg.Temperature,

		NoPendingDelivery: cfg.NoPendingDelivery,
	}

	return spawnSubTurn(ctx, s.al, parentTS, agentCfg)
}

// NewSubTurnSpawner creates a SubTurnSpawner for the tools of agent agentID.
func NewSubTurnSpawner(al *AgentLoop, agentID string) *AgentLoopSpawner {
	return &AgentLoopSpawner{al: al, agentID: agentID}
}

// SpawnSubTurn is the exported entry point for tools to spawn sub-turns.
// It retrieves AgentLoop and parent turnState from context and delegates to spawnSubTurn.
func SpawnSubTurn(ctx context.Context, cfg SubTurnConfig) (*tools.ToolResult, error) {
	al := AgentLoopFromContext(ctx)
	if al == nil {
		return nil, errors.New("AgentLoop not found in context - ensure context is properly initialized")
	}

	parentTS := turnStateFromContext(ctx)
	if parentTS == nil {
		return nil, errors.New("parent turnState not found in context - cannot spawn sub-turn outside of a turn")
	}

	return spawnSubTurn(ctx, al, parentTS, cfg)
}

func spawnSubTurn(ctx context.Context, al *AgentLoop, parentTS *turnState, cfg SubTurnConfig) (result *tools.ToolResult, err error) {
	// 0. Acquire concurrency semaphore FIRST to ensure it's released even if early validation fails.
	// Blocks if parent already has maxConcurrentSubTurns running, with a timeout to prevent indefinite blocking.
	// Also respects context cancellation so we don't block forever if parent is aborted.
	var semAcquired bool
	if parentTS.concurrencySem != nil {
		// Create a timeout context for semaphore acquisition
		timeoutCtx, cancel := context.WithTimeout(ctx, concurrencyTimeout)
		defer cancel()

		select {
		case parentTS.concurrencySem <- struct{}{}:
			semAcquired = true
			defer func() {
				if semAcquired {
					<-parentTS.concurrencySem
				}
			}()
		case <-parentTS.Aborted():
			// The parent was hard-aborted while this spawn waited for a slot.
			// An async spawn runs on a context that outlives its parent's
			// normal return (see SpawnTool), so ctx does not report this.
			return nil, context.Canceled
		case <-timeoutCtx.Done():
			// Check parent context first - if it was cancelled, propagate that error
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			// Otherwise it's our timeout
			return nil, fmt.Errorf("%w: all %d slots occupied for %v",
				ErrConcurrencyTimeout, maxConcurrentSubTurns, concurrencyTimeout)
		}
	}

	// 1. Depth limit check
	if parentTS.depth >= maxSubTurnDepth {
		logger.WarnCF("subturn", "Depth limit exceeded", map[string]any{
			"parent_id": parentTS.turnID,
			"depth":     parentTS.depth,
			"max_depth": maxSubTurnDepth,
		})
		return nil, ErrDepthLimitExceeded
	}

	// 2. Config validation
	if cfg.Model == "" {
		return nil, ErrInvalidSubTurnConfig
	}

	// 3. Determine timeout for child SubTurn
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultSubTurnTimeout
	}

	// 4. Create INDEPENDENT child context (not derived from parent ctx).
	// This allows the child to continue running after parent finishes gracefully.
	// The child has its own timeout for self-protection.
	childCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	childID := al.generateSubTurnID()
	childTS := newTurnState(childCtx, childID, parentTS)
	// Set the cancel function so Finish(true) can trigger hard cancellation
	childTS.cancelFunc = cancel
	childTS.critical = cfg.Critical

	// IMPORTANT: Put childTS into childCtx so that code inside runTurn can retrieve it
	childCtx = withTurnState(childCtx, childTS)
	childCtx = WithAgentLoop(childCtx, al) // Propagate AgentLoop to child turn
	// childCtx starts from Background, so the policy tags on the spawning
	// turn do not carry over by themselves - and a child is never a root turn,
	// so runAgentLoop will not tag it either. Untagged fails closed, which
	// would refuse every financial tool a person's subagent calls; worse in
	// spirit, a cron job's subagent would stop being "cron". Carry both.
	childCtx = tools.WithToolOrigin(childCtx, tools.ToolOrigin(ctx))
	childCtx = tools.WithToolSender(childCtx, tools.ToolSender(ctx))
	// Say it is a sub-turn too: with the origin and sender above it looks like the
	// turn that spawned it, and what a model wrote for it is not that turn's person.
	childCtx = tools.WithSubTurn(childCtx)
	// The job scope too: a child must not lose the ceiling's job-bound exception
	// nor be able to claim one its parent did not have.
	if scope, ok := tools.AutomationScopeFrom(ctx); ok {
		childCtx = tools.WithAutomationScope(childCtx, scope)
	}
	// The turn ID has to be carried for the same reason. A child shares its
	// parent's: a message the subagent sends to the user's chat is a message
	// sent in the parent's turn, and must suppress the parent's duplicate
	// final reply exactly as if the parent had sent it.
	childCtx = tools.WithTurnID(childCtx, tools.TurnID(ctx))
	// A synchronous child runs while its caller - a tool call inside the
	// parent turn - is blocked on it, so it is still working under the parent's
	// session lock and may re-enter it; without the marker it would deadlock
	// against its own parent. An async child gets no such claim: it can
	// outlive the parent turn, and with it the lock.
	if !cfg.Async {
		childCtx = inheritHeldSessionLocks(childCtx, ctx)
	}

	// 5. Establish parent-child relationship (thread-safe). The child is
	// registered with its parent so a hard abort can reach it - its context is
	// independent of the parent's, so nothing else would. A parent that has
	// already been hard-aborted takes no new children: the abort walked the
	// tree before this one existed.
	if !parentTS.addChild(childTS) {
		return nil, context.Canceled
	}
	defer parentTS.removeChild(childTS)
	// Also listed loop-wide. An async child outlives its root turn, and a root
	// that has returned is no longer registered - without this a cancel issued
	// after the parent finished could not find the child at all.
	al.activeTurnStates.addSubTurn(childTS)
	defer al.activeTurnStates.removeSubTurn(childTS)

	// 6. Emit Spawn event. The sink is read once so this sub-turn's events all
	// go to the same place even if the loop's sink is replaced meanwhile. The
	// event carries identifiers and settings, never the prompt.
	sink := al.subTurnSink()
	startedAt := time.Now()
	sink.emit(SubTurnSpawnEvent{
		ParentID:  parentTS.turnID,
		ChildID:   childID,
		AgentID:   cfg.AgentID,
		Model:     cfg.Model,
		Async:     cfg.Async,
		Critical:  cfg.Critical,
		ToolCount: len(cfg.Tools),
		Timeout:   timeout,
	})

	// 7. Defer cleanup: deliver result (for async), emit End event, and recover from panics
	defer func() {
		panicked := false
		if r := recover(); r != nil {
			panicked = true
			err = fmt.Errorf("subturn panicked: %v", r)
			result = nil
			logger.ErrorCF("subturn", "SubTurn panicked", map[string]any{
				"child_id":  childID,
				"parent_id": parentTS.turnID,
				"panic":     r,
			})
		}

		// Result Delivery Strategy (Async vs Sync)
		if cfg.Async && !cfg.NoPendingDelivery {
			deliverSubTurnResult(sink, parentTS, childID, result)
		}

		end := SubTurnEndEvent{
			ParentID: parentTS.turnID,
			ChildID:  childID,
			Status:   subTurnStatus(err, panicked),
			Duration: time.Since(startedAt),
		}
		if result != nil {
			end.HasResult = true
			end.ResultIsErr = result.IsError
			end.ResultBytes = len(result.ForLLM)
		}
		sink.emit(end)
	}()

	// 8. Execute sub-turn via the real agent loop.
	result, err = runTurn(childCtx, al, childTS, cfg)

	return result, err
}

// ====================== Result Delivery ======================

// deliverSubTurnResult delivers a sub-turn result to the parent turn's pendingResults channel.
//
// IMPORTANT: This function is ONLY called for asynchronous sub-turns (Async=true).
// For synchronous sub-turns (Async=false), results are returned directly via the function
// return value to avoid double delivery.
//
// Delivery behavior:
//   - If parent turn is still running: attempts to deliver to pendingResults channel
//   - If channel is full: emits SubTurnOrphanResultEvent (result is lost from channel but tracked)
//   - If parent turn has finished: emits SubTurnOrphanResultEvent (late arrival)
//
// Thread safety:
//   - Reads parent state under lock, then releases lock before channel send
//   - Small race window exists but is acceptable (worst case: result becomes orphan)
//
// Event emissions:
//   - SubTurnResultDeliveredEvent: successful delivery to channel
//   - SubTurnOrphanResultEvent: delivery failed (parent finished or channel full)
func deliverSubTurnResult(sink eventSink, parentTS *turnState, childID string, result *tools.ToolResult) {
	if sink == nil {
		sink = noopSink{}
	}
	// Let GC clean up the pendingResults channel; parent Finish will no longer close it.
	// We use defer/recover to catch any unlikely channel panics if it were ever closed.
	defer func() {
		if r := recover(); r != nil {
			logger.RecoverPanicNoExit(r)
			logger.WarnCF("subturn", "recovered panic sending to pendingResults", map[string]any{
				"parent_id": parentTS.turnID,
				"child_id":  childID,
				"recover":   r,
			})
			if result != nil {
				sink.emit(SubTurnOrphanResultEvent{
					ParentID: parentTS.turnID,
					ChildID:  childID,
				})
			}
		}
	}()
	parentTS.mu.Lock()
	isFinished := parentTS.isFinished
	resultChan := parentTS.pendingResults
	parentTS.mu.Unlock()

	// If parent turn has already finished, treat this as an orphan result
	if isFinished || resultChan == nil {
		if result != nil {
			sink.emit(SubTurnOrphanResultEvent{
				ParentID: parentTS.turnID,
				ChildID:  childID,
			})
		}
		return
	}

	// Parent Turn is still running → attempt to deliver result
	// We use a select statement with parentTS.Finished() to ensure that if the
	// parent turn finishes while we are waiting to send the result (e.g. channel
	// is full), we don't leak this goroutine by blocking forever.
	select {
	case resultChan <- result:
		// Successfully delivered
		sink.emit(SubTurnResultDeliveredEvent{
			ParentID: parentTS.turnID,
			ChildID:  childID,
		})
	case <-parentTS.Finished():
		// Parent finished while we were waiting to deliver.
		// The result cannot be delivered to the LLM, so it becomes an orphan.
		logger.WarnCF("subturn", "parent finished before result could be delivered", map[string]any{
			"parent_id": parentTS.turnID,
			"child_id":  childID,
		})
		if result != nil {
			sink.emit(SubTurnOrphanResultEvent{
				ParentID: parentTS.turnID,
				ChildID:  childID,
			})
		}
	}
}

// isSubTurnSpawningTool reports whether the named tool starts sub-turns. A child
// is never given one.
func isSubTurnSpawningTool(name string) bool {
	switch name {
	case tools.NameSpawn, tools.NameDelegate, "subagent":
		return true
	}
	return false
}

// runTurn builds a temporary AgentInstance from SubTurnConfig and delegates to
// the real agent loop. The child's ephemeral session is used for history so it
// never pollutes the parent session.
//
// This function implements multiple layers of context protection and error recovery:
//
// 1. Soft Context Limit (MaxContextRunes):
//   - Proactively truncates message history before LLM calls
//   - Default: 75% of model's context window
//   - Preserves system messages and recent context
//   - First line of defense against context overflow
//
// 2. Hard Context Error Recovery:
//   - Detects context_length_exceeded errors from provider
//   - Triggers force compression and retries (up to 2 times)
//   - Second line of defense when soft limit is insufficient
//
// 3. Truncation Recovery:
//   - Detects when LLM response is truncated (finish_reason="truncated")
//   - Injects recovery prompt asking for shorter response
//   - Retries up to 2 times
//   - Handles cases where max_tokens is hit
func runTurn(ctx context.Context, al *AgentLoop, ts *turnState, cfg SubTurnConfig) (*tools.ToolResult, error) {
	// Derive candidates from the requested model using the parent loop's provider.
	defaultProvider := al.GetConfig().Agents.Defaults.Provider
	candidates := providers.ResolveCandidates(
		providers.ModelConfig{Primary: cfg.Model},
		defaultProvider,
	)

	// Build a minimal AgentInstance for this sub-turn.
	// It reuses the parent loop's provider and config, but gets its own
	// ephemeral session store and tool registry.
	parentAgent := al.GetRegistry().GetDefaultAgent()
	if cfg.AgentID != "" {
		a, ok := al.GetRegistry().GetAgent(cfg.AgentID)
		if !ok {
			return nil, fmt.Errorf("unknown agent %q", cfg.AgentID)
		}
		parentAgent = a
	}

	// Determine which tools to use: explicit config or inherit from parent
	toolRegistry := tools.NewToolRegistry()
	toolsToRegister := cfg.Tools
	if len(toolsToRegister) == 0 {
		toolsToRegister = parentAgent.Tools.GetAll()
	}
	for _, t := range toolsToRegister {
		// A child cannot start children of its own, whichever way its tools
		// were chosen: depth stays at 1.
		if isSubTurnSpawningTool(t.Name()) {
			continue
		}
		toolRegistry.Register(t)
	}
	// A child gets the light prompt: skills are listed by name and read through
	// load_skill. Keep that tool in the child whichever way its tools were
	// chosen, or the index would name skills the child cannot open.
	if _, ok := toolRegistry.Get(tools.NameLoadSkill); !ok {
		if t, ok := parentAgent.Tools.Get(tools.NameLoadSkill); ok {
			toolRegistry.Register(t)
		}
	}

	childAgent := &AgentInstance{
		ID:                        ts.turnID,
		Model:                     cfg.Model,
		MaxIterations:             parentAgent.MaxIterations,
		MaxTokens:                 cfg.MaxTokens,
		Temperature:               parentAgent.Temperature,
		ThinkingLevel:             parentAgent.ThinkingLevel,
		ContextWindow:             parentAgent.ContextWindow, // Inherit from parent agent
		SummarizeMessageThreshold: parentAgent.SummarizeMessageThreshold,
		SummarizeTokenPercent:     parentAgent.SummarizeTokenPercent,
		Provider:                  parentAgent.Provider,
		Sessions:                  ts.session,
		ContextBuilder:            parentAgent.ContextBuilder,
		Tools:                     toolRegistry,
		Candidates:                candidates,
	}
	if childAgent.MaxTokens == 0 {
		childAgent.MaxTokens = parentAgent.MaxTokens
	}
	if cfg.Temperature != 0 {
		childAgent.Temperature = cfg.Temperature
	}

	// Resolve MaxContextRunes configuration
	maxContextRunes := utils.ResolveMaxContextRunes(cfg.MaxContextRunes, childAgent.ContextWindow)

	logger.DebugCF("subturn", "Context limit resolved",
		map[string]any{
			"turn_id":           ts.turnID,
			"context_window":    childAgent.ContextWindow,
			"max_context_runes": maxContextRunes,
			"configured_value":  cfg.MaxContextRunes,
		})

	// Retry loop for truncation and context errors
	const (
		maxTruncationRetries = 2
		maxContextRetries    = 2
	)

	truncationRetryCount := 0
	contextRetryCount := 0
	currentPrompt := cfg.SystemPrompt
	promptAlreadyAdded := false

	for {
		// Soft context limit: check and truncate before LLM call
		if maxContextRunes > 0 {
			messages := childAgent.Sessions.GetHistory(ts.turnID)
			currentRunes := utils.MeasureContextRunes(messages)

			if currentRunes > maxContextRunes {
				logger.WarnCF("subturn", "Context exceeds soft limit, truncating",
					map[string]any{
						"turn_id":       ts.turnID,
						"current_runes": currentRunes,
						"max_runes":     maxContextRunes,
						"overflow":      currentRunes - maxContextRunes,
					})

				truncatedMessages := utils.TruncateContextSmart(messages, maxContextRunes)
				childAgent.Sessions.SetHistory(ts.turnID, truncatedMessages)

				// Log truncation result
				newRunes := utils.MeasureContextRunes(truncatedMessages)
				logger.InfoCF("subturn", "Context truncated successfully",
					map[string]any{
						"turn_id":      ts.turnID,
						"before_runes": currentRunes,
						"after_runes":  newRunes,
						"saved_runes":  currentRunes - newRunes,
					})
			}
		}

		// Call the agent loop
		finalContent, err := al.runAgentLoop(ctx, childAgent, processOptions{
			SessionKey:         ts.turnID,
			UserMessage:        currentPrompt,
			DefaultResponse:    "",
			EnableSummary:      false,
			SendResponse:       false,
			SkipAddUserMessage: promptAlreadyAdded,
			// Background work: the light prompt is a fraction of the full
			// one, and a child that needs a skill loads it by name.
			PromptMode: PromptLight,
		})

		// Mark the prompt as added so subsequent truncation retries
		// won't duplicate it in the history.
		promptAlreadyAdded = true

		// 1. Handle context length errors
		if err != nil && isContextLengthError(err) {
			if contextRetryCount >= maxContextRetries {
				logger.ErrorCF("subturn", "Context limit exceeded after max retries",
					map[string]any{
						"turn_id":     ts.turnID,
						"retries":     contextRetryCount,
						"max_retries": maxContextRetries,
					})
				return nil, fmt.Errorf("context limit exceeded after %d retries: %w", maxContextRetries, err)
			}

			logger.WarnCF("subturn", "Context length exceeded, compressing and retrying",
				map[string]any{
					"turn_id": ts.turnID,
					"retry":   contextRetryCount + 1,
				})

			// Trigger force compression via context manager
			_ = al.contextManager.Compact(ctx, &CompactRequest{
				SessionKey: ts.turnID,
				Reason:     ContextCompressReasonRetry,
				Budget:     childAgent.ContextWindow,
			})

			contextRetryCount++
			continue // Retry with compressed history
		}

		if err != nil {
			return nil, err // Other errors, return immediately
		}

		// 2. Check for truncation (retrieve finishReason from turnState)
		finishReason := ts.GetLastFinishReason()

		if finishReason == "truncated" && truncationRetryCount < maxTruncationRetries {
			logger.WarnCF("subturn", "Response truncated, injecting recovery message",
				map[string]any{
					"turn_id": ts.turnID,
					"retry":   truncationRetryCount + 1,
				})

			// IMPORTANT: Do NOT manually add messages to history here.
			// runAgentLoop has already saved both the assistant message (finalContent)
			// and will save the next user message (currentPrompt) on the next iteration.
			// Manually adding them would cause duplicates.

			// Inject recovery prompt - it will be added by runAgentLoop on next iteration
			recoveryPrompt := "Your previous response was truncated due to length. Please provide a shorter, complete response that finishes your thought."
			currentPrompt = recoveryPrompt
			promptAlreadyAdded = false // We need this new recovery prompt to be added

			truncationRetryCount++
			continue // Retry with recovery prompt
		}

		// 3. Success - return result
		return &tools.ToolResult{ForLLM: finalContent}, nil
	}
}

// isContextLengthError checks if the error is due to context length exceeded.
// It excludes timeout errors to avoid false positives.
func isContextLengthError(err error) bool {
	if err == nil {
		return false
	}
	errMsg := strings.ToLower(err.Error())

	// Exclude timeout errors
	if strings.Contains(errMsg, "timeout") || strings.Contains(errMsg, "deadline exceeded") {
		return false
	}

	// Detect context error patterns
	return strings.Contains(errMsg, "context_length_exceeded") ||
		strings.Contains(errMsg, "maximum context length") ||
		strings.Contains(errMsg, "context window") ||
		strings.Contains(errMsg, "too many tokens") ||
		strings.Contains(errMsg, "token limit") ||
		strings.Contains(errMsg, "prompt is too long")
}

// ====================== Other Types ======================
