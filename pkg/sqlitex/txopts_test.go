package sqlitex_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex/sqlitextest"
)

// Documents how transaction options behave on an Open handle, whose
// transactions all begin with BEGIN IMMEDIATE.
func TestOpen_TxOptions(t *testing.T) {
	db := sqlitextest.Open(t)
	if _, err := db.Exec(`CREATE TABLE t(v INTEGER)`); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// A ReadOnly:true transaction still begins (and reads) normally.
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatalf("BeginTx(ReadOnly): %v", err)
	}
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM t`).Scan(&n); err != nil {
		t.Errorf("read in a ReadOnly tx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Error(err)
	}

	// It does not make the transaction write-proof on this driver; callers must
	// not rely on TxOptions.ReadOnly for safety. Use a sqlitex.ReadOnly() handle.
	tx, err = db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	_, werr := tx.ExecContext(ctx, `INSERT INTO t VALUES (1)`)
	t.Logf("write inside TxOptions{ReadOnly:true} on a read-write handle: err = %v", werr)
	_ = tx.Rollback()

	// A genuinely read-only handle: reads work, and so does a plain transaction.
	ro, err := sqlitex.Open(roPath(t, db), sqlitex.ReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	rtx, err := ro.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	t.Logf("BeginTx(ReadOnly:true) on a sqlitex.ReadOnly handle: err = %v", err)
	if err == nil {
		_ = rtx.Rollback()
	}
}

func roPath(t *testing.T, db *sql.DB) string {
	t.Helper()
	var seq int
	var name, file string
	if err := db.QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &file); err != nil {
		t.Fatal(err)
	}
	return file
}
