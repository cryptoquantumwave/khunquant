package agent

import (
	"context"
	"sync"
	"sync/atomic"
)

// sessionLocks serialises work on a session: at most one holder per session
// key, any number of sessions in parallel.
//
// It exists because turns no longer run one at a time. A heartbeat, a cron job
// and a user message can each start a turn on its own goroutine, and two turns
// on one session would interleave their writes to the session history (a tool
// result landing between another turn's tool call and its result is enough to
// make the provider reject the history).
//
// Who takes it:
//   - every root turn, in runAgentLoop, for the whole turn;
//   - non-turn writers, through AgentLoop.WithSessionLock / WithSessionLockContext.
//
// Sub-turns do not: they run on their own ephemeral store under their own key
// and never touch the parent's session.
//
// Properties callers can rely on:
//   - Acquisition honours ctx. A caller cancelled while waiting returns
//     ctx.Err() and does not hold the lock.
//   - It is re-entrant along a context chain. The context returned by lock
//     records the key; a later lock on a context derived from it, for the same
//     key, succeeds immediately. So work that already runs under a session's
//     lock can call something that also takes it without deadlocking on itself.
//   - Entries are refcounted and removed when unused, so the map holds only the
//     sessions that are busy right now.
//
// What it does not do: order two different keys. Code that holds one session's
// lock and then waits for another's can deadlock against code doing the
// reverse. Nothing does that today; do not start.
//
// Nothing else is held while waiting: mu guards the map only and is never held
// across the blocking acquire.
type sessionLocks struct {
	mu      sync.Mutex
	entries map[string]*sessionLockEntry
}

type sessionLockEntry struct {
	// sem has capacity 1: the goroutine whose token is in it holds the lock.
	// A channel rather than a sync.Mutex so that waiting can be abandoned
	// when the context is cancelled.
	sem chan struct{}
	// refs counts the holder and every waiter. Guarded by sessionLocks.mu.
	refs int
}

// heldSessionLock is one link in the chain of session locks a context holds.
type heldSessionLock struct {
	key string
	// released is set when the lock is given back. The context outlives the
	// lock (an async subagent keeps the context of the turn that spawned it),
	// and a stale context must not be mistaken for a holder.
	released atomic.Bool
	parent   *heldSessionLock
}

type heldSessionLockKeyType struct{}

var heldSessionLockKey = heldSessionLockKeyType{}

func heldSessionLocksFromContext(ctx context.Context) *heldSessionLock {
	h, _ := ctx.Value(heldSessionLockKey).(*heldSessionLock)
	return h
}

// holdsSessionLock reports whether ctx descends from a context that acquired
// the lock for key and has not released it yet.
func holdsSessionLock(ctx context.Context, key string) bool {
	for h := heldSessionLocksFromContext(ctx); h != nil; h = h.parent {
		if h.key == key && !h.released.Load() {
			return true
		}
	}
	return false
}

// inheritHeldSessionLocks returns ctx carrying the session locks that from
// holds. It is for a context deliberately built from context.Background, which
// drops the chain, when the work it runs is still synchronous with the holder.
func inheritHeldSessionLocks(ctx, from context.Context) context.Context {
	h := heldSessionLocksFromContext(from)
	if h == nil {
		return ctx
	}
	return context.WithValue(ctx, heldSessionLockKey, h)
}

// lock acquires the lock for key, waiting until it is free or ctx is done.
//
// On success it returns a context that records the lock as held, and an unlock
// function that is safe to call more than once. Pass the returned context to
// whatever runs under the lock, so that nested acquisitions are recognised.
// If ctx already holds the lock the call returns at once and its unlock is a
// no-op: the outer holder releases.
//
// On failure it returns ctx.Err(); the lock is not held and there is nothing
// to unlock.
func (l *sessionLocks) lock(ctx context.Context, key string) (context.Context, func(), error) {
	if holdsSessionLock(ctx, key) {
		return ctx, func() {}, nil
	}
	if err := ctx.Err(); err != nil {
		return ctx, nil, err
	}

	l.mu.Lock()
	if l.entries == nil {
		l.entries = make(map[string]*sessionLockEntry)
	}
	e, ok := l.entries[key]
	if !ok {
		e = &sessionLockEntry{sem: make(chan struct{}, 1)}
		l.entries[key] = e
	}
	e.refs++
	l.mu.Unlock()

	select {
	case e.sem <- struct{}{}:
	case <-ctx.Done():
		l.release(key, e, false)
		return ctx, nil, ctx.Err()
	}
	// select picks at random when both cases are ready. A caller that was
	// cancelled while it waited must not come away holding the lock.
	if err := ctx.Err(); err != nil {
		l.release(key, e, true)
		return ctx, nil, err
	}

	held := &heldSessionLock{key: key, parent: heldSessionLocksFromContext(ctx)}
	var once sync.Once
	unlock := func() {
		once.Do(func() {
			held.released.Store(true)
			l.release(key, e, true)
		})
	}
	return context.WithValue(ctx, heldSessionLockKey, held), unlock, nil
}

// release drops one reference to e, first giving the lock back if the caller
// acquired it, and removes the entry once nobody holds or waits for it.
func (l *sessionLocks) release(key string, e *sessionLockEntry, acquired bool) {
	if acquired {
		<-e.sem
	}
	l.mu.Lock()
	e.refs--
	if e.refs == 0 {
		delete(l.entries, key)
	}
	l.mu.Unlock()
}

// size returns the number of sessions that currently have a holder or a waiter.
func (l *sessionLocks) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}

// WithSessionLock runs fn while holding the session's lock, so that fn does
// not interleave with a turn (or another WithSessionLock caller) on the same
// session. It is for writers that are not turns, such as appending a proactive
// message to a session's history.
//
// It waits as long as it takes and is NOT re-entrant, because it has no
// context to recognise a holder by: calling it from inside a turn on the same
// session - from a tool, say - deadlocks. Code that runs inside a turn, or
// that must be cancellable, uses WithSessionLockContext.
func (al *AgentLoop) WithSessionLock(sessionKey string, fn func() error) error {
	return al.WithSessionLockContext(context.Background(), sessionKey, func(context.Context) error {
		return fn()
	})
}

// WithSessionLockContext is WithSessionLock with a context: waiting for the
// lock stops when ctx is done (returning ctx.Err() without running fn), and a
// ctx that already holds this session's lock - a tool's context inside a turn
// on that session - runs fn immediately instead of deadlocking.
//
// fn receives a context that records the lock as held; pass it on to anything
// that may take the same lock again.
func (al *AgentLoop) WithSessionLockContext(
	ctx context.Context,
	sessionKey string,
	fn func(ctx context.Context) error,
) error {
	lockedCtx, unlock, err := al.sessionLocks.lock(ctx, sessionKey)
	if err != nil {
		return err
	}
	defer unlock()
	return fn(lockedCtx)
}
