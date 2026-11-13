package sqlitex

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

func seedDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)`); err != nil {
		t.Fatal(err)
	}
	return db
}

func countRows(t *testing.T, path string) (n int, integrity string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+escapePath(filepath.ToSlash(path))+"?mode=ro&immutable=1")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.QueryRow(`SELECT count(*) FROM t`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil {
		t.Fatal(err)
	}
	return n, integrity
}

func TestBackup_ConsistentUnderWrites(t *testing.T) {
	dir := t.TempDir()
	db := seedDB(t, filepath.Join(dir, "src.db"))
	stop, done := make(chan struct{}), make(chan struct{})
	var inserted atomic.Int64
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := db.Exec(`INSERT INTO t (v) VALUES ('x')`); err != nil {
				return
			}
			inserted.Add(1)
		}
	}()
	for inserted.Load() < 50 {
		select {
		case <-done:
			t.Fatal("the writer stopped before 50 inserts")
		default:
			runtime.Gosched()
		}
	}
	dst := filepath.Join(dir, "copy.db")
	if err := Backup(context.Background(), db, dst); err != nil {
		t.Fatal(err)
	}
	close(stop)
	<-done
	n, integrity := countRows(t, dst)
	if integrity != "ok" || n < 50 || n > int(inserted.Load()) {
		t.Fatalf("copy: integrity %q, %d rows, writer inserted %d in all", integrity, n, inserted.Load())
	}
	for _, side := range []string{"-wal", "-shm", ".part"} {
		if _, err := os.Stat(dst + side); err == nil {
			t.Errorf("the copy left %s%s behind", dst, side)
		}
	}
	if err := Backup(context.Background(), db, dst); err == nil {
		t.Error("a backup over an existing file was allowed")
	}
}

// SQLite's verdict that a file is no database is told apart from every other failure: the
// sandbox snapshot copies such a file as bytes, and fails on any other error.
func TestIsNotADatabase(t *testing.T) {
	dir := t.TempDir()
	text := filepath.Join(dir, "notes")
	if err := os.WriteFile(text, []byte(strings.Repeat("plain text, not a database\n", 8)), 0o600); err != nil {
		t.Fatal(err)
	}
	cp := filepath.Join(dir, "notes-copy")
	db, err := Open(text, ReadOnly())
	if err == nil {
		t.Cleanup(func() { _ = db.Close() })
		err = Backup(context.Background(), db, cp)
	}
	if !IsNotADatabase(err) {
		t.Fatalf("a text file: err = %v, want SQLITE_NOTADB", err)
	}
	if _, err := os.Stat(cp); err == nil {
		t.Error("the refused backup left a copy")
	}

	// Controls: no error, and a failure that is not about the source.
	if IsNotADatabase(nil) {
		t.Error("nil is reported as not a database")
	}
	src := seedDB(t, filepath.Join(dir, "src.db"))
	dst := filepath.Join(dir, "copy.db")
	if err := Backup(context.Background(), src, dst); err != nil {
		t.Fatal(err)
	}
	if err := Backup(context.Background(), src, dst); err == nil || IsNotADatabase(err) {
		t.Errorf("a backup over an existing file: err = %v, want an error that is not SQLITE_NOTADB", err)
	}
}

func TestBackup_FallbackVacuumInto(t *testing.T) {
	dir := t.TempDir()
	db := seedDB(t, filepath.Join(dir, "src.db"))
	for range 20 {
		if _, err := db.Exec(`INSERT INTO t (v) VALUES ('y')`); err != nil {
			t.Fatal(err)
		}
	}
	old := driverBackup
	driverBackup = func(any, string) error { return errNoBackupAPI }
	t.Cleanup(func() { driverBackup = old })
	dst := filepath.Join(dir, "copy.db")
	if err := Backup(context.Background(), db, dst); err != nil {
		t.Fatal(err)
	}
	if n, integrity := countRows(t, dst); n != 20 || integrity != "ok" {
		t.Fatalf("fallback copy: %d rows, integrity %q", n, integrity)
	}
}
