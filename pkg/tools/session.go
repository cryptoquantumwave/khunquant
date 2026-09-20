package tools

import (
	"bytes"
	"errors"
	"io"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
)

const maxOutputBufferSize = 1 * 1024 * 1024 // 1MB

const outputTruncateMarker = "\n... [output truncated, exceeded 1MB]\n"

// PtyKeyMode represents arrow key encoding mode for PTY sessions.
// Programs send smkx/rmkx sequences to switch between CSI and SS3 modes.
type PtyKeyMode uint8

const (
	PtyKeyModeCSI PtyKeyMode = iota // triggered by rmkx (\x1b[?1l)
	PtyKeyModeSS3                   // triggered by smkx (\x1b[?1h)
)

const PtyKeyModeNotFound PtyKeyMode = 255

var (
	ErrSessionNotFound = errors.New("session not found")
	ErrSessionDone     = errors.New("session already completed")
	ErrPTYNotSupported = errors.New("PTY is not supported on this platform")
	ErrNoStdin         = errors.New("no stdin available")
)

type ProcessSession struct {
	mu              sync.Mutex
	ID              string
	PID             int
	Command         string
	PTY             bool
	Background      bool
	StartTime       int64
	ExitCode        int
	Status          string
	stdinWriter     io.Writer
	stdoutPipe      io.Reader
	outputBuffer    *bytes.Buffer
	outputTruncated bool
	ptyMaster       *os.File

	// ptyKeyMode tracks arrow key encoding mode (CSI vs SS3)
	ptyKeyMode PtyKeyMode

	// lastActivity is updated on every read, write, and poll (T1-6) and drives
	// SessionManager's idle-timeout reaping. It starts at session creation, so
	// a session nobody ever touches still times out relative to its start.
	lastActivity time.Time

	// exitReason explains an exit the session did not choose itself: an idle
	// timeout or a SessionManager.Shutdown() (T1-6). Empty for a normal
	// process exit or an explicit "kill" action.
	exitReason string
}

func (s *ProcessSession) IsDone() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Status == "done" || s.Status == "exited"
}

func (s *ProcessSession) GetPtyKeyMode() PtyKeyMode {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ptyKeyMode
}

func (s *ProcessSession) SetPtyKeyMode(mode PtyKeyMode) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ptyKeyMode = mode
}

func (s *ProcessSession) GetStatus() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Status
}

func (s *ProcessSession) SetStatus(status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Status = status
}

func (s *ProcessSession) GetExitCode() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ExitCode
}

func (s *ProcessSession) SetExitCode(code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ExitCode = code
}

// Touch records activity (a read, write, or poll) against the session. The
// SessionManager idle reaper (T1-6) kills a session once this has not been
// called for longer than the configured idle timeout.
func (s *ProcessSession) Touch() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastActivity = time.Now()
}

func (s *ProcessSession) GetLastActivity() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastActivity
}

func (s *ProcessSession) GetExitReason() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exitReason
}

// killLocked kills the process group and marks the session with the given
// status/reason. Caller must hold s.mu.
func (s *ProcessSession) killLocked(status, reason string) error {
	if s.Status != "running" {
		return ErrSessionDone
	}

	pid := s.PID
	if pid <= 0 {
		return ErrSessionNotFound
	}

	if err := killProcessGroup(pid); err != nil {
		return err
	}

	s.Status = status
	s.ExitCode = -1
	s.exitReason = reason
	return nil
}

// Kill terminates the session's process in response to an explicit "kill"
// action from the caller that owns it.
func (s *ProcessSession) Kill() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.killLocked("done", "")
}

// KillWithReason force-terminates the session for a reason the session did
// not choose itself (SessionManager idle timeout or Shutdown, T1-6), marking
// it "exited" rather than "done" so a subsequent poll can tell the two apart
// and surface why the process is no longer running.
func (s *ProcessSession) KillWithReason(reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.killLocked("exited", reason)
}

func (s *ProcessSession) Write(data string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.Status != "running" {
		return ErrSessionDone
	}

	var writer io.Writer
	if s.PTY && s.ptyMaster != nil {
		writer = s.ptyMaster
	} else if s.stdinWriter != nil {
		writer = s.stdinWriter
	} else {
		return ErrNoStdin
	}

	_, err := writer.Write([]byte(data))
	return err
}

func (s *ProcessSession) Read() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.outputBuffer.Len() == 0 {
		return ""
	}

	data := s.outputBuffer.String()
	s.outputBuffer.Reset()
	return data
}

func (s *ProcessSession) ToSessionInfo() SessionInfo {
	s.mu.Lock()
	defer s.mu.Unlock()

	return SessionInfo{
		ID:        s.ID,
		Command:   s.Command,
		Status:    s.Status,
		PID:       s.PID,
		StartedAt: s.StartTime,
	}
}

// defaultSessionIdleTimeout (T1-6) is how long a background/PTY session may
// go without a read, write, or poll before SessionManager kills it. Sessions
// otherwise had unbounded lifetime: nothing tied them to the per-call ctx of
// the "run" action that started them (deliberately - a background job must
// outlive that call), and nothing else ever stopped them either.
const defaultSessionIdleTimeout = 30 * time.Minute

// minIdleReapInterval floors how often the reap loop wakes up, so a very
// short idle timeout (as set in tests) doesn't busy-loop.
const minIdleReapInterval = 10 * time.Millisecond

// maxIdleReapInterval caps the reap loop's wake-up interval so a long or
// disabled idle timeout still lets cleanupOldSessions run roughly every 5
// minutes, matching this type's behavior before T1-6.
const maxIdleReapInterval = 5 * time.Minute

type SessionManager struct {
	mu          sync.RWMutex
	sessions    map[string]*ProcessSession
	idleTimeout time.Duration
	stopCh      chan struct{}
	stopOnce    sync.Once
	// wakeCh interrupts the reap loop's sleep when the idle timeout changes.
	// Without it the loop keeps sleeping on the interval it computed from the
	// *previous* timeout - up to 5 minutes - so a shortened timeout would not
	// be enforced until that sleep expired.
	wakeCh chan struct{}
}

func NewSessionManager() *SessionManager {
	sm := &SessionManager{
		sessions:    make(map[string]*ProcessSession),
		idleTimeout: defaultSessionIdleTimeout,
		stopCh:      make(chan struct{}),
		wakeCh:      make(chan struct{}, 1),
	}

	go sm.reapLoop()

	return sm
}

// SetIdleTimeout configures how long a session may go without a read, write,
// or poll before the manager kills it (T1-6). A value <= 0 disables idle
// reaping; cleanupOldSessions still runs periodically to garbage-collect
// sessions that finished on their own.
func (sm *SessionManager) SetIdleTimeout(d time.Duration) {
	sm.mu.Lock()
	sm.idleTimeout = d
	sm.mu.Unlock()

	// Wake the reap loop so it recomputes its interval against the new value
	// rather than finishing a sleep sized by the old one. Non-blocking: a wake
	// already queued achieves the same thing.
	select {
	case sm.wakeCh <- struct{}{}:
	default:
	}
}

// Shutdown kills every session this manager owns and stops its background
// reap loop. Safe to call more than once and safe to call with no sessions.
func (sm *SessionManager) Shutdown() {
	sm.stopOnce.Do(func() {
		close(sm.stopCh)
	})

	sm.mu.RLock()
	sessions := make([]*ProcessSession, 0, len(sm.sessions))
	for _, session := range sm.sessions {
		sessions = append(sessions, session)
	}
	sm.mu.RUnlock()

	for _, session := range sessions {
		if !session.IsDone() {
			_ = session.KillWithReason("session manager shutdown")
		}
	}
}

// reapLoop periodically kills sessions idle longer than idleTimeout and
// garbage-collects sessions that finished (by any means) more than 30
// minutes ago. It self-adjusts its wake-up interval to the currently
// configured idle timeout so a short timeout (as set in tests) is enforced
// promptly, and exits once Shutdown closes stopCh.
func (sm *SessionManager) reapLoop() {
	lastCleanup := time.Now()
	for {
		sm.mu.RLock()
		timeout := sm.idleTimeout
		sm.mu.RUnlock()

		interval := timeout / 10
		if interval <= 0 || interval > maxIdleReapInterval {
			interval = maxIdleReapInterval
		}
		if interval < minIdleReapInterval {
			interval = minIdleReapInterval
		}

		timer := time.NewTimer(interval)
		select {
		case <-timer.C:
			sm.reapIdleSessions()
			if time.Since(lastCleanup) >= maxIdleReapInterval {
				sm.cleanupOldSessions()
				lastCleanup = time.Now()
			}
		case <-sm.wakeCh:
			// The idle timeout changed; recompute the interval from it.
			timer.Stop()
		case <-sm.stopCh:
			timer.Stop()
			return
		}
	}
}

// reapIdleSessions kills any running session whose last read/write/poll was
// longer than idleTimeout ago.
func (sm *SessionManager) reapIdleSessions() {
	sm.mu.RLock()
	timeout := sm.idleTimeout
	sessions := make([]*ProcessSession, 0, len(sm.sessions))
	for _, session := range sm.sessions {
		sessions = append(sessions, session)
	}
	sm.mu.RUnlock()

	if timeout <= 0 {
		return
	}

	cutoff := time.Now().Add(-timeout)
	for _, session := range sessions {
		if session.IsDone() {
			continue
		}
		if session.GetLastActivity().Before(cutoff) {
			_ = session.KillWithReason("idle timeout after " + timeout.String())
		}
	}
}

// cleanupOldSessions removes sessions that are done and older than 30 minutes
func (sm *SessionManager) cleanupOldSessions() {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	cutoff := time.Now().Add(-30 * time.Minute)
	for id, session := range sm.sessions {
		if session.IsDone() && session.StartTime < cutoff.Unix() {
			delete(sm.sessions, id)
		}
	}
}

func (sm *SessionManager) Add(session *ProcessSession) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.sessions[session.ID] = session
}

func (sm *SessionManager) Get(sessionID string) (*ProcessSession, error) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	session, ok := sm.sessions[sessionID]
	if !ok {
		return nil, ErrSessionNotFound
	}

	return session, nil
}

func (sm *SessionManager) Remove(sessionID string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	delete(sm.sessions, sessionID)
}

func (sm *SessionManager) List() []SessionInfo {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	result := make([]SessionInfo, 0, len(sm.sessions))
	for _, session := range sm.sessions {
		result = append(result, session.ToSessionInfo())
	}

	return result
}

func generateSessionID() string {
	return uuid.New().String()[:8]
}

type SessionInfo struct {
	ID        string `json:"id"`
	Command   string `json:"command"`
	Status    string `json:"status"`
	PID       int    `json:"pid"`
	StartedAt int64  `json:"startedAt"`
}
