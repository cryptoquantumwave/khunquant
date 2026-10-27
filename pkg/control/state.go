package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex"
)

// Component is the sqlitex migration component of this package.
const Component = "control"

// KillKey is the control_state row that holds the kill switch.
const KillKey = "kill_engaged_at"

const stateTimeout = 5 * time.Second

// DefaultRefresh is how often Killed re-reads the table at most. It is the bound
// on how long a second process (the CLI, the web launcher's gateway route) takes
// to see a flag that this one set, and the other way round.
const DefaultRefresh = 2 * time.Second

var migrations = []sqlitex.Migration{{
	Component: Component,
	Version:   1,
	// value_json of kill_engaged_at is the JSON null while the switch is off, and
	// {"at_ms":…,"by":…,"reason":…} while it is engaged. The row is seeded so that
	// engaging and clearing are each one conditional UPDATE.
	SQL: `CREATE TABLE control_state (
	key        TEXT PRIMARY KEY,
	value_json TEXT NOT NULL
);
INSERT INTO control_state (key, value_json) VALUES ('kill_engaged_at', 'null');`,
}}

// KillInfo describes an engaged kill switch.
type KillInfo struct {
	// At is when it was engaged.
	At time.Time
	// By is who engaged it (a canonical sender id, or "api").
	By     string
	Reason string
}

type killRecord struct {
	AtMs   int64  `json:"at_ms"`
	By     string `json:"by"`
	Reason string `json:"reason,omitempty"`
}

// State is the persisted control state: today, the kill switch.
//
// The switch is stored in the shared proactive database, so it survives a restart
// and is seen by every process that opens the file. Killed is cheap: it answers
// from memory and re-reads the table at most every Refresh. Engage and Clear
// update the memory of this process at once.
type State struct {
	db      *sql.DB
	now     func() time.Time
	refresh time.Duration

	mu        sync.Mutex
	killed    bool
	info      KillInfo
	checkedAt time.Time
	loaded    bool
	// degraded: the last read failed. Killed answers true until a read succeeds.
	degraded bool
}

// StateOption customises NewState.
type StateOption func(*State)

// WithStateClock sets the clock Killed's refresh interval runs on (tests).
func WithStateClock(now func() time.Time) StateOption { return func(s *State) { s.now = now } }

// WithRefresh sets how often Killed re-reads the table at most.
func WithRefresh(d time.Duration) StateOption { return func(s *State) { s.refresh = d } }

// NewState migrates the control_state table in db and loads the switch.
func NewState(ctx context.Context, db *sql.DB, opts ...StateOption) (*State, error) {
	if db == nil {
		return nil, errors.New("control: nil database")
	}
	if err := sqlitex.Migrate(ctx, db, migrations...); err != nil {
		return nil, fmt.Errorf("control: migrate: %w", err)
	}
	//simclock:wall the default when no clock is injected
	s := &State{db: db, now: time.Now, refresh: DefaultRefresh}
	for _, o := range opts {
		o(s)
	}
	if s.refresh <= 0 {
		s.refresh = DefaultRefresh
	}
	if _, _, err := s.read(ctx); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.loaded = true
	s.mu.Unlock()
	return s, nil
}

// read loads the row and stores it in memory.
func (s *State) read(ctx context.Context) (bool, KillInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, stateTimeout)
	defer cancel()
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT value_json FROM control_state WHERE key = ?`, KillKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		raw = "null"
	} else if err != nil {
		s.mu.Lock()
		started := !s.degraded
		s.degraded = true
		s.mu.Unlock()
		if started {
			logger.WarnCF("control", "Could not read the kill switch; treating it as engaged until a read succeeds",
				map[string]any{"error": err.Error()})
		}
		return false, KillInfo{}, fmt.Errorf("control: read kill switch: %w", err)
	}
	killed, info, err := decodeKill(raw)
	if err != nil {
		// A row nobody can read is treated as engaged: the safe side to err on.
		logger.ErrorCF("control", "The kill switch row is unreadable; treating it as engaged",
			map[string]any{"error": err.Error()})
		killed, info = true, KillInfo{By: "unreadable", Reason: "unreadable kill switch row"}
	}
	s.mu.Lock()
	recovered := s.degraded
	s.degraded = false
	s.killed, s.info, s.checkedAt = killed, info, s.now()
	s.mu.Unlock()
	if recovered {
		logger.InfoCF("control", "The kill switch can be read again", nil)
	}
	return killed, info, nil
}

func decodeKill(raw string) (bool, KillInfo, error) {
	if raw == "" || raw == "null" {
		return false, KillInfo{}, nil
	}
	var r killRecord
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return false, KillInfo{}, err
	}
	return true, KillInfo{At: time.UnixMilli(r.AtMs).UTC(), By: r.By, Reason: r.Reason}, nil
}

// Killed reports whether the kill switch is engaged. It is what every executor
// that can act on its own asks (it satisfies approval.Killer).
//
// It fails closed: when the table cannot be read, the answer is true until a later
// read succeeds, so a database error refuses acts for at most one refresh interval
// instead of leaving the switch looking off.
func (s *State) Killed() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	due := !s.loaded || s.now().Sub(s.checkedAt) >= s.refresh
	if !due {
		k := s.killed || s.degraded
		s.mu.Unlock()
		return k
	}
	// Claim this refresh so that concurrent callers answer from memory meanwhile.
	s.checkedAt = s.now()
	s.mu.Unlock()

	killed, _, err := s.read(context.Background())
	if err != nil {
		return true // read has recorded the failure and logged it once
	}
	return killed
}

// Info returns the engaged switch's details; ok is false while it is off. It
// reads the table.
func (s *State) Info(ctx context.Context) (KillInfo, bool, error) {
	killed, info, err := s.read(ctx)
	return info, killed, err
}

// Engage engages the kill switch on behalf of by. changed is false when it was
// engaged already (the original time and author are kept).
func (s *State) Engage(ctx context.Context, by, reason string) (changed bool, err error) {
	b, err := json.Marshal(killRecord{AtMs: s.now().UnixMilli(), By: by, Reason: reason})
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(ctx, stateTimeout)
	defer cancel()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO control_state (key, value_json) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value_json = excluded.value_json WHERE control_state.value_json = 'null'`,
		KillKey, string(b))
	if err != nil {
		return false, fmt.Errorf("control: engage kill switch: %w", err)
	}
	n, _ := res.RowsAffected()
	if _, _, rerr := s.read(ctx); rerr != nil {
		// Written, but this process could not reload it: say engaged.
		s.mu.Lock()
		s.killed, s.checkedAt = true, s.now()
		s.mu.Unlock()
	}
	return n > 0, nil
}

// Clear clears the kill switch: the row goes back to null. changed is false when
// it was not engaged. The second argument is who asked, kept so that a later
// change can record it.
func (s *State) Clear(ctx context.Context, _ string) (changed bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, stateTimeout)
	defer cancel()
	res, err := s.db.ExecContext(ctx,
		`UPDATE control_state SET value_json = 'null' WHERE key = ? AND value_json <> 'null'`, KillKey)
	if err != nil {
		return false, fmt.Errorf("control: clear kill switch: %w", err)
	}
	n, _ := res.RowsAffected()
	if _, _, rerr := s.read(ctx); rerr != nil {
		s.mu.Lock()
		s.killed, s.checkedAt = false, s.now()
		s.mu.Unlock()
	}
	if n > 0 {
		logger.InfoCF("control", "Kill switch cleared", nil)
	}
	return n > 0, nil
}
