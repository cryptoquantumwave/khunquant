package sqlitex

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Migration is one versioned schema step of one component (a package's tables).
type Migration struct {
	Component string // "attention", "intent", …
	Version   int    // 1,2,3 … per component, never reused
	SQL       string // may contain multiple statements; runs in one transaction
}

const createMigrationsTable = `CREATE TABLE IF NOT EXISTS schema_migrations (
	component  TEXT    NOT NULL,
	version    INTEGER NOT NULL,
	applied_at INTEGER NOT NULL,
	PRIMARY KEY (component, version)
)`

// Migrate applies, in (Component, Version) order, every migration in ms that
// db has not yet recorded in schema_migrations. Each migration's SQL and its
// bookkeeping row commit in one transaction: a step that fails rolls back,
// records nothing, and stops the run, leaving earlier steps applied.
//
// Per component, versions must start at 1 and be contiguous with no duplicates;
// that is checked before anything is touched. If db already holds a version of
// a component higher than every version supplied, a newer binary wrote it and
// Migrate refuses to run rather than operate on a schema it does not know.
//
// Migrate is safe to call concurrently, from several goroutines and from
// several processes sharing the file: each step takes the write lock with
// BEGIN IMMEDIATE and re-checks, under that lock, whether a peer already
// applied it. Pragmas such as foreign_keys have no effect inside a migration's
// transaction, so do not rely on them there.
func Migrate(ctx context.Context, db *sql.DB, ms ...Migration) error {
	sorted, err := validateMigrations(ms)
	if err != nil {
		return err
	}

	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("sqlitex: migrate: %w", err)
	}
	defer conn.Close()

	if err := runTx(ctx, conn, func() error {
		_, err := conn.ExecContext(ctx, createMigrationsTable)
		return err
	}); err != nil {
		return fmt.Errorf("sqlitex: migrate: create schema_migrations: %w", err)
	}

	if err := refuseNewerSchema(ctx, conn, sorted); err != nil {
		return err
	}

	for _, m := range sorted {
		if err := applyOne(ctx, conn, m); err != nil {
			return fmt.Errorf("sqlitex: migrate %s v%d: %w", m.Component, m.Version, err)
		}
	}
	return nil
}

// validateMigrations returns a copy of ms sorted by (Component, Version) after
// checking each component's versions are 1..n with no gaps or duplicates.
func validateMigrations(ms []Migration) ([]Migration, error) {
	sorted := append([]Migration(nil), ms...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Component != sorted[j].Component {
			return sorted[i].Component < sorted[j].Component
		}
		return sorted[i].Version < sorted[j].Version
	})
	next := map[string]int{}
	for _, m := range sorted {
		if strings.TrimSpace(m.Component) == "" {
			return nil, errors.New("sqlitex: migration with empty Component")
		}
		if strings.TrimSpace(m.SQL) == "" {
			return nil, fmt.Errorf("sqlitex: migration %s v%d has no SQL", m.Component, m.Version)
		}
		want := next[m.Component] + 1
		switch {
		case m.Version < want && m.Version >= 1:
			return nil, fmt.Errorf("sqlitex: duplicate migration %s v%d", m.Component, m.Version)
		case m.Version != want:
			return nil, fmt.Errorf("sqlitex: migration %s: expected version %d, got %d (versions start at 1 with no gaps)",
				m.Component, want, m.Version)
		}
		next[m.Component] = want
	}
	return sorted, nil
}

// refuseNewerSchema errors when the database records a version of a supplied
// component beyond the highest this binary knows.
func refuseNewerSchema(ctx context.Context, conn *sql.Conn, sorted []Migration) error {
	known := map[string]int{} // component -> highest supplied version
	for _, m := range sorted {
		known[m.Component] = m.Version
	}
	for comp, highest := range known {
		var applied sql.NullInt64
		err := conn.QueryRowContext(ctx,
			`SELECT MAX(version) FROM schema_migrations WHERE component = ?`, comp).Scan(&applied)
		if err != nil {
			return fmt.Errorf("sqlitex: migrate: read schema_migrations: %w", err)
		}
		if applied.Valid && int(applied.Int64) > highest {
			return fmt.Errorf("sqlitex: database has %s schema v%d but this build only knows up to v%d: "+
				"it was written by a newer version; upgrade KhunQuant instead of running on an unknown schema",
				comp, applied.Int64, highest)
		}
	}
	return nil
}

// applyOne runs one migration in its own IMMEDIATE transaction unless a peer
// already recorded it.
func applyOne(ctx context.Context, conn *sql.Conn, m Migration) error {
	return runTx(ctx, conn, func() error {
		var one int
		err := conn.QueryRowContext(ctx,
			`SELECT 1 FROM schema_migrations WHERE component = ? AND version = ?`,
			m.Component, m.Version).Scan(&one)
		switch {
		case err == nil:
			return nil // already applied, possibly by a concurrent caller
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		if _, err := conn.ExecContext(ctx, m.SQL); err != nil {
			return err
		}
		_, err = conn.ExecContext(ctx,
			`INSERT INTO schema_migrations (component, version, applied_at) VALUES (?, ?, ?)`,
			m.Component, m.Version, time.Now().UnixMilli())
		return err
	})
}

// runTx runs fn between BEGIN IMMEDIATE and COMMIT on one connection, rolling
// back if fn or the commit fails. It issues the statements itself, rather than
// using db.BeginTx, so the lock mode does not depend on how db was opened.
func runTx(ctx context.Context, conn *sql.Conn, fn func() error) error {
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	err := fn()
	if err == nil {
		if _, err = conn.ExecContext(ctx, "COMMIT"); err == nil {
			return nil
		}
	}
	// Roll back even if ctx is already cancelled, or the connection would go
	// back to the pool holding the write lock.
	// SQLite may already have rolled the transaction back itself (some errors
	// do), in which case ROLLBACK reports there is nothing to roll back.
	if _, rbErr := conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK"); rbErr != nil &&
		!strings.Contains(rbErr.Error(), "no transaction is active") {
		// Unknown transaction state: make sure the pool discards the connection.
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		err = errors.Join(err, fmt.Errorf("rollback: %w", rbErr))
	}
	return err
}
