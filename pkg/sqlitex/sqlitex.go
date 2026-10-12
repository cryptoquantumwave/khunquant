// Package sqlitex opens SQLite databases the way every KhunQuant store should:
// the same pragmas on every pooled connection, a bounded pool, a shared
// process-wide handle for the proactive-agent database, and versioned
// migrations (see Migrate).
//
// The pre-existing stores (dca, deltaneutral, snapshot) run PRAGMAs with
// db.Exec on a pool, which configures only whichever connection happened to
// serve the call. Open instead puts every setting in the DSN, which the driver
// applies to each connection as it is created.
//
// # Transaction rule
//
// A transaction, or an open *sql.Rows, holds a pooled connection (and, for a
// write, the database write lock). Never call another package's store while one
// is open: with a pool of four connections, a handful of nested calls is enough
// to starve the pool and deadlock. Pass the *sql.Tx explicitly inside your own
// package, read rows fully and close them, then act.
// sqlitextest.AssertNoCrossPackageCallsInTx enforces this in tests.
//
// Because every connection also uses BEGIN IMMEDIATE for transactions (see
// Open), a transaction that starts by reading and then writes cannot fail with
// SQLITE_BUSY_SNAPSHOT, which busy_timeout does not retry.
package sqlitex

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

const (
	driverName = "sqlite"

	// MaxOpenConns is the size of the pool Open creates.
	MaxOpenConns = 4

	// BusyTimeoutMillis is how long a connection waits for a lock before
	// reporting SQLITE_BUSY.
	BusyTimeoutMillis = 5000

	sharedRelPath = "memory/proactive/proactive.db"
)

type config struct {
	readOnly bool
}

// Option customises Open.
type Option func(*config)

// ReadOnly opens the database with mode=ro: no migrations, and writes fail.
// It is meant for processes that only observe the database (the web launcher).
// The file must already exist; opening never creates it. If the database is in
// WAL mode, a read-only connection still needs write access to the directory
// to create the -shm file when no writer has it open.
func ReadOnly() Option { return func(c *config) { c.readOnly = true } }

// Open opens the SQLite database at path with WAL journaling,
// busy_timeout=5000, foreign_keys=ON and synchronous=NORMAL applied to every
// connection, a pool capped at MaxOpenConns, and BEGIN IMMEDIATE for
// transactions. Open does not create missing directories and runs no
// migrations. The file is created if it does not exist, unless ReadOnly is set.
func Open(path string, opts ...Option) (*sql.DB, error) {
	var cfg config
	for _, o := range opts {
		o(&cfg)
	}
	if path == "" {
		return nil, errors.New("sqlitex: empty database path")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("sqlitex: resolve %q: %w", path, err)
	}
	if cfg.readOnly {
		// mode=ro never creates the file, but its error message ("unable to
		// open database file") says little; name the real cause.
		if _, err := os.Stat(abs); err != nil {
			return nil, fmt.Errorf("sqlitex: open read-only: %w", err)
		}
	}

	db, err := sql.Open(driverName, dsn(abs, cfg))
	if err != nil {
		return nil, fmt.Errorf("sqlitex: open %q: %w", path, err)
	}
	db.SetMaxOpenConns(MaxOpenConns)
	db.SetMaxIdleConns(MaxOpenConns)
	// sql.Open is lazy; connect now so a bad path or a bad pragma surfaces here.
	if err := db.PingContext(context.Background()); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlitex: open %q: %w", path, err)
	}
	return db, nil
}

// dsn builds the modernc DSN. The driver hands the part before "?" to SQLite as
// a URI filename, so the path is percent-escaped: a bare "?" would end the
// path early, "#" would start a fragment, and "%" would be read as an escape.
func dsn(absPath string, cfg config) string {
	p := filepath.ToSlash(absPath)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p // Windows drive path: file:/C:/dir/x.db
	}
	var b strings.Builder
	b.WriteString("file:")
	b.WriteString(escapePath(p))

	q := url.Values{}
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", BusyTimeoutMillis))
	q.Add("_pragma", "foreign_keys(1)")
	if cfg.readOnly {
		// journal_mode is a write to the file when it has to change, so a
		// read-only handle leaves it alone and takes whatever the writer chose.
		q.Set("mode", "ro")
	} else {
		q.Add("_pragma", "journal_mode(WAL)")
		q.Add("_pragma", "synchronous(NORMAL)")
		// Every transaction starts with BEGIN IMMEDIATE: it takes the write lock
		// up front, where busy_timeout can wait for it. A deferred transaction
		// that reads and then writes fails at the upgrade with an error that
		// busy_timeout never retries.
		q.Set("_txlock", "immediate")
	}
	b.WriteByte('?')
	b.WriteString(q.Encode())
	return b.String()
}

func escapePath(p string) string {
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '/', c == '-', c == '_', c == '.', c == '~', c == ':':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

var (
	sharedMu sync.Mutex
	shared   = map[string]*sql.DB{}
)

// Shared returns the process-wide handle for
// <workspace>/memory/proactive/proactive.db, opening it on first use. Every
// caller using the same workspace gets the same *sql.DB, so the pool bound and
// lock ordering hold across packages. The directory is created 0700 and the
// file 0600 (where the platform honours modes).
//
// Shared runs no migrations: each package migrates its own tables with Migrate.
// Do not Close the returned handle; CloseShared owns it.
func Shared(workspace string) (*sql.DB, error) {
	if workspace == "" {
		return nil, errors.New("sqlitex: empty workspace")
	}
	abs, err := filepath.Abs(workspace)
	if err != nil {
		return nil, fmt.Errorf("sqlitex: resolve workspace %q: %w", workspace, err)
	}
	key := filepath.Clean(abs)

	sharedMu.Lock()
	defer sharedMu.Unlock()
	if db, ok := shared[key]; ok {
		return db, nil
	}

	path := filepath.Join(key, filepath.FromSlash(sharedRelPath))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("sqlitex: create %s: %w", filepath.Dir(path), err)
	}
	// Create the file ourselves so it is 0600 rather than the umask default;
	// SQLite gives the -wal and -shm files the main file's permissions.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("sqlitex: create %s: %w", path, err)
	}
	_ = f.Close()
	if runtime.GOOS != "windows" {
		_ = os.Chmod(path, 0o600) // an existing file keeps its old mode otherwise
	}

	db, err := Open(path)
	if err != nil {
		return nil, err
	}
	shared[key] = db
	return db, nil
}

// CloseShared closes every handle Shared returned and forgets them, so a later
// Shared call opens afresh. It is for shutdown and for tests.
func CloseShared() error {
	sharedMu.Lock()
	defer sharedMu.Unlock()
	var errs []error
	for key, db := range shared {
		if err := db.Close(); err != nil {
			errs = append(errs, fmt.Errorf("sqlitex: close %s: %w", key, err))
		}
		delete(shared, key)
	}
	return errors.Join(errs...)
}
