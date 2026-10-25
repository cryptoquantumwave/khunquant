// Package bad is a fixture for the transaction-rule checker: it is never
// compiled (testdata is ignored by the go tool) and must keep violating the rule.
package bad

import (
	"database/sql"

	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex"
)

func F(db *sql.DB) {
	tx, _ := db.Begin()
	_, _ = sqlitex.Open("x")
	_ = tx.Commit()
}
