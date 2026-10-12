package notify

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex"
)

// component is this package's name in schema_migrations.
const component = "notify"

// retention bounds the growth of notify_sent on small devices.
const retention = 90 * 24 * time.Hour

var migrations = []sqlitex.Migration{{
	Component: component,
	Version:   1,
	SQL: `CREATE TABLE notify_sent (
	intervention_id     TEXT    NOT NULL,
	channel             TEXT    NOT NULL,
	chat_id             TEXT    NOT NULL,
	platform_message_id TEXT    NOT NULL,
	sent_at_ms          INTEGER NOT NULL,
	PRIMARY KEY (channel, platform_message_id)
);
CREATE INDEX notify_sent_by_time ON notify_sent (sent_at_ms);`,
}}

// store is the notify_sent table. It holds no other package's types and calls
// no other package inside a transaction.
type store struct {
	db *sql.DB
}

func newStore(ctx context.Context, db *sql.DB) (*store, error) {
	if err := sqlitex.Migrate(ctx, db, migrations...); err != nil {
		return nil, fmt.Errorf("notify: migrate: %w", err)
	}
	return &store{db: db}, nil
}

// prune deletes rows sent before cutoff and returns how many went.
func (s *store) prune(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM notify_sent WHERE sent_at_ms < ?`, cutoff.UnixMilli())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// record writes one row per platform message ID. A channel that reported no
// IDs gets a single row keyed by a synthetic ID, so the delivery still shows
// in the audit trail. A repeated (channel, ID) updates the row rather than
// failing.
func (s *store) record(ctx context.Context, interventionID, channel, chatID string, ids []string, at time.Time) error {
	if len(ids) == 0 {
		ids = []string{syntheticMessageID(interventionID)}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `INSERT INTO notify_sent
			(intervention_id, channel, chat_id, platform_message_id, sent_at_ms)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(channel, platform_message_id) DO UPDATE SET
				intervention_id = excluded.intervention_id,
				chat_id         = excluded.chat_id,
				sent_at_ms      = excluded.sent_at_ms`,
			interventionID, channel, chatID, id, at.UnixMilli()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// lookup maps a platform message ID back to the intervention that sent it.
func (s *store) lookup(ctx context.Context, channel, platformMsgID string) (string, bool, error) {
	var id string
	err := s.db.QueryRowContext(ctx,
		`SELECT intervention_id FROM notify_sent WHERE channel = ? AND platform_message_id = ?`,
		channel, platformMsgID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return id, true, nil
}

// syntheticMessageID is the row key used when a channel reports no message ID.
func syntheticMessageID(interventionID string) string {
	return "nomsgid:" + interventionID
}
