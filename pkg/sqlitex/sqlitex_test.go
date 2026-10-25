package sqlitex_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex/sqlitextest"
)

func pragma(ctx context.Context, t *testing.T, c *sql.Conn, name string) string {
	t.Helper()
	var v string
	if err := c.QueryRowContext(ctx, "PRAGMA "+name).Scan(&v); err != nil {
		t.Fatalf("PRAGMA %s: %v", name, err)
	}
	return v
}

// Spike S1 (kept as a permanent test): the driver applies the DSN's _pragma
// settings on EVERY pooled connection, not just the first.
func TestOpen_PragmasApplied(t *testing.T) {
	db := sqlitextest.Open(t)
	ctx := context.Background()

	conns := make([]*sql.Conn, 0, sqlitex.MaxOpenConns)
	for i := 0; i < sqlitex.MaxOpenConns; i++ {
		c, err := db.Conn(ctx)
		if err != nil {
			t.Fatalf("Conn %d: %v", i, err)
		}
		defer c.Close()
		conns = append(conns, c)
	}
	// All four are held at once, so they are four distinct driver connections.
	for i, c := range conns {
		if got := pragma(ctx, t, c, "journal_mode"); got != "wal" {
			t.Errorf("conn %d journal_mode = %q, want wal", i, got)
		}
		if got := pragma(ctx, t, c, "busy_timeout"); got != "5000" {
			t.Errorf("conn %d busy_timeout = %q, want 5000", i, got)
		}
		if got := pragma(ctx, t, c, "foreign_keys"); got != "1" {
			t.Errorf("conn %d foreign_keys = %q, want 1", i, got)
		}
		if got := pragma(ctx, t, c, "synchronous"); got != "1" { // 1 = NORMAL
			t.Errorf("conn %d synchronous = %q, want 1 (NORMAL)", i, got)
		}
	}

	// The pool is bounded: a fifth connection must wait.
	waitCtx, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	defer cancel()
	if c, err := db.Conn(waitCtx); err == nil {
		_ = c.Close()
		t.Error("a fifth simultaneous connection was granted; MaxOpenConns is not applied")
	} else if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("fifth Conn error = %v, want deadline exceeded", err)
	}
}

func TestOpen_Errors(t *testing.T) {
	if _, err := sqlitex.Open(""); err == nil {
		t.Error("empty path must fail")
	}
	if _, err := sqlitex.Open(filepath.Join(t.TempDir(), "no", "such", "dir", "x.db")); err == nil {
		t.Error("a path in a missing directory must fail (Open does not create directories)")
	}
}

// Paths containing characters that are special in a SQLite URI must reach the
// right file: "?" and "#" would otherwise truncate the name, "%" would be read
// as an escape.
func TestOpen_AwkwardPaths(t *testing.T) {
	for _, name := range []string{"with space", "q?mark", "hash#tag", "pct%41", "amp&x=1", "plus+eq=", "unicode-ภาษา"} {
		t.Run(name, func(t *testing.T) {
			if runtime.GOOS == "windows" && strings.ContainsAny(name, `?`) {
				t.Skip("not a legal file name on Windows")
			}
			dir := filepath.Join(t.TempDir(), name)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Skipf("cannot create directory %q on this platform: %v", name, err)
			}
			path := filepath.Join(dir, "d?b#1 %20.db")
			if runtime.GOOS == "windows" {
				path = filepath.Join(dir, "d#b1 %20.db")
			}

			db, err := sqlitex.Open(path)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			if _, err := db.Exec(`CREATE TABLE t(v TEXT); INSERT INTO t VALUES ('hello')`); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("database not created at the requested path: %v", err)
			}
			entries, _ := os.ReadDir(dir)
			if len(entries) != 1 {
				t.Errorf("expected only %q in %q, found %d entries (a differently named file was created?)",
					filepath.Base(path), dir, len(entries))
			}

			db, err = sqlitex.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var v string
			if err := db.QueryRow(`SELECT v FROM t`).Scan(&v); err != nil || v != "hello" {
				t.Errorf("reopen read = %q, %v", v, err)
			}
		})
	}
}

func TestOpen_ForeignKeysEnforcedOnEveryConnection(t *testing.T) {
	db := sqlitextest.Open(t)
	if _, err := db.Exec(`CREATE TABLE p(id INTEGER PRIMARY KEY); CREATE TABLE c(p INTEGER REFERENCES p(id))`); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var conns []*sql.Conn
	for i := 0; i < sqlitex.MaxOpenConns; i++ {
		c, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		conns = append(conns, c)
	}
	for i, c := range conns {
		if _, err := c.ExecContext(ctx, `INSERT INTO c VALUES (999)`); err == nil {
			t.Errorf("conn %d accepted a foreign-key violation", i)
		}
	}
}

// A transaction that reads and then writes must not fail with SQLITE_BUSY when
// several run at once; that is what BEGIN IMMEDIATE (the _txlock) buys.
func TestOpen_ReadThenWriteTxDoesNotBusy(t *testing.T) {
	db := sqlitextest.Open(t)
	if _, err := db.Exec(`CREATE TABLE n(v INTEGER)`); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				tx, err := db.BeginTx(context.Background(), nil)
				if err != nil {
					errs <- err
					return
				}
				var max sql.NullInt64
				if err := tx.QueryRow(`SELECT MAX(v) FROM n`).Scan(&max); err != nil {
					_ = tx.Rollback()
					errs <- err
					return
				}
				if _, err := tx.Exec(`INSERT INTO n VALUES (?)`, max.Int64+1); err != nil {
					_ = tx.Rollback()
					errs <- err
					return
				}
				if err := tx.Commit(); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("read-then-write transaction failed: %v", err)
	}
	var n, distinct int
	if err := db.QueryRow(`SELECT COUNT(*), COUNT(DISTINCT v) FROM n`).Scan(&n, &distinct); err != nil {
		t.Fatal(err)
	}
	if n != 200 || distinct != 200 {
		t.Errorf("rows = %d, distinct = %d; want 200/200 (a lost update means the tx was not serialised)", n, distinct)
	}
}

func TestReadOnly_RejectsWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ro.db")
	writer, err := sqlitex.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err := writer.Exec(`CREATE TABLE t(v INTEGER); INSERT INTO t VALUES (1)`); err != nil {
		t.Fatal(err)
	}

	// The writer stays open, so the file is live in WAL mode.
	ro, err := sqlitex.Open(path, sqlitex.ReadOnly())
	if err != nil {
		t.Fatalf("Open ReadOnly: %v", err)
	}
	defer ro.Close()

	var v int
	if err := ro.QueryRow(`SELECT v FROM t`).Scan(&v); err != nil || v != 1 {
		t.Fatalf("read through ro handle = %d, %v", v, err)
	}
	// A write made after the ro handle opened is visible to it.
	if _, err := writer.Exec(`INSERT INTO t VALUES (2)`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := ro.QueryRow(`SELECT COUNT(*) FROM t`).Scan(&n); err != nil || n != 2 {
		t.Errorf("ro handle sees %d rows, %v; want 2", n, err)
	}

	for _, stmt := range []string{
		`INSERT INTO t VALUES (3)`, `UPDATE t SET v = 9`, `DELETE FROM t`,
		`CREATE TABLE u(x)`, `DROP TABLE t`,
	} {
		if _, err := ro.Exec(stmt); err == nil {
			t.Errorf("read-only handle accepted %q", stmt)
		}
	}
	if err := sqlitex.Migrate(context.Background(), ro, sqlitex.Migration{Component: "x", Version: 1, SQL: "CREATE TABLE x(a)"}); err == nil {
		t.Error("Migrate on a read-only handle must fail")
	}
	if err := writer.QueryRow(`SELECT COUNT(*) FROM t`).Scan(&n); err != nil || n != 2 {
		t.Errorf("rows after rejected writes = %d, %v; want 2", n, err)
	}
}

func TestReadOnly_MissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.db")
	if db, err := sqlitex.Open(path, sqlitex.ReadOnly()); err == nil {
		_ = db.Close()
		t.Fatal("opening a missing file read-only must fail")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("read-only open created the file (stat err = %v)", err)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 0 {
		t.Errorf("read-only open left %d files behind", len(entries))
	}
}

func TestShared_SingleHandlePerWorkspace(t *testing.T) {
	t.Cleanup(func() { _ = sqlitex.CloseShared() })
	ws := t.TempDir()

	a, err := sqlitex.Shared(ws)
	if err != nil {
		t.Fatal(err)
	}
	// The same workspace spelled differently is still the same workspace.
	sep := string(filepath.Separator)
	b, err := sqlitex.Shared(ws + sep + "." + sep + "sub" + sep + "..") // uncleaned spelling
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Error("equivalent workspace paths returned different handles")
	}
	t.Chdir(filepath.Dir(ws))
	rel, err := sqlitex.Shared(filepath.Base(ws)) // relative spelling
	if err != nil {
		t.Fatal(err)
	}
	if rel != a {
		t.Error("a relative workspace path returned a different handle")
	}

	other, err := sqlitex.Shared(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if other == a {
		t.Error("different workspaces shared a handle")
	}

	// Concurrent first-use callers all get one handle.
	ws2 := t.TempDir()
	got := make([]*sql.DB, 16)
	var wg sync.WaitGroup
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			db, err := sqlitex.Shared(ws2)
			if err != nil {
				t.Error(err)
			}
			got[i] = db
		}()
	}
	wg.Wait()
	for i, db := range got {
		if db != got[0] {
			t.Errorf("caller %d got a different handle", i)
		}
	}

	// Location, creation and permissions.
	dbPath := filepath.Join(ws, "memory", "proactive", "proactive.db")
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("database not at %s: %v", dbPath, err)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(filepath.Dir(dbPath)); fi.Mode().Perm() != 0o700 {
			t.Errorf("directory mode = %v, want 0700", fi.Mode().Perm())
		}
		if fi, _ := os.Stat(dbPath); fi.Mode().Perm() != 0o600 {
			t.Errorf("file mode = %v, want 0600", fi.Mode().Perm())
		}
	}

	// Shared runs no migrations.
	var n int
	if err := a.QueryRow(`SELECT COUNT(*) FROM sqlite_master`).Scan(&n); err != nil || n != 0 {
		t.Errorf("fresh shared database has %d schema objects (err %v); Shared must not migrate", n, err)
	}

	// CloseShared closes the handles and allows reopening.
	if err := sqlitex.CloseShared(); err != nil {
		t.Fatal(err)
	}
	if err := a.Ping(); err == nil {
		t.Error("handle still usable after CloseShared")
	}
	c, err := sqlitex.Shared(ws)
	if err != nil {
		t.Fatal(err)
	}
	if c == a {
		t.Error("CloseShared did not forget the handle")
	}
	if err := c.Ping(); err != nil {
		t.Errorf("reopened handle unusable: %v", err)
	}
	if err := sqlitex.CloseShared(); err != nil {
		t.Errorf("second CloseShared: %v", err)
	}
}

func TestShared_Errors(t *testing.T) {
	if _, err := sqlitex.Shared(""); err == nil {
		t.Error("empty workspace must fail")
	}
	// A regular file where the directory should go.
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "memory"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlitex.Shared(ws); err == nil {
		t.Error("expected an error when memory/ is a file")
	}
}

// Acceptance test: 100 goroutines doing interleaved writes and reads through
// Shared() never see SQLITE_BUSY / "database is locked".
func TestShared_100Writers_NoBusy(t *testing.T) {
	t.Cleanup(func() { _ = sqlitex.CloseShared() })
	db, err := sqlitex.Shared(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE ev(id INTEGER PRIMARY KEY, w INTEGER, i INTEGER)`); err != nil {
		t.Fatal(err)
	}

	const writers, iters = 100, 15
	var wg sync.WaitGroup
	errc := make(chan error, writers*iters)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := context.Background()
			for i := 0; i < iters; i++ {
				var err error
				switch (w + i) % 4 {
				case 0: // plain write
					_, err = db.ExecContext(ctx, `INSERT INTO ev(w, i) VALUES (?, ?)`, w, i)
				case 1: // read
					var n int
					err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ev`).Scan(&n)
				case 2: // read-then-write transaction
					err = readThenWrite(ctx, db, w, i)
				case 3: // rows fully read, then a write
					err = scanThenWrite(ctx, db, w, i)
				}
				if err != nil {
					errc <- fmt.Errorf("writer %d iter %d: %w", w, i, err)
				}
			}
		}()
	}
	wg.Wait()
	close(errc)
	for err := range errc {
		t.Error(err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ev`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	// Each writer inserts on 3 of every 4 iterations (cases 0, 2, 3).
	want := 0
	for w := 0; w < writers; w++ {
		for i := 0; i < iters; i++ {
			if (w+i)%4 != 1 {
				want++
			}
		}
	}
	if n != want {
		t.Errorf("rows = %d, want %d: writes were lost", n, want)
	}
}

func readThenWrite(ctx context.Context, db *sql.DB, w, i int) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM ev`).Scan(&n); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO ev(w, i) VALUES (?, ?)`, w, i); err != nil {
		return err
	}
	return tx.Commit()
}

func scanThenWrite(ctx context.Context, db *sql.DB, w, i int) error {
	rows, err := db.QueryContext(ctx, `SELECT id FROM ev ORDER BY id DESC LIMIT 5`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `INSERT INTO ev(w, i) VALUES (?, ?)`, w, i)
	return err
}
