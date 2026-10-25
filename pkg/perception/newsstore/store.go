// Package newsstore keeps what the news source has already read, in the shared
// proactive database, so that an article is parsed once and a restart does not read
// the same pages again. The source itself may not import the database package; it is
// given the sources.NewsSeen it needs, and this is the one the gateway gives it.
//
// Two tables, one migration (component "news", version 1): news_seen(url_hash PRIMARY
// KEY, at_ms) holds the sha1 of each article URL read, and news_parses(day PRIMARY KEY,
// n) counts the model calls of one owner-local day.
package newsstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex"
)

// Component is the migration component name.
const Component = "news"

var migrations = []sqlitex.Migration{{
	Component: Component,
	Version:   1,
	SQL: `CREATE TABLE news_seen (
	url_hash TEXT    PRIMARY KEY,
	at_ms    INTEGER NOT NULL
);
CREATE INDEX news_seen_at ON news_seen (at_ms);
CREATE TABLE news_parses (
	day TEXT    PRIMARY KEY,
	n   INTEGER NOT NULL DEFAULT 0
)`,
}}

// Store is the SQLite-backed seen-set and daily parse counter. It does not own the
// database.
type Store struct{ db *sql.DB }

// New migrates the tables on db and returns a store over it.
func New(ctx context.Context, db *sql.DB) (*Store, error) {
	if db == nil {
		return nil, errors.New("newsstore: nil database")
	}
	if err := sqlitex.Migrate(ctx, db, migrations...); err != nil {
		return nil, fmt.Errorf("newsstore: %w", err)
	}
	return &Store{db: db}, nil
}

// Seen reports whether the article with this hash has been read.
func (s *Store) Seen(ctx context.Context, hash string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM news_seen WHERE url_hash = ?`, hash).Scan(&one)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("newsstore: seen: %w", err)
	}
	return true, nil
}

// Mark records that the article was read at the given time.
func (s *Store) Mark(ctx context.Context, hash string, at time.Time) error {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO news_seen (url_hash, at_ms) VALUES (?, ?) ON CONFLICT(url_hash) DO UPDATE SET at_ms = excluded.at_ms`,
		hash, at.UnixMilli()); err != nil {
		return fmt.Errorf("newsstore: mark: %w", err)
	}
	return nil
}

// Prune deletes the entries older than before, and the parse counts of days older
// than a week before it.
func (s *Store) Prune(ctx context.Context, before time.Time) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM news_seen WHERE at_ms < ?`, before.UnixMilli()); err != nil {
		return fmt.Errorf("newsstore: prune: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM news_parses WHERE day < ?`,
		before.AddDate(0, 0, -7).Format("2006-01-02")); err != nil {
		return fmt.Errorf("newsstore: prune: %w", err)
	}
	return nil
}

// Parses is how many model calls the day (owner-local, "2006-01-02") has seen.
func (s *Store) Parses(ctx context.Context, day string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT n FROM news_parses WHERE day = ?`, day).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("newsstore: parses: %w", err)
	}
	return n, nil
}

// AddParse counts one model call against the day.
func (s *Store) AddParse(ctx context.Context, day string) error {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO news_parses (day, n) VALUES (?, 1) ON CONFLICT(day) DO UPDATE SET n = n + 1`, day); err != nil {
		return fmt.Errorf("newsstore: add parse: %w", err)
	}
	return nil
}
