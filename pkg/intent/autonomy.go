package intent

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// The owner's autonomy dial: per tier, how much LESS than today the assistant may do
// on its own. It can only tighten. There is no setting that does more than today.
const (
	AutonomyDefault = "default" // today's behaviour
	AutonomyAsk     = "ask"
	AutonomyNotify  = "notify"
	AutonomyOff     = "off"
)

// AutonomyTiers are the only tiers the dial has. T0 (reads) is not dialled and T4
// (moving funds) is always denied.
var AutonomyTiers = []string{"T1", "T2", "T3"}

// AutonomyRank orders the settings by how much they restrict: default 0 < ask 1 <
// notify 2 < off 3. ok is false for any other word ("act" included).
func AutonomyRank(setting string) (rank int, ok bool) {
	switch setting {
	case AutonomyDefault:
		return 0, true
	case AutonomyAsk:
		return 1, true
	case AutonomyNotify:
		return 2, true
	case AutonomyOff:
		return 3, true
	}
	return 0, false
}

// NormalizeAutonomy validates a dial and returns its canonical form: no other tier
// than T1, T2 and T3, no other setting than the four, and "default" entries dropped
// (a missing tier is default). An empty dial is nil.
func NormalizeAutonomy(in map[string]string) (map[string]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make(map[string]string, len(in))
	for _, k := range keys {
		if !validTier(k) {
			return nil, fmt.Errorf("autonomy tier %q must be one of %s", truncateForError(k), strings.Join(AutonomyTiers, ", "))
		}
		v := in[k]
		if _, ok := AutonomyRank(v); !ok {
			return nil, fmt.Errorf("autonomy %s = %q must be default, ask, notify or off", k, truncateForError(v))
		}
		if v != AutonomyDefault {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func validTier(k string) bool {
	for _, t := range AutonomyTiers {
		if t == k {
			return true
		}
	}
	return false
}

// AutonomyOf is the setting for a tier, "default" when none is stored.
func (p Profile) AutonomyOf(tier string) string {
	if v, ok := p.Autonomy[tier]; ok {
		return v
	}
	return AutonomyDefault
}

// SetAutonomy stores a new profile version whose dial is a and whose other fields are
// the latest version's (the defaults when there is none), authored by `by`. It is the
// ONLY writer of the dial. The read of the latest version and the insert are one
// transaction, so another writer's change to any other field is kept. It validates a;
// who may call it, and the tighten-only rule, belong to the caller.
func (s *Store) SetAutonomy(ctx context.Context, a map[string]string, by string) (int64, error) {
	a, err := NormalizeAutonomy(a)
	if err != nil {
		return 0, fmt.Errorf("intent: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("intent: store autonomy: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	p, err := latestProfileTx(ctx, tx)
	if err != nil {
		return 0, err
	}
	p.Autonomy = a
	by, js, err := prepare(p, by, proactive.TrustUserAuthored)
	if err != nil {
		return 0, err
	}
	version, err := insertVersion(ctx, tx, js, by, proactive.TrustUserAuthored, s.clock())
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("intent: store autonomy: %w", err)
	}
	return version, nil
}

// latestProfileTx reads the newest stored profile inside tx, or the defaults.
func latestProfileTx(ctx context.Context, tx *sql.Tx) (Profile, error) {
	var js string
	err := tx.QueryRowContext(ctx, `SELECT profile_json FROM profile_versions ORDER BY version DESC LIMIT 1`).Scan(&js)
	if errors.Is(err, sql.ErrNoRows) {
		return DefaultProfile(), nil
	}
	if err != nil {
		return Profile{}, fmt.Errorf("intent: read profile: %w", err)
	}
	return decodeProfile(js)
}

// insertVersion inserts one profile version and prunes, inside tx.
func insertVersion(ctx context.Context, tx *sql.Tx, js, by string, trust proactive.Trust, at time.Time) (int64, error) {
	res, err := tx.ExecContext(ctx,
		`INSERT INTO profile_versions (profile_json, updated_at_ms, updated_by, trust) VALUES (?, ?, ?, ?)`,
		js, at.UnixMilli(), by, int(trust))
	if err != nil {
		return 0, fmt.Errorf("intent: store profile: %w", err)
	}
	version, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("intent: store profile: %w", err)
	}
	if err := pruneVersions(ctx, tx); err != nil {
		return 0, err
	}
	return version, nil
}
