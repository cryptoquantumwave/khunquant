package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

<<<<<<< HEAD
	"github.com/khunquant/khunquant/pkg/providers"
	"github.com/khunquant/khunquant/pkg/session"
	"github.com/khunquant/khunquant/pkg/tools"
=======
	"github.com/khunquant/khunquant/pkg/logger"
	"github.com/khunquant/khunquant/pkg/providers"
	"github.com/khunquant/khunquant/pkg/session"
	"github.com/khunquant/khunquant/pkg/tools"
>>>>>>> 12a8590 (fix(agent): enhance SubTurn robustness and fix race conditions)
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
)

var (
	ErrDepthLimitExceeded       = errors.New("sub-turn depth limit exceeded")
	ErrInvalidSubTurnConfig     = errors.New("invalid sub-turn config")
	ErrConcurrencyLimitExceeded = errors.New("sub-turn concurrency limit exceeded")
	ErrConcurrencyTimeout       = errors.New("timeout waiting for concurrency slot")
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
//
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

	// Can be extended with temperature, topP, etc.
}

// ====================== Sub-turn Events (Aligned with EventBus) ======================
type SubTurnSpawnEvent struct {
	ParentID string
	ChildID  string
	Config   SubTurnConfig
}

type SubTurnEndEvent struct {
	ChildID string
	Result  *tools.ToolResult
	Err     error
}

type SubTurnResultDeliveredEvent struct {
	ParentID string
	ChildID  string
	Result   *tools.ToolResult
}

type SubTurnOrphanResultEvent struct {
	ParentID string
	ChildID  string
	Result   *tools.ToolResult
}

// ====================== Context Keys ======================
type turnStateKeyType struct{}
type agentLoopKeyType struct{}

var turnStateKey = turnStateKeyType{}
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

func withTurnState(ctx context.Context, ts *turnState) context.Context {
	return context.WithValue(ctx, turnStateKey, ts)
}

// TurnStateFromContext retrieves turnState from context (exported for tools)
func TurnStateFromContext(ctx context.Context) *turnState {
	return turnStateFromContext(ctx)
}

func turnStateFromContext(ctx context.Context) *turnState {
	ts, _ := ctx.Value(turnStateKey).(*turnState)
	return ts
}

type turnState struct {
	ctx                  context.Context
	cancelFunc           context.CancelFunc // Used to cancel all children when this turn finishes
	turnID               string
	parentTurnID         string
	depth                int
	childTurnIDs         []string // MUST be accessed under mu lock or maybe add a getter method
	pendingResults       chan *tools.ToolResult
	session              session.SessionStore
	initialHistoryLength int // Snapshot of session history length at turn start, for rollback on hard abort
	mu                   sync.Mutex
	isFinished           bool          // MUST be accessed under mu lock
	closeOnce            sync.Once     // Ensures pendingResults channel is closed exactly once
	concurrencySem       chan struct{} // Limits concurrent child sub-turns
}

// ====================== Public API ======================

// TurnInfo provides read-only information about an active turn.
type TurnInfo struct {
	TurnID       string
	ParentTurnID string
	Depth        int
	ChildTurnIDs []string
	IsFinished   bool
}

// GetActiveTurn retrieves information about the currently active turn for a session.
// Returns nil if no active turn exists for the given session key.
func (al *AgentLoop) GetActiveTurn(sessionKey string) *TurnInfo {
	tsInterface, ok := al.activeTurnStates.Load(sessionKey)
	if !ok {
		return nil
	}

	ts, ok := tsInterface.(*turnState)
	if !ok {
		return nil
	}

	return ts.Info()
}

// Info returns a read-only snapshot of the turn state information.
// This method is thread-safe and can be called concurrently.
func (ts *turnState) Info() *TurnInfo {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	// Create a copy of childTurnIDs to avoid race conditions
	childIDs := make([]string, len(ts.childTurnIDs))
	copy(childIDs, ts.childTurnIDs)

	return &TurnInfo{
		TurnID:       ts.turnID,
		ParentTurnID: ts.parentTurnID,
		Depth:        ts.depth,
		ChildTurnIDs: childIDs,
		IsFinished:   ts.isFinished,
	}
}

// ====================== Helper Functions ======================

func (al *AgentLoop) generateSubTurnID() string {
	return fmt.Sprintf("subturn-%d", al.subTurnCounter.Add(1))
}

func newTurnState(ctx context.Context, id string, parent *turnState) *turnState {
	// Note: We don't create a new context with cancel here because the caller
	// (spawnSubTurn) already creates one. The turnState stores the context and
	// cancelFunc provided by the caller to avoid redundant context wrapping.
	return &turnState{
		ctx:          ctx,
		cancelFunc:   nil, // Will be set by the caller
		turnID:       id,
		parentTurnID: parent.turnID,
		depth:        parent.depth + 1,
		session:      newEphemeralSession(parent.session),
		// NOTE: In this PoC, I use a fixed-size channel (16).
		// Under high concurrency or long-running sub-turns, this might fill up and cause
		// intermediate results to be discarded in deliverSubTurnResult.
		// For production, consider an unbounded queue or a blocking strategy with backpressure.
		pendingResults: make(chan *tools.ToolResult, 16),
		concurrencySem: make(chan struct{}, maxConcurrentSubTurns),
	}
}

// Finish marks the turn as finished and cancels its context, aborting any running sub-turns.
// It also closes the pendingResults channel to signal that no more results will be delivered.
// This method is safe to call multiple times - the channel will only be closed once.
// Any results remaining in the channel after close will be drained and emitted as orphan events.
func (ts *turnState) Finish() {
	ts.mu.Lock()
	ts.isFinished = true
	resultChan := ts.pendingResults
	ts.mu.Unlock()

	if ts.cancelFunc != nil {
		ts.cancelFunc()
	}

	// Use sync.Once to ensure the channel is closed exactly once, even if Finish() is called concurrently.
	// This prevents "close of closed channel" panics.
	ts.closeOnce.Do(func() {
		if resultChan != nil {
			close(resultChan)
			// Drain any remaining results from the channel and emit them as orphan events.
			// This prevents goroutine leaks and ensures all results are accounted for.
			ts.drainPendingResults(resultChan)
		}
	})
}

// drainPendingResults drains all remaining results from the closed channel
// and emits them as orphan events. This must be called after the channel is closed.
func (ts *turnState) drainPendingResults(ch chan *tools.ToolResult) {
	for result := range ch {
		if result != nil {
			MockEventBus.Emit(SubTurnOrphanResultEvent{
				ParentID: ts.turnID,
				ChildID:  "unknown", // We don't know which child this came from
				Result:   result,
			})
		}
	}
}

// ephemeralSessionStore is a pure in-memory SessionStore for SubTurns.
// It never writes to disk, keeping sub-turn history isolated from the parent session.
// It automatically truncates history when it exceeds maxEphemeralHistorySize to prevent memory accumulation.
type ephemeralSessionStore struct {
	mu      sync.Mutex
	history []providers.Message
	summary string
}

func (e *ephemeralSessionStore) AddMessage(sessionKey, role, content string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.history = append(e.history, providers.Message{Role: role, Content: content})
	e.autoTruncate()
}

func (e *ephemeralSessionStore) AddFullMessage(sessionKey string, msg providers.Message) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.history = append(e.history, msg)
	e.autoTruncate()
}

// autoTruncate automatically limits history size to prevent memory accumulation.
// Must be called with mu held.
func (e *ephemeralSessionStore) autoTruncate() {
	if len(e.history) > maxEphemeralHistorySize {
		// Keep only the most recent messages
		e.history = e.history[len(e.history)-maxEphemeralHistorySize:]
	}
}

func (e *ephemeralSessionStore) GetHistory(key string) []providers.Message {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]providers.Message, len(e.history))
	copy(out, e.history)
	return out
}

func (e *ephemeralSessionStore) GetSummary(key string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.summary
}

func (e *ephemeralSessionStore) SetSummary(key, summary string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.summary = summary
}

func (e *ephemeralSessionStore) SetHistory(key string, history []providers.Message) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.history = make([]providers.Message, len(history))
	copy(e.history, history)
}

func (e *ephemeralSessionStore) TruncateHistory(key string, keepLast int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.history) > keepLast {
		e.history = e.history[len(e.history)-keepLast:]
	}
}

func (e *ephemeralSessionStore) Save(key string) error { return nil }
func (e *ephemeralSessionStore) Close() error          { return nil }

// newEphemeralSession creates a new isolated ephemeral session for a sub-turn.
//
// IMPORTANT: The parent session parameter is intentionally unused (marked with _).
// This is by design according to issue #1316: sub-turns use completely isolated
// ephemeral sessions that do NOT inherit history from the parent session.
//
// Rationale for isolation:
//   - Sub-turns are independent execution contexts with their own prompts
//   - Inheriting parent history could cause context pollution
//   - Each sub-turn should start with a clean slate
//   - Memory is managed independently (auto-truncation at maxEphemeralHistorySize)
//   - Results are communicated back via the result channel, not via shared history
//
// If future requirements need parent history inheritance, this design decision
// should be reconsidered with careful attention to memory management and context size.
func newEphemeralSession(_ session.SessionStore) session.SessionStore {
	return &ephemeralSessionStore{}
}

// ====================== Core Function: spawnSubTurn ======================

// AgentLoopSpawner implements tools.SubTurnSpawner interface.
// This allows tools to spawn sub-turns without circular dependency.
type AgentLoopSpawner struct {
	al *AgentLoop
}

// SpawnSubTurn implements tools.SubTurnSpawner interface.
func (s *AgentLoopSpawner) SpawnSubTurn(ctx context.Context, cfg tools.SubTurnConfig) (*tools.ToolResult, error) {
	parentTS := turnStateFromContext(ctx)
	if parentTS == nil {
		return nil, errors.New("parent turnState not found in context - cannot spawn sub-turn outside of a turn")
	}

	// Convert tools.SubTurnConfig to agent.SubTurnConfig
	agentCfg := SubTurnConfig{
		Model:        cfg.Model,
		Tools:        cfg.Tools,
		SystemPrompt: cfg.SystemPrompt,
		MaxTokens:    cfg.MaxTokens,
		Async:        cfg.Async,
	}

	return spawnSubTurn(ctx, s.al, parentTS, agentCfg)
}

// NewSubTurnSpawner creates a SubTurnSpawner for the given AgentLoop.
func NewSubTurnSpawner(al *AgentLoop) *AgentLoopSpawner {
	return &AgentLoopSpawner{al: al}
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
		case <-timeoutCtx.Done():
			// Check if it was a timeout or parent context cancellation
			if timeoutCtx.Err() == context.DeadlineExceeded {
				return nil, fmt.Errorf("%w: all %d slots occupied for %v",
					ErrConcurrencyTimeout, maxConcurrentSubTurns, concurrencyTimeout)
			}
			return nil, ctx.Err()
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

	// 3. Create child Turn state with a cancellable context
	// This single context wrapping is sufficient - no need for additional layers.
	childCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	childID := al.generateSubTurnID()
	childTS := newTurnState(childCtx, childID, parentTS)
	// Set the cancel function so Finish() can trigger cascading cancellation
	childTS.cancelFunc = cancel

	// IMPORTANT: Put childTS into childCtx so that code inside runTurn can retrieve it
	childCtx = withTurnState(childCtx, childTS)
	childCtx = WithAgentLoop(childCtx, al) // Propagate AgentLoop to child turn

	// 4. Establish parent-child relationship (thread-safe)
	parentTS.mu.Lock()
	parentTS.childTurnIDs = append(parentTS.childTurnIDs, childID)
	parentTS.mu.Unlock()

	// 5. Emit Spawn event (currently using Mock, will be replaced by real EventBus)
	MockEventBus.Emit(SubTurnSpawnEvent{
		ParentID: parentTS.turnID,
		ChildID:  childID,
		Config:   cfg,
	})

	// 6. Defer cleanup: deliver result (for async), emit End event, and recover from panics
	// IMPORTANT: deliverSubTurnResult must be in defer to ensure it runs even if runTurn panics.
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("subturn panicked: %v", r)
		}

		// 7. Result Delivery Strategy (Async vs Sync)
		//
		// WHY we have different delivery mechanisms:
		// ==========================================
		//
		// Synchronous sub-turns (Async=false):
		//   - Caller expects immediate result via return value
		//   - Delivering to channel would cause DOUBLE DELIVERY:
		//     1. Caller gets result from return value
		//     2. Parent turn would poll channel and get the same result again
		//   - This would confuse the parent turn's result processing logic
		//   - Solution: Skip channel delivery, only return via function return
		//
		// Asynchronous sub-turns (Async=true):
		//   - Caller may not immediately process the return value
		//   - Result needs to be available for later polling via pendingResults
		//   - Parent turn can collect multiple async results in batches
		//   - Solution: Deliver to channel AND return via function return
		//
		// This must be in defer to ensure delivery even if runTurn panics.
		if cfg.Async {
			deliverSubTurnResult(parentTS, childID, result)
		}

		MockEventBus.Emit(SubTurnEndEvent{
			ChildID: childID,
			Result:  result,
			Err:     err,
		})
	}()

	// 7. Execute sub-turn via the real agent loop.
	// Build a child AgentInstance from SubTurnConfig, inheriting defaults from the parent agent.
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
func deliverSubTurnResult(parentTS *turnState, childID string, result *tools.ToolResult) {
	// Check parent state under lock, but don't hold lock while sending to channel
	parentTS.mu.Lock()
	isFinished := parentTS.isFinished
	resultChan := parentTS.pendingResults
	parentTS.mu.Unlock()

	// If parent turn has already finished, treat this as an orphan result
	if isFinished || resultChan == nil {
		if result != nil {
			MockEventBus.Emit(SubTurnOrphanResultEvent{
				ParentID: parentTS.turnID,
				ChildID:  childID,
				Result:   result,
			})
		}
		return
	}

	// Parent Turn is still running → attempt to deliver result
	// Note: There's still a small race window between the isFinished check above and the send below,
	// but this is acceptable - worst case the result becomes an orphan, which is handled gracefully.
	select {
	case resultChan <- result:
		// Successfully delivered
		MockEventBus.Emit(SubTurnResultDeliveredEvent{
			ParentID: parentTS.turnID,
			ChildID:  childID,
			Result:   result,
		})
	default:
		// Channel is full - treat as orphan result
		fmt.Println("[SubTurn] warning: pendingResults channel full")
		if result != nil {
			MockEventBus.Emit(SubTurnOrphanResultEvent{
				ParentID: parentTS.turnID,
				ChildID:  childID,
				Result:   result,
			})
		}
	}
}

// runTurn builds a temporary AgentInstance from SubTurnConfig and delegates to
// the real agent loop. The child's ephemeral session is used for history so it
// never pollutes the parent session.
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

	var toolRegistry *tools.ToolRegistry
	if len(cfg.Tools) > 0 {
		// Use explicitly provided tools
		toolRegistry = tools.NewToolRegistry()
		for _, t := range cfg.Tools {
			toolRegistry.Register(t)
		}
	} else {
		// Inherit tools from parent agent when cfg.Tools is nil or empty
		toolRegistry = tools.NewToolRegistry()
		for _, t := range parentAgent.Tools.GetAll() {
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

	finalContent, err := al.runAgentLoop(ctx, childAgent, processOptions{
		SessionKey:      ts.turnID,
		UserMessage:     cfg.SystemPrompt,
		DefaultResponse: "",
		EnableSummary:   false,
		SendResponse:    false,
	})
	if err != nil {
		return nil, err
	}
	return &tools.ToolResult{ForLLM: finalContent}, nil
}

// ====================== Other Types ======================
