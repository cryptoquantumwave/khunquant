package snapshot

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS snapshots (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    taken_at    TEXT    NOT NULL,
    quote       TEXT    NOT NULL,
    total_value REAL    NOT NULL,
    label       TEXT    NOT NULL DEFAULT '',
    note        TEXT    NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS positions (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    snapshot_id INTEGER NOT NULL REFERENCES snapshots(id) ON DELETE CASCADE,
    source      TEXT    NOT NULL,
    account     TEXT    NOT NULL DEFAULT '',
    category    TEXT    NOT NULL DEFAULT '',
    asset       TEXT    NOT NULL,
    quantity    REAL    NOT NULL,
    price       REAL    NOT NULL DEFAULT 0,
    value       REAL    NOT NULL DEFAULT 0,
    meta        TEXT    NOT NULL DEFAULT '{}'
);

CREATE INDEX IF NOT EXISTS idx_snapshots_taken_at ON snapshots(taken_at);
CREATE INDEX IF NOT EXISTS idx_snapshots_label ON snapshots(label);
CREATE INDEX IF NOT EXISTS idx_positions_snapshot_id ON positions(snapshot_id);
CREATE INDEX IF NOT EXISTS idx_positions_source ON positions(source);
CREATE INDEX IF NOT EXISTS idx_positions_asset ON positions(asset);
`

// Store persists portfolio snapshots in SQLite.
type Store struct {
	db *sql.DB
}

// NewStore opens (or creates) the snapshots database under
// {workspacePath}/memory/snapshots/snapshots.db.
func NewStore(workspacePath string) (*Store, error) {
	dir := filepath.Join(workspacePath, "memory", "snapshots")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("snapshot: create dir: %w", err)
	}
	dbPath := filepath.Join(dir, "snapshots.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("snapshot: open db: %w", err)
	}

	// SQLite pragmas for performance and correctness.
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA foreign_keys=ON",
		"PRAGMA cache_size=-2000",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("snapshot: %s: %w", pragma, err)
		}
	}

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("snapshot: create schema: %w", err)
	}

	return &Store{db: db}, nil
}

// Close releases the database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// SaveSnapshot persists a snapshot and its positions. It sets snap.ID on success.
func (s *Store) SaveSnapshot(ctx context.Context, snap *Snapshot) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx,
		`INSERT INTO snapshots (taken_at, quote, total_value, label, note) VALUES (?, ?, ?, ?, ?)`,
		snap.TakenAt.Format(time.RFC3339), snap.Quote, snap.TotalValue, snap.Label, snap.Note,
	)
	if err != nil {
		return 0, fmt.Errorf("snapshot: insert snapshot: %w", err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	snap.ID = id

	for i := range snap.Positions {
		p := &snap.Positions[i]
		p.SnapshotID = id

		meta := "{}"
		if len(p.Meta) > 0 {
			b, _ := json.Marshal(p.Meta)
			meta = string(b)
		}

		_, err := tx.ExecContext(ctx,
			`INSERT INTO positions (snapshot_id, source, account, category, asset, quantity, price, value, meta)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, p.Source, p.Account, p.Category, p.Asset, p.Quantity, p.Price, p.Value, meta,
		)
		if err != nil {
			return 0, fmt.Errorf("snapshot: insert position %s/%s: %w", p.Source, p.Asset, err)
		}
	}

	return id, tx.Commit()
}

// GetSnapshot retrieves a single snapshot by ID, including positions.
func (s *Store) GetSnapshot(ctx context.Context, id int64) (*Snapshot, error) {
	snap := &Snapshot{}
	var takenAt string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, taken_at, quote, total_value, label, note FROM snapshots WHERE id = ?`, id,
	).Scan(&snap.ID, &takenAt, &snap.Quote, &snap.TotalValue, &snap.Label, &snap.Note)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("snapshot %d not found", id)
		}
		return nil, err
	}
	snap.TakenAt, _ = time.Parse(time.RFC3339, takenAt)

	positions, err := s.loadPositions(ctx, id)
	if err != nil {
		return nil, err
	}
	snap.Positions = positions
	return snap, nil
}

// QuerySnapshots returns snapshots matching the filter. Positions are loaded
// only if the caller needs them (via GetSnapshot).
func (s *Store) QuerySnapshots(ctx context.Context, f QueryFilter) ([]Snapshot, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}

	var where []string
	var args []any

	if f.Since != nil {
		where = append(where, "s.taken_at >= ?")
		args = append(args, f.Since.Format(time.RFC3339))
	}
	if f.Until != nil {
		where = append(where, "s.taken_at <= ?")
		args = append(args, f.Until.Format(time.RFC3339))
	}
	if f.Label != "" {
		where = append(where, "s.label = ?")
		args = append(args, f.Label)
	}

	// Source and Asset filters require a join to positions.
	needJoin := f.Source != "" || f.Asset != ""
	fromClause := "snapshots s"
	if needJoin {
		fromClause = "snapshots s JOIN positions p ON p.snapshot_id = s.id"
		if f.Source != "" {
			where = append(where, "p.source = ?")
			args = append(args, f.Source)
		}
		if f.Asset != "" {
			where = append(where, "p.asset = ?")
			args = append(args, strings.ToUpper(f.Asset))
		}
	}

	query := fmt.Sprintf("SELECT DISTINCT s.id, s.taken_at, s.quote, s.total_value, s.label, s.note FROM %s", fromClause)
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY s.taken_at DESC"
	query += fmt.Sprintf(" LIMIT %d OFFSET %d", limit, f.Offset)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []Snapshot
	for rows.Next() {
		var snap Snapshot
		var takenAt string
		if err := rows.Scan(&snap.ID, &takenAt, &snap.Quote, &snap.TotalValue, &snap.Label, &snap.Note); err != nil {
			return nil, err
		}
		snap.TakenAt, _ = time.Parse(time.RFC3339, takenAt)
		results = append(results, snap)
	}
	return results, rows.Err()
}

// SnapshotSummary computes aggregate statistics over matching snapshots.
func (s *Store) SnapshotSummary(ctx context.Context, f SummaryFilter) (*Summary, error) {
	var where []string
	var args []any

	if f.Since != nil {
		where = append(where, "s.taken_at >= ?")
		args = append(args, f.Since.Format(time.RFC3339))
	}
	if f.Until != nil {
		where = append(where, "s.taken_at <= ?")
		args = append(args, f.Until.Format(time.RFC3339))
	}
	if f.Label != "" {
		where = append(where, "s.label = ?")
		args = append(args, f.Label)
	}

	needJoin := f.Source != ""
	fromClause := "snapshots s"
	if needJoin {
		fromClause = "snapshots s JOIN positions p ON p.snapshot_id = s.id"
		where = append(where, "p.source = ?")
		args = append(args, f.Source)
	}

	whereClause := ""
	if len(where) > 0 {
		whereClause = " WHERE " + strings.Join(where, " AND ")
	}

	// Aggregate query.
	aggQuery := fmt.Sprintf(
		`SELECT COUNT(*), COALESCE(MIN(s.taken_at),''), COALESCE(MAX(s.taken_at),''),
		        COALESCE(MIN(s.total_value),0), COALESCE(MAX(s.total_value),0),
		        COALESCE(AVG(s.total_value),0), COALESCE(s.quote,'')
		 FROM (SELECT DISTINCT s.id, s.taken_at, s.total_value, s.quote FROM %s%s) s`,
		fromClause, whereClause,
	)

	summary := &Summary{}
	var earliest, latest, quote string
	err := s.db.QueryRowContext(ctx, aggQuery, args...).Scan(
		&summary.Count, &earliest, &latest,
		&summary.MinValue, &summary.MaxValue, &summary.AvgValue, &quote,
	)
	if err != nil {
		return nil, err
	}
	summary.Quote = quote

	if summary.Count == 0 {
		return summary, nil
	}

	summary.Earliest, _ = time.Parse(time.RFC3339, earliest)
	summary.Latest, _ = time.Parse(time.RFC3339, latest)

	// Get earliest and latest values for change calculation.
	var earliestValue, latestValue float64
	earlyQ := fmt.Sprintf(
		`SELECT s.total_value FROM (SELECT DISTINCT s.id, s.taken_at, s.total_value FROM %s%s ORDER BY s.taken_at ASC LIMIT 1) s`,
		fromClause, whereClause,
	)
	s.db.QueryRowContext(ctx, earlyQ, args...).Scan(&earliestValue)

	lateQ := fmt.Sprintf(
		`SELECT s.total_value FROM (SELECT DISTINCT s.id, s.taken_at, s.total_value FROM %s%s ORDER BY s.taken_at DESC LIMIT 1) s`,
		fromClause, whereClause,
	)
	s.db.QueryRowContext(ctx, lateQ, args...).Scan(&latestValue)

	summary.LatestValue = latestValue
	summary.Change = latestValue - earliestValue
	if earliestValue != 0 {
		summary.ChangePct = (summary.Change / earliestValue) * 100
	}

	// Get second-to-last snapshot value for prev-change calculation.
	prevQ := fmt.Sprintf(
		`SELECT s.total_value FROM (SELECT DISTINCT s.id, s.taken_at, s.total_value FROM %s%s ORDER BY s.taken_at DESC LIMIT 2) s ORDER BY s.taken_at ASC LIMIT 1`,
		fromClause, whereClause,
	)
	var prevValue float64
	s.db.QueryRowContext(ctx, prevQ, args...).Scan(&prevValue)
	summary.PrevValue = prevValue
	summary.PrevChange = latestValue - prevValue
	if prevValue != 0 {
		summary.PrevChangePct = (summary.PrevChange / prevValue) * 100
	}

	// Group-by queries.
	if f.GroupBy != "" {
		groups, err := s.computeGroups(ctx, f, fromClause, whereClause, args)
		if err != nil {
			return nil, err
		}
		summary.Groups = groups
	}

	return summary, nil
}

func (s *Store) computeGroups(ctx context.Context, f SummaryFilter, fromClause, whereClause string, args []any) ([]GroupSummary, error) {
	var groupExpr string
	switch f.GroupBy {
	case "day":
		groupExpr = "SUBSTR(s.taken_at, 1, 10)"
	case "week":
		groupExpr = "STRFTIME('%Y-W%W', s.taken_at)"
	case "month":
		groupExpr = "SUBSTR(s.taken_at, 1, 7)"
	case "source":
		// Need positions join for source grouping.
		if !strings.Contains(fromClause, "positions") {
			fromClause = "snapshots s JOIN positions p ON p.snapshot_id = s.id"
		}
		groupExpr = "p.source"
	default:
		return nil, nil
	}

	query := fmt.Sprintf(
		`SELECT %s AS grp, COUNT(DISTINCT s.id), AVG(s.total_value), MIN(s.total_value), MAX(s.total_value)
		 FROM %s%s GROUP BY grp ORDER BY grp`,
		groupExpr, fromClause, whereClause,
	)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var groups []GroupSummary
	for rows.Next() {
		var g GroupSummary
		if err := rows.Scan(&g.Key, &g.Count, &g.AvgValue, &g.MinValue, &g.MaxValue); err != nil {
			return nil, err
		}
		groups = append(groups, g)
	}
	return groups, rows.Err()
}

// DeleteSnapshots removes snapshots matching the filter. It always keeps the
// most recent KeepLast snapshots (default 5) as a safety measure.
func (s *Store) DeleteSnapshots(ctx context.Context, f DeleteFilter) (int, error) {
	keepLast := f.KeepLast
	if keepLast <= 0 {
		keepLast = 5
	}

	// Get IDs to keep (most recent N).
	keepRows, err := s.db.QueryContext(ctx,
		`SELECT id FROM snapshots ORDER BY taken_at DESC LIMIT ?`, keepLast)
	if err != nil {
		return 0, err
	}
	var keepIDs []int64
	for keepRows.Next() {
		var id int64
		keepRows.Scan(&id)
		keepIDs = append(keepIDs, id)
	}
	keepRows.Close()

	var where []string
	var args []any

	if f.ID != nil {
		where = append(where, "id = ?")
		args = append(args, *f.ID)
	}
	if f.Before != nil {
		where = append(where, "taken_at < ?")
		args = append(args, f.Before.Format(time.RFC3339))
	}
	if f.Label != "" {
		where = append(where, "label = ?")
		args = append(args, f.Label)
	}

	if len(where) == 0 {
		return 0, fmt.Errorf("delete requires at least one filter (id, before, or label)")
	}

	// Exclude kept IDs.
	if len(keepIDs) > 0 {
		placeholders := make([]string, len(keepIDs))
		for i, id := range keepIDs {
			placeholders[i] = "?"
			args = append(args, id)
		}
		where = append(where, fmt.Sprintf("id NOT IN (%s)", strings.Join(placeholders, ",")))
	}

	// Delete positions first (cascade may handle this, but be explicit).
	delPosQuery := fmt.Sprintf(
		"DELETE FROM positions WHERE snapshot_id IN (SELECT id FROM snapshots WHERE %s)",
		strings.Join(where, " AND "),
	)
	s.db.ExecContext(ctx, delPosQuery, args...)

	delQuery := fmt.Sprintf("DELETE FROM snapshots WHERE %s", strings.Join(where, " AND "))
	res, err := s.db.ExecContext(ctx, delQuery, args...)
	if err != nil {
		return 0, err
	}

	n, _ := res.RowsAffected()
	return int(n), nil
}

func (s *Store) loadPositions(ctx context.Context, snapshotID int64) ([]Position, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, snapshot_id, source, account, category, asset, quantity, price, value, meta
		 FROM positions WHERE snapshot_id = ?`, snapshotID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var positions []Position
	for rows.Next() {
		var p Position
		var metaJSON string
		if err := rows.Scan(&p.ID, &p.SnapshotID, &p.Source, &p.Account, &p.Category,
			&p.Asset, &p.Quantity, &p.Price, &p.Value, &metaJSON); err != nil {
			return nil, err
		}
		if metaJSON != "" && metaJSON != "{}" {
			json.Unmarshal([]byte(metaJSON), &p.Meta)
		}
		positions = append(positions, p)
	}
	return positions, rows.Err()
}
