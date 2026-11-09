package sqlitex

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
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
