// Package sqlitextest holds test helpers for code built on pkg/sqlitex.
//
// It is a separate package, rather than an OpenForTest function inside
// sqlitex, so that importing "testing" never links the test framework into the
// production binary.
package sqlitextest

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex"
)

// Open returns a database in a fresh file under t.TempDir(), opened exactly as
// production code opens it (sqlitex.Open: WAL, busy_timeout, a pool of four),
// and closed by t.Cleanup. It runs no migrations; call sqlitex.Migrate with the
// package's own migrations.
func Open(t testing.TB) *sql.DB {
	t.Helper()
	db, err := sqlitex.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("sqlitextest.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}
