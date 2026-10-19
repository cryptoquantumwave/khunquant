package replay

import (
	"context"
	"database/sql"
	"fmt"
	"sync/atomic"

	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

var memSeq atomic.Uint64

// openMemory opens a database that lives in memory only: nothing touches a disk, and
// each call gets a database of its own (a shared-cache name unique to the call, so the
// pool's connections all see the same tables, which a plain ":memory:" would not give
// them). It is closed with the returned *sql.DB. The pragmas are the ones
// sqlitex.Open sets, apart from the journal, which a memory database does not have.
func openMemory() (*sql.DB, error) {
	dsn := fmt.Sprintf("file:khunquant-replay-%d?mode=memory&cache=shared"+
		"&_pragma=busy_timeout(%d)&_pragma=foreign_keys(1)&_txlock=immediate",
		memSeq.Add(1), sqlitex.BusyTimeoutMillis)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("replay: %w", err)
	}
	// The database vanishes with its last connection: keep the pool's idle.
	db.SetMaxOpenConns(sqlitex.MaxOpenConns)
	db.SetMaxIdleConns(sqlitex.MaxOpenConns)
	if err := db.PingContext(context.Background()); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("replay: %w", err)
	}
	return db, nil
}
