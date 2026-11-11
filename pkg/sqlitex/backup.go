package sqlitex

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"modernc.org/sqlite"
)

// errNoBackupAPI: the driver connection does not expose NewBackup (the driver moved).
var errNoBackupAPI = errors.New("sqlitex: the driver has no online backup")

// driverBackup copies the database of one driver connection into dst with SQLite's online
// backup API. A variable so a test can stand in a driver without it.
var driverBackup = func(driverConn any, dst string) error {
	b, ok := driverConn.(interface {
		NewBackup(dstURI string) (*sqlite.Backup, error)
	})
	if !ok {
		return errNoBackupAPI
	}
	bk, err := b.NewBackup(dst)
	if err != nil {
		return err
	}
	for more := true; more; {
		if more, err = bk.Step(-1); err != nil {
			_ = bk.Finish()
			return err
		}
	}
	return bk.Finish() // also closes the destination connection
}

// Backup writes a consistent copy of the database db is open on to dstPath, while other
// connections keep writing: the online backup API, or VACUUM INTO where the driver lacks it.
// The copy is self-contained (no -wal or -shm), written under a temporary name, synced, and
// renamed, so dstPath is either absent or whole. dstPath must not exist.
func Backup(ctx context.Context, db *sql.DB, dstPath string) error {
	abs, err := filepath.Abs(dstPath)
	if err != nil {
		return fmt.Errorf("sqlitex: backup: %w", err)
	}
	if _, err := os.Lstat(abs); err == nil {
		return fmt.Errorf("sqlitex: backup: %s already exists", abs)
	}
	tmp := abs + ".part"
	_ = os.Remove(tmp)
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("sqlitex: backup: %w", err)
	}
	defer conn.Close()
	err = conn.Raw(func(dc any) error {
		return driverBackup(dc, "file:"+escapePath(filepath.ToSlash(tmp)))
	})
	if errors.Is(err, errNoBackupAPI) { // outside Raw, which holds the connection
		_, err = conn.ExecContext(ctx, "VACUUM INTO ?", tmp)
	}
	if err == nil {
		err = syncAndRename(tmp, abs)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("sqlitex: backup to %s: %w", dstPath, err)
	}
	return nil
}

func syncAndRename(tmp, dst string) error {
	f, err := os.OpenFile(tmp, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}
