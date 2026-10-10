package agent

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/providers"
	"github.com/cryptoquantumwave/khunquant/pkg/session"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

// ====================== Context Keys ======================
type turnStateKeyType struct{}

var turnStateKey = turnStateKeyType{}

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

// ====================== turnState ======================

type turnState struct {
	ctx context.Context
	// cancelFunc cancels this turn's own context. Finish(true) calls it and
	// then hard-aborts every child; a graceful finish leaves children running.
	cancelFunc context.CancelFunc
	// turnID identifies the turn. A root turn's ID is the one carried in its
	// context (tools.TurnID) and is the key it is registered under; a
	// sub-turn's is "subturn-N".
	turnID string
	// sessionKey is the session a root turn runs on. Empty for sub-turns, which
	// are found through their parent rather than by session.
	sessionKey     string
	parentTurnID   string
	depth          int
	childTurnIDs   []string // MUST be accessed under mu lock or maybe add a getter method
	pendingResults chan *tools.ToolResult
	session        session.SessionStore
	// initialHistoryLength is the session history length when the turn began,
	// the point a hard abort rolls back to. It is only meaningful once
	// historySnapshotTaken is set, which happens after the turn has acquired
	// its session lock: measured any earlier it would count messages of
	// whichever turn holds the lock at that moment. Both are guarded by mu,
	// since HardAbort reads them from another goroutine.
	initialHistoryLength int
	historySnapshotTaken bool
	// rollbackOnExit asks the turn to roll its session back once more as it
	// returns. HardAbort rolls back at once, but the aborted turn may still be
	// inside a tool call and append that call's result afterwards. Guarded by mu.
	rollbackOnExit bool
	// exited is set when a root turn has returned and given up its session.
	// Guarded by mu.
	exited bool
	// children are the sub-turns currently running under this turn, so a hard
	// abort can reach them: their contexts are deliberately not derived from
	// this turn's, to let them outlive a graceful finish. Guarded by mu.
	children []*turnState
	// hardAborted is set by Finish(true). A turn that has been hard-aborted
	// accepts no new children. Guarded by mu.
	hardAborted bool
	// abortedChan is closed on hard abort. Lazily initialized under mu.
	abortedChan chan struct{}
	mu          sync.Mutex
	isFinished           bool          // MUST be accessed under mu lock
	closeOnce            sync.Once     // Ensures pendingResults channel is closed exactly once
	concurrencySem       chan struct{} // Limits concurrent child sub-turns
	finishedChan         chan struct{} // Lazily initialized, closed when turn finishes

	// parentEnded signals that the parent turn has finished gracefully.
	// Child SubTurns should check this via IsParentEnded() to decide whether
	// to continue running (Critical=true) or exit gracefully (Critical=false).
	parentEnded atomic.Bool

	// critical indicates whether this SubTurn should continue running after
	// the parent turn finishes gracefully. Set from SubTurnConfig.Critical.
	critical bool

	// parentTurnState holds a reference to the parent turnState.
	// This allows child SubTurns to check if the parent has ended.
	// Nil for root turns.
	parentTurnState *turnState

	// lastFinishReason stores the finish_reason from the last LLM call.
	// Used by SubTurn to detect truncation and retry.
	// MUST be accessed under mu lock.
	lastFinishReason string
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
//
// Several root turns can be registered for one session - one running, the rest
// waiting for the session lock. The running one is reported; if none has
// started yet, the one that registered first.
func (al *AgentLoop) GetActiveTurn(sessionKey string) *TurnInfo {
	ts := al.activeTurnStates.activeForSession(sessionKey)
	if ts == nil {
		return nil
	}
	return ts.Info()
}

// ====================== Turn Registry ======================

// turnRegistry tracks the root turns that are running or waiting for their
// session lock.
//
// Turns are keyed by turn ID, not by session key: two turns on one session
// are distinct entries (a heartbeat always uses the key "heartbeat", and a cron
// turn shares the session of the chat it reports to), so one finishing cannot
// remove the other. bySession is the index that answers "which turns belong to
// this session", in registration order.
//
// The zero value is ready to use.
type turnRegistry struct {
	mu        sync.Mutex
	byID      map[string]*turnState
	bySession map[string][]*turnState
	// subTurns is every sub-turn that is running right now, whatever its
	// depth. A root turn unregisters when it returns, but its async sub-turns
	// may run on; this is how a cancel still finds them.
	subTurns map[*turnState]struct{}
}

// addSubTurn records a running sub-turn; removeSubTurn forgets it when it returns.
func (r *turnRegistry) addSubTurn(ts *turnState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.subTurns == nil {
		r.subTurns = make(map[*turnState]struct{})
	}
	r.subTurns[ts] = struct{}{}
}

func (r *turnRegistry) removeSubTurn(ts *turnState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.subTurns, ts)
}

// cancelTargets returns what a cancel has to abort, for one session or (all)
// for every session:
//
//   - registered: the root turns that are running or queued, oldest first;
//   - orphaned: root turns that have already returned but still have sub-turns
//     running - async sub-turns outlive a parent's normal return by design.
//
// Aborting a root reaches every sub-turn under it, so the roots are all a
// caller needs. Both lists come from one look at the registry, so no turn is
// in both.
func (r *turnRegistry) cancelTargets(sessionKey string, all bool) (registered, orphaned []*turnState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if all {
		for _, ts := range r.byID {
			registered = append(registered, ts)
		}
	} else {
		registered = append(registered, r.bySession[sessionKey]...)
	}
	seen := make(map[*turnState]struct{})
	for sub := range r.subTurns {
		root := sub.root()
		if r.byID[root.turnID] == root {
			continue // still registered: already in the first list
		}
		if !all && root.sessionKey != sessionKey {
			continue
		}
		if _, dup := seen[root]; dup {
			continue
		}
		seen[root] = struct{}{}
		orphaned = append(orphaned, root)
	}
	return registered, orphaned
}

// register adds a root turn. It must be called before ts is shared with
// another goroutine, because it may rewrite ts.turnID: IDs are unique for
// every turn the loop mints itself, but a caller that supplies its own ID in
// the context could reuse one for two overlapping turns, and the second must
// not replace the first.
func (r *turnRegistry) register(ts *turnState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.byID == nil {
		r.byID = make(map[string]*turnState)
		r.bySession = make(map[string][]*turnState)
	}
	if _, taken := r.byID[ts.turnID]; taken || ts.turnID == "" {
		ts.turnID = ts.turnID + "#" + tools.NewTurnID()
	}
	r.byID[ts.turnID] = ts
	r.bySession[ts.sessionKey] = append(r.bySession[ts.sessionKey], ts)
}

// unregister removes exactly ts. It is a no-op if ts is not registered, so it
// is safe to call twice.
func (r *turnRegistry) unregister(ts *turnState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.byID[ts.turnID] != ts {
		return
	}
	delete(r.byID, ts.turnID)
	turns := r.bySession[ts.sessionKey]
	for i, t := range turns {
		if t == ts {
			turns = append(turns[:i], turns[i+1:]...)
			break
		}
	}
	if len(turns) == 0 {
		delete(r.bySession, ts.sessionKey)
	} else {
		r.bySession[ts.sessionKey] = turns
	}
}

// forSession returns the root turns registered for the session, oldest first.
func (r *turnRegistry) forSession(sessionKey string) []*turnState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*turnState(nil), r.bySession[sessionKey]...)
}

// activeForSession returns the session's running turn - the one that holds the
// session lock - or, if none has started, the first one registered. Nil if the
// session has no turn.
func (r *turnRegistry) activeForSession(sessionKey string) *turnState {
	turns := r.forSession(sessionKey)
	if len(turns) == 0 {
		return nil
	}
	for _, ts := range turns {
		if ts.hasStarted() {
			return ts
		}
	}
	return turns[0]
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

func newTurnState(ctx context.Context, id string, parent *turnState) *turnState {
	// Note: We don't create a new context with cancel here because the caller
	// (spawnSubTurn) already creates one. The turnState stores the context and
	// cancelFunc provided by the caller to avoid redundant context wrapping.
	return &turnState{
		ctx:             ctx,
		cancelFunc:      nil, // Will be set by the caller
		turnID:          id,
		parentTurnID:    parent.turnID,
		depth:           parent.depth + 1,
		session:         newEphemeralSession(parent.session),
		parentTurnState: parent, // Store reference to parent for IsParentEnded() checks
		// NOTE: In this PoC, I use a fixed-size channel (16).
		// Under high concurrency or long-running sub-turns, this might fill up and cause
		// intermediate results to be discarded in deliverSubTurnResult.
		// For production, consider an unbounded queue or a blocking strategy with backpressure.
		pendingResults: make(chan *tools.ToolResult, 16),
		concurrencySem: make(chan struct{}, maxConcurrentSubTurns),
	}
}

// IsParentEnded returns true if the parent turn has finished gracefully.
// This is safe to call from child SubTurn goroutines.
// Returns false if this is a root turn (no parent).
func (ts *turnState) IsParentEnded() bool {
	if ts.parentTurnState == nil {
		return false
	}
	return ts.parentTurnState.parentEnded.Load()
}

// SetLastFinishReason updates the last finish reason (thread-safe).
func (ts *turnState) SetLastFinishReason(reason string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.lastFinishReason = reason
}

// GetLastFinishReason retrieves the last finish reason (thread-safe).
func (ts *turnState) GetLastFinishReason() string {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.lastFinishReason
}

// IsParentEnded is a convenience method to check if parent ended.
// It returns the value of the parent's parentEnded atomic flag.

// Finished returns a channel that is closed when the turn finishes.
// This allows child turns to safely block on delivering results without leaking
// if the parent finishes before they can deliver.
func (ts *turnState) Finished() <-chan struct{} {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.finishedChan == nil {
		ts.finishedChan = make(chan struct{})
		if ts.isFinished {
			close(ts.finishedChan)
		}
	}
	return ts.finishedChan
}

// Aborted returns a channel that is closed when the turn is hard-aborted. A
// graceful finish does not close it.
func (ts *turnState) Aborted() <-chan struct{} {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.abortedChan == nil {
		// Finish(true) creates the channel itself before closing it, so a nil
		// channel here means no hard abort has happened yet.
		ts.abortedChan = make(chan struct{})
	}
	return ts.abortedChan
}

// addChild records child as a running sub-turn of ts. It returns false, and
// records nothing, if ts has been hard-aborted: the abort has already walked
// the children, so one added now would never be cancelled.
func (ts *turnState) addChild(child *turnState) bool {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.hardAborted {
		return false
	}
	ts.children = append(ts.children, child)
	ts.childTurnIDs = append(ts.childTurnIDs, child.turnID)
	return true
}

// removeChild forgets a sub-turn that has returned. Its ID stays in
// childTurnIDs, which is the record of everything this turn spawned.
func (ts *turnState) removeChild(child *turnState) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	for i, c := range ts.children {
		if c == child {
			ts.children = append(ts.children[:i], ts.children[i+1:]...)
			return
		}
	}
}

// setHistorySnapshot records n, the length of the turn's session history, as
// the point a hard abort rolls back to. A root turn sets it once it holds its
// session lock and before it writes anything.
func (ts *turnState) setHistorySnapshot(n int) {
	ts.mu.Lock()
	ts.initialHistoryLength = n
	ts.historySnapshotTaken = true
	ts.mu.Unlock()
}

// snapshotHistory is setHistorySnapshot for a caller that has not already
// loaded the session: it reads the history to measure it.
func (ts *turnState) snapshotHistory() {
	if ts.session == nil {
		return
	}
	ts.setHistorySnapshot(len(ts.session.GetHistory(ts.sessionKey)))
}

// root returns the root turn of the tree ts belongs to (ts itself for a root).
// The parent links are set once, before a turn is shared, so no lock is needed.
func (ts *turnState) root() *turnState {
	for ts.parentTurnState != nil {
		ts = ts.parentTurnState
	}
	return ts
}

// exit is called by a root turn as it returns, while it still holds its
// session lock. If a hard abort asked for it, the session is rolled back one
// last time, now that the turn can no longer write; after that the turn is
// marked exited and never touches the session again - see rollbackHistory.
func (ts *turnState) exit() {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.rollbackOnExit {
		ts.rollbackHistoryLocked()
	}
	ts.exited = true
}

// hasStarted reports whether the turn has got past waiting for its session
// lock and begun work.
func (ts *turnState) hasStarted() bool {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.historySnapshotTaken
}

// rollbackHistory drops the messages this turn added to its session, leaving
// what was there before the turn began. It does nothing for a turn that never
// took its snapshot - one cancelled while still waiting for the session lock
// has added nothing, and has no length to roll back to.
//
// It also does nothing once the turn has exited. From that moment the session
// lock is free and the session may belong to the next turn; truncating it to
// this turn's snapshot would cut into that turn's messages. mu is held across
// the rollback so that "has not exited" stays true while it runs: exit takes
// the same lock before the turn gives the session up.
func (ts *turnState) rollbackHistory() {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.exited {
		return
	}
	ts.rollbackHistoryLocked()
}

// rollbackHistoryLocked is rollbackHistory's body. The caller holds mu.
func (ts *turnState) rollbackHistoryLocked() {
	keep, ok := ts.initialHistoryLength, ts.historySnapshotTaken
	if !ok || ts.session == nil {
		return
	}
	history := ts.session.GetHistory(ts.sessionKey)
	if len(history) <= keep {
		return
	}
	logger.InfoCF("agent", "Rolling back session history", map[string]any{
		"session_key": ts.sessionKey,
		"turn_id":     ts.turnID,
		"from":        len(history),
		"to":          keep,
	})
	ts.session.SetHistory(ts.sessionKey, history[:keep])
}

// drainPendingResults returns the sub-turn results waiting for this turn,
// without blocking.
func (ts *turnState) drainPendingResults() []*tools.ToolResult {
	var results []*tools.ToolResult
	for {
		select {
		case result := <-ts.pendingResults:
			if result != nil {
				results = append(results, result)
			}
		default:
			return results
		}
	}
}

// Finish marks the turn as finished.
//
// If isHardAbort is true (Hard Abort):
//   - Cancels this turn's context via cancelFunc
//   - Hard-aborts every running child, and theirs, by walking the turn tree
//     (child contexts are independent of the parent's, so cancelling the
//     parent's context alone would not reach them)
//   - Refuses any child spawned afterwards
//   - Used for user-initiated termination (e.g., "stop now")
//
// If isHardAbort is false (Graceful Finish):
//   - Only signals parentEnded for graceful child exit
//   - Children check IsParentEnded() and decide whether to continue or exit
//   - Critical SubTurns continue running and deliver orphan results
//   - Non-Critical SubTurns exit gracefully without error
//
// In both cases, the pendingResults channel is NOT closed.
// It is left open to be garbage collected when no longer used, avoiding
// "send on closed channel" panics from concurrently finishing async subturns.
func (ts *turnState) Finish(isHardAbort bool) {
	var fc chan struct{}
	var children []*turnState

	ts.mu.Lock()
	if !ts.isFinished {
		ts.isFinished = true
		if ts.finishedChan == nil {
			ts.finishedChan = make(chan struct{})
		}
		fc = ts.finishedChan
	}
	if isHardAbort && !ts.hardAborted {
		ts.hardAborted = true
		if ts.abortedChan == nil {
			ts.abortedChan = make(chan struct{})
		}
		close(ts.abortedChan) // exactly once: guarded by hardAborted under mu
		children = append(children, ts.children...)
	}
	ts.mu.Unlock()

	if isHardAbort {
		// Hard abort: cancel this turn, then every child. mu is released
		// first: each child takes its own lock, and none is held across.
		if ts.cancelFunc != nil {
			ts.cancelFunc()
		}
		for _, child := range children {
			child.Finish(true)
		}
	} else {
		// Graceful finish: signal parent ended, let children decide
		ts.parentEnded.Store(true)
	}

	// Safely close the finishedChan exactly once
	if fc != nil {
		ts.closeOnce.Do(func() {
			close(fc)
		})
	}

	// We no longer close(ts.pendingResults) here to avoid panicking any
	// concurrent deliverSubTurnResult calls. We rely on GC to clean up the channel.
}

// ====================== Ephemeral Session Store ======================

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

func (e *ephemeralSessionStore) Save(key string) error    { return nil }
func (e *ephemeralSessionStore) Close() error              { return nil }
func (e *ephemeralSessionStore) ListSessions() []string    { return nil }

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
