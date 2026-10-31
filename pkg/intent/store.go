package intent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex"
)

const (
	// Component is the sqlitex migration component of this package.
	Component = "intent"

	// MaxProfileVersions is how many profile versions are kept; the oldest are
	// pruned in the same transaction that inserts a new one.
	MaxProfileVersions = 200

	// ImportedBy is the updated_by of a profile imported from USER.md.
	ImportedBy = "import:USER.md"

	maxUpdatedByRunes = 128
)

var migrations = []sqlitex.Migration{
	{
		Component: Component,
		Version:   1,
		SQL: `CREATE TABLE profile_versions (
	version       INTEGER PRIMARY KEY AUTOINCREMENT,
	profile_json  TEXT    NOT NULL,
	updated_at_ms INTEGER NOT NULL,
	updated_by    TEXT    NOT NULL,
	trust         INTEGER NOT NULL
)`,
	},
	{
		// The intent ledger. Timestamps are unix milliseconds; JSON columns hold ''
		// when the field is absent. Rows are never deleted.
		Component: Component,
		Version:   2,
		SQL: `CREATE TABLE intents (
	id              TEXT    PRIMARY KEY,
	type            TEXT    NOT NULL,
	title           TEXT    NOT NULL,
	params_json     TEXT    NOT NULL DEFAULT '',
	trigger_json    TEXT    NOT NULL DEFAULT '',
	expected_json   TEXT    NOT NULL DEFAULT '',
	mandate_id      TEXT    NOT NULL DEFAULT '',
	status          TEXT    NOT NULL,
	source          TEXT    NOT NULL,
	provenance_json TEXT    NOT NULL,
	fire_budget     INTEGER NOT NULL DEFAULT 0,
	fires_used      INTEGER NOT NULL DEFAULT 0,
	cooldown_ms     INTEGER NOT NULL DEFAULT 0,
	lifetime_ms     INTEGER NOT NULL DEFAULT 0,
	expires_at_ms   INTEGER,
	valid_from_ms   INTEGER NOT NULL,
	invalid_at_ms   INTEGER,
	superseded_by   TEXT    NOT NULL DEFAULT '',
	created_at_ms   INTEGER NOT NULL,
	updated_at_ms   INTEGER NOT NULL
);
CREATE INDEX idx_intents_status ON intents (status, created_at_ms);
CREATE TABLE intent_events (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	intent_id   TEXT    NOT NULL,
	at_ms       INTEGER NOT NULL,
	from_status TEXT    NOT NULL,
	to_status   TEXT    NOT NULL,
	by_sender   TEXT    NOT NULL,
	reason      TEXT    NOT NULL
);
CREATE INDEX idx_intent_events_intent ON intent_events (intent_id, id)`,
	},
}

// ProfileVersion is one stored version of the profile. Provenance is the trust
// of whoever authored it.
type ProfileVersion struct {
	Version    int64
	Profile    Profile
	UpdatedAt  time.Time
	UpdatedBy  string
	Provenance proactive.Trust
}

// Store keeps the owner's profile and the intent ledger in the shared proactive database. It does not
// own db: close it through sqlitex.CloseShared, never through the Store.
type Store struct {
	db  *sql.DB
	now func() time.Time // nil = time.Now; SetClock sets it
}

// NewStore migrates the intent tables on db and returns a Store over it. It
// does not import USER.md; see ImportIfEmpty.
func NewStore(ctx context.Context, db *sql.DB) (*Store, error) {
	if db == nil {
		return nil, errors.New("intent: nil database")
	}
	if err := sqlitex.Migrate(ctx, db, migrations...); err != nil {
		return nil, fmt.Errorf("intent: %w", err)
	}
	return &Store{db: db}, nil
}

// wireProfile is the stored JSON. It differs from Profile in one field: the
// budget is a pointer so a stored 0 ("nothing non-critical") can be told from
// a row that never said, which reads as the default.
type wireProfile struct {
	Timezone                 string             `json:"timezone,omitempty"`
	ActiveHours              HoursRange         `json:"active_hours"`
	Language                 string             `json:"language,omitempty"`
	RiskTolerance            string             `json:"risk_tolerance,omitempty"`
	Horizon                  string             `json:"horizon,omitempty"`
	PrimaryGoal              string             `json:"primary_goal,omitempty"`
	AllocationTargets        map[string]float64 `json:"allocation_targets,omitempty"`
	AssetsToAvoid            []string           `json:"assets_to_avoid,omitempty"`
	NotificationBudgetPerDay *int               `json:"notification_budget_per_day,omitempty"`
	DigestTime               string             `json:"digest_time,omitempty"`
	Autonomy                 map[string]string  `json:"autonomy,omitempty"`
}

func toWire(p Profile) wireProfile {
	b := p.NotificationBudgetPerDay
	return wireProfile{
		Timezone:                 p.Timezone,
		ActiveHours:              p.ActiveHours,
		Language:                 p.Language,
		RiskTolerance:            p.RiskTolerance,
		Horizon:                  p.Horizon,
		PrimaryGoal:              p.PrimaryGoal,
		AllocationTargets:        p.AllocationTargets,
		AssetsToAvoid:            p.AssetsToAvoid,
		NotificationBudgetPerDay: &b,
		DigestTime:               p.DigestTime,
		Autonomy:                 p.Autonomy,
	}
}

func (w wireProfile) profile() Profile {
	p := Profile{
		Timezone:          w.Timezone,
		ActiveHours:       w.ActiveHours,
		Language:          w.Language,
		RiskTolerance:     w.RiskTolerance,
		Horizon:           w.Horizon,
		PrimaryGoal:       w.PrimaryGoal,
		AllocationTargets: w.AllocationTargets,
		AssetsToAvoid:     w.AssetsToAvoid,
		DigestTime:        w.DigestTime,
	}
	// A stored dial that does not validate (a hand-edited row) reads as the
	// strictest setting for every tier, never as default: an unreadable restriction
	// must not become no restriction.
	if a, err := NormalizeAutonomy(w.Autonomy); err != nil {
		p.Autonomy = map[string]string{}
		for _, t := range AutonomyTiers {
			p.Autonomy[t] = AutonomyOff
		}
	} else {
		p.Autonomy = a
	}
	p.NotificationBudgetPerDay = DefaultBudgetPerDay
	if w.NotificationBudgetPerDay != nil {
		p.NotificationBudgetPerDay = *w.NotificationBudgetPerDay
	}
	return p.resolve()
}

func decodeProfile(data string) (Profile, error) {
	var w wireProfile
	if err := json.Unmarshal([]byte(data), &w); err != nil {
		return Profile{}, fmt.Errorf("intent: decode stored profile: %w", err)
	}
	return w.profile(), nil
}

// Latest returns the current version of the profile, or ok=false when none has
// been stored yet.
func (s *Store) Latest(ctx context.Context) (v ProfileVersion, ok bool, err error) {
	rows, err := s.queryVersions(ctx, 1)
	if err != nil {
		return ProfileVersion{}, false, err
	}
	if len(rows) == 0 {
		return ProfileVersion{}, false, nil
	}
	return rows[0], true, nil
}

// Profile returns the current profile, or the defaults when none has been
// stored. Unset fields of a stored profile read as their defaults; see the
// package comment for which fields those are.
func (s *Store) Profile(ctx context.Context) (Profile, error) {
	v, ok, err := s.Latest(ctx)
	if err != nil {
		return Profile{}, err
	}
	if !ok {
		return DefaultProfile(), nil
	}
	return v.Profile, nil
}

// HasProfile reports whether at least one version has been stored.
func (s *Store) HasProfile(ctx context.Context) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM profile_versions LIMIT 1`).Scan(&one)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("intent: check for a profile: %w", err)
	}
	return true, nil
}

// ProfileHistory returns stored versions, newest first. A limit outside
// 1..MaxProfileVersions means "as many as are kept".
func (s *Store) ProfileHistory(ctx context.Context, limit int) ([]ProfileVersion, error) {
	if limit < 1 || limit > MaxProfileVersions {
		limit = MaxProfileVersions
	}
	return s.queryVersions(ctx, limit)
}

// queryVersions reads the newest limit rows fully and closes the cursor before
// returning, so no row set stays open while the caller acts.
func (s *Store) queryVersions(ctx context.Context, limit int) ([]ProfileVersion, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT version, profile_json, updated_at_ms, updated_by, trust
		   FROM profile_versions ORDER BY version DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("intent: read profile versions: %w", err)
	}
	defer rows.Close()

	var out []ProfileVersion
	for rows.Next() {
		var (
			v     ProfileVersion
			js    string
			at    int64
			trust int
		)
		if err := rows.Scan(&v.Version, &js, &at, &v.UpdatedBy, &trust); err != nil {
			return nil, fmt.Errorf("intent: read profile versions: %w", err)
		}
		p, err := decodeProfile(js)
		if err != nil {
			return nil, fmt.Errorf("intent: profile version %d: %w", v.Version, err)
		}
		v.Profile = p
		v.UpdatedAt = time.UnixMilli(at).UTC()
		v.Provenance = proactive.Trust(trust)
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("intent: read profile versions: %w", err)
	}
	return out, nil
}

// PutProfile validates and normalises p, stores it as a new version authored by
// by with the given trust, and returns the new version number. p is taken as a
// complete profile (see the package comment on zero versus unset). Versions
// beyond MaxProfileVersions are pruned oldest-first in the same transaction.
//
// p.Autonomy is IGNORED: the stored version's dial is carried over, read in the same
// transaction as the insert. So a writer that read the profile earlier (the model's
// update_profile, the launcher's profile PUT) can never revert the owner's dial by
// writing back the value it read. SetAutonomy is the only way to change it.
func (s *Store) PutProfile(ctx context.Context, p Profile, by string, trust proactive.Trust) (int64, error) {
	if _, _, err := prepare(p, by, trust); err != nil {
		return 0, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("intent: store profile: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	cur, err := latestProfileTx(ctx, tx)
	if err != nil {
		return 0, err
	}
	p.Autonomy = cur.Autonomy
	by, js, err := prepare(p, by, trust)
	if err != nil {
		return 0, err
	}
	version, err := insertVersion(ctx, tx, js, by, trust, s.clock())
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("intent: store profile: %w", err)
	}
	return version, nil
}

// prepare validates the inputs of a write and returns the cleaned author and
// the stored JSON of the normalised profile.
func prepare(p Profile, by string, trust proactive.Trust) (cleanBy, profileJSON string, err error) {
	np, err := p.Normalize()
	if err != nil {
		return "", "", fmt.Errorf("intent: invalid profile: %w", err)
	}
	by = cleanAuthor(by)
	if by == "" {
		return "", "", errors.New("intent: a profile change needs an author (updated_by)")
	}
	if trust < proactive.TrustUntrusted || trust > proactive.TrustUserAuthored {
		return "", "", fmt.Errorf("intent: unknown trust level %d", int(trust))
	}
	data, err := json.Marshal(toWire(np))
	if err != nil {
		return "", "", fmt.Errorf("intent: encode profile: %w", err)
	}
	return by, string(data), nil
}

// cleanAuthor keeps updated_by to one short printable line.
func cleanAuthor(by string) string {
	by = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return ' '
		}
		return r
	}, by)
	by = strings.Join(strings.Fields(by), " ")
	if utf8.RuneCountInString(by) > maxUpdatedByRunes {
		by = string([]rune(by)[:maxUpdatedByRunes])
	}
	return by
}

// pruneVersions deletes everything but the newest MaxProfileVersions rows.
func pruneVersions(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx,
		`DELETE FROM profile_versions WHERE version NOT IN
		   (SELECT version FROM profile_versions ORDER BY version DESC LIMIT ?)`, MaxProfileVersions)
	if err != nil {
		return fmt.Errorf("intent: prune profile versions: %w", err)
	}
	return nil
}

// ImportIfEmpty seeds the profile from the USER.md at userMDPath, once: it does
// nothing when any version exists, when the file is missing, or when the file
// holds nothing usable (no rows are created then). The seed is stored with
// by=ImportedBy and TrustUserAuthored, since USER.md is the owner's own file.
// The check and the insert are one statement, so two processes starting at once
// import at most once.
func (s *Store) ImportIfEmpty(ctx context.Context, userMDPath string) (imported bool, err error) {
	has, err := s.HasProfile(ctx)
	if err != nil {
		return false, err
	}
	if has {
		return false, nil
	}
	p, found, err := ImportFromUserMD(userMDPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if !found {
		return false, nil
	}
	by, js, err := prepare(p, ImportedBy, proactive.TrustUserAuthored)
	if err != nil {
		return false, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("intent: import USER.md: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx,
		`INSERT INTO profile_versions (profile_json, updated_at_ms, updated_by, trust)
		 SELECT ?, ?, ?, ? WHERE NOT EXISTS (SELECT 1 FROM profile_versions)`,
		js, s.clock().UnixMilli(), by, int(proactive.TrustUserAuthored))
	if err != nil {
		return false, fmt.Errorf("intent: import USER.md: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("intent: import USER.md: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("intent: import USER.md: %w", err)
	}
	if n == 1 {
		logger.InfoCF("intent", "Imported structured profile from USER.md", map[string]any{"path": userMDPath})
	}
	return n == 1, nil
}
