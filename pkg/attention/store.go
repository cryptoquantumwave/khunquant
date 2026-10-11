package attention

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex"
)

// Component is the sqlitex migration component of this package.
const Component = "attention"

// maxCandidateJSON caps the candidate JSON stored per row. Evidence can hold
// arbitrary values, and on a small device an unbounded column is a way to fill
// the disk.
const maxCandidateJSON = 16 * 1024

// maxClampedText bounds Title and Subject when even the Evidence-less JSON is
// over the cap.
const maxClampedText = 512

// Reading attention_decisions: delivered is the only authority on whether the
// owner was told. delivered_at_ms is set when delivered becomes 1, and ALSO on a
// row whose delivered is still 0 when its send was reported failed (that marker
// releases the row's budget and dedup reservation). Consumers must test
// delivered, never delivered_at_ms IS NOT NULL.
var migrations = []sqlitex.Migration{
	{
		Component: Component,
		Version:   1,
		SQL: `CREATE TABLE attention_state (
	dedup_key           TEXT PRIMARY KEY,
	last_severity       INTEGER NOT NULL,
	last_sent_ms        INTEGER NOT NULL,
	last_candidate_json TEXT NOT NULL,
	sends               INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE attention_decisions (
	id              TEXT PRIMARY KEY,
	at_ms           INTEGER NOT NULL,
	dedup_key       TEXT NOT NULL,
	trigger_id      TEXT NOT NULL,
	severity        INTEGER NOT NULL,
	disposition     TEXT NOT NULL,
	reason          TEXT NOT NULL,
	shadow          INTEGER NOT NULL DEFAULT 0,
	candidate_json  TEXT NOT NULL,
	delivered       INTEGER NOT NULL DEFAULT 0,
	delivered_at_ms INTEGER
);
CREATE INDEX idx_attention_decisions_at ON attention_decisions(at_ms);
CREATE INDEX idx_attention_decisions_key ON attention_decisions(dedup_key, at_ms);
CREATE INDEX idx_attention_decisions_delivered ON attention_decisions(delivered, delivered_at_ms);
CREATE TABLE attention_mutes (
	key        TEXT PRIMARY KEY,
	until_ms   INTEGER NOT NULL,
	reason     TEXT NOT NULL,
	created_ms INTEGER NOT NULL
);`,
	},
}

// dispositionsSent are the dispositions that put a message in front of the owner
// (or ask the LLM to): the only ones that reserve budget and dedup.
const sentDispositions = `('notify','escalate')`

// encodeCandidate returns the JSON stored for c, never longer than
// maxCandidateJSON. Past the cap, Evidence is replaced with {"truncated":true};
// if the rest still does not fit, Title and Subject are clamped too. A candidate
// that cannot be marshalled at all (NaN in Evidence, say) gets the same
// treatment, so a bad value costs the evidence, not the decision.
func encodeCandidate(c proactive.Candidate) string {
	if b, err := json.Marshal(c); err == nil && len(b) <= maxCandidateJSON {
		return string(b)
	}
	c.Evidence = map[string]any{"truncated": true}
	if b, err := json.Marshal(c); err == nil && len(b) <= maxCandidateJSON {
		return string(b)
	}
	c.Title = clampRunes(c.Title, maxClampedText)
	c.Subject = clampRunes(c.Subject, maxClampedText)
	c.DedupKey = clampRunes(c.DedupKey, maxClampedText)
	c.TriggerID = clampRunes(c.TriggerID, maxClampedText)
	c.IntentID = clampRunes(c.IntentID, maxClampedText)
	b, err := json.Marshal(c)
	if err != nil || len(b) > maxCandidateJSON {
		return `{"truncated":true}`
	}
	return string(b)
}

func clampRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}

// decodeCandidate parses stored candidate JSON; ok is false when it does not
// parse (a hand-edited row), so callers can carry on without a Previous.
func decodeCandidate(s string) (proactive.Candidate, bool) {
	var c proactive.Candidate
	if err := json.Unmarshal([]byte(s), &c); err != nil {
		return proactive.Candidate{}, false
	}
	return c, true
}

// ---- reads (all take the caller's transaction) -----------------------------

// deleteExpiredMutes removes mutes whose time has passed.
func deleteExpiredMutes(ctx context.Context, tx *sql.Tx, nowMs int64) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM attention_mutes WHERE until_ms <= ?`, nowMs); err != nil {
		return fmt.Errorf("attention: expire mutes: %w", err)
	}
	return nil
}

// isMuted reports whether an active mute applies to c. Prefixes are compared
// literally (substr, not LIKE) so a key containing % or _ cannot widen a mute.
// A protective Critical candidate is only muted by a prefix that equals its full
// DedupKey or full TriggerID.
func isMuted(ctx context.Context, tx *sql.Tx, c proactive.Candidate, nowMs int64) (bool, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT key FROM attention_mutes
		  WHERE until_ms > ?
		    AND (substr(?, 1, length(key)) = key OR substr(?, 1, length(key)) = key)`,
		nowMs, c.DedupKey, c.TriggerID)
	if err != nil {
		return false, fmt.Errorf("attention: read mutes: %w", err)
	}
	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			_ = rows.Close()
			return false, fmt.Errorf("attention: read mutes: %w", err)
		}
		keys = append(keys, k)
	}
	if err := rows.Close(); err != nil {
		return false, fmt.Errorf("attention: read mutes: %w", err)
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("attention: read mutes: %w", err)
	}
	if len(keys) == 0 {
		return false, nil
	}
	if !isProtectiveCritical(c) {
		return true, nil
	}
	for _, k := range keys {
		if k == c.DedupKey || (c.TriggerID != "" && k == c.TriggerID) {
			return true, nil
		}
	}
	return false, nil
}

// stateRow is attention_state for one dedup key.
type stateRow struct {
	exists       bool
	lastSeverity proactive.Severity
	lastSentMs   int64
	previous     *proactive.Candidate
}

func readState(ctx context.Context, tx *sql.Tx, key string) (stateRow, error) {
	var (
		sev  int
		sent int64
		js   string
	)
	err := tx.QueryRowContext(ctx,
		`SELECT last_severity, last_sent_ms, last_candidate_json FROM attention_state WHERE dedup_key = ?`,
		key).Scan(&sev, &sent, &js)
	if errors.Is(err, sql.ErrNoRows) {
		return stateRow{}, nil
	}
	if err != nil {
		return stateRow{}, fmt.Errorf("attention: read state: %w", err)
	}
	s := stateRow{exists: true, lastSeverity: proactive.Severity(sev), lastSentMs: sent}
	if prev, ok := decodeCandidate(js); ok {
		s.previous = &prev
	}
	return s, nil
}

// inFlight reports whether a notify/escalate decision for key at severity >= sev
// was made within the lease and has been neither delivered nor reported failed.
func inFlight(ctx context.Context, tx *sql.Tx, key string, sev proactive.Severity, leaseStartMs int64) (bool, error) {
	return exists(ctx, tx,
		`SELECT 1 FROM attention_decisions
		  WHERE dedup_key = ? AND at_ms > ? AND severity >= ?
		    AND disposition IN `+sentDispositions+`
		    AND delivered = 0 AND delivered_at_ms IS NULL
		  LIMIT 1`, key, leaseStartMs, int(sev))
}

// queuedDigest reports whether an undelivered digest item for key at severity
// >= sev exists newer than sinceMs.
func queuedDigest(ctx context.Context, tx *sql.Tx, key string, sev proactive.Severity, sinceMs int64) (bool, error) {
	return exists(ctx, tx,
		`SELECT 1 FROM attention_decisions
		  WHERE dedup_key = ? AND at_ms > ? AND severity >= ?
		    AND disposition = 'digest' AND delivered = 0
		  LIMIT 1`, key, sinceMs, int(sev))
}

func exists(ctx context.Context, tx *sql.Tx, q string, args ...any) (bool, error) {
	var one int
	err := tx.QueryRowContext(ctx, q, args...).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("attention: query: %w", err)
	}
	return true, nil
}

// sendsToday counts the non-critical, non-protective notify/escalate decisions
// that were delivered inside [dayStartMs, dayEndMs), plus those still in flight
// (decided within the lease, not yet delivered or reported failed). Counting
// in-flight decisions is what stops concurrent callers overspending the budget:
// a decision reserves its slot when it is made and gives it back only when its
// send is reported failed.
func sendsToday(ctx context.Context, tx *sql.Tx, dayStartMs, dayEndMs, leaseStartMs int64) (int, error) {
	var delivered, pending int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM attention_decisions
		  WHERE delivered = 1 AND delivered_at_ms >= ? AND delivered_at_ms < ?
		    AND disposition IN `+sentDispositions+`
		    AND severity < ? AND reason <> ?`,
		dayStartMs, dayEndMs, int(proactive.SeverityCritical), ReasonProtective).Scan(&delivered); err != nil {
		return 0, fmt.Errorf("attention: count sends: %w", err)
	}
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM attention_decisions
		  WHERE delivered = 0 AND delivered_at_ms IS NULL AND at_ms >= ?
		    AND disposition IN `+sentDispositions+`
		    AND severity < ? AND reason <> ?`,
		leaseStartMs, int(proactive.SeverityCritical), ReasonProtective).Scan(&pending); err != nil {
		return 0, fmt.Errorf("attention: count in-flight sends: %w", err)
	}
	return delivered + pending, nil
}

// ---- writes ----------------------------------------------------------------

func insertDecision(ctx context.Context, tx *sql.Tx, d Decision) error {
	shadow := 0
	if d.Reason == ReasonShadow {
		shadow = 1
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO attention_decisions
		   (id, at_ms, dedup_key, trigger_id, severity, disposition, reason, shadow, candidate_json)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.ID, d.At.UnixMilli(), d.Candidate.DedupKey, d.Candidate.TriggerID,
		int(d.Candidate.Severity), string(d.Disposition), d.Reason, shadow,
		encodeCandidate(d.Candidate))
	if err != nil {
		return fmt.Errorf("attention: record decision: %w", err)
	}
	return nil
}

// recentShadow returns the ID of a log/shadow row for key with severity >= sev
// newer than sinceMs, if one exists.
func recentShadow(ctx context.Context, tx *sql.Tx, key string, sev proactive.Severity, sinceMs int64) (string, bool, error) {
	var id string
	err := tx.QueryRowContext(ctx,
		`SELECT id FROM attention_decisions
		  WHERE dedup_key = ? AND at_ms > ? AND severity >= ?
		    AND disposition = 'log' AND reason = ?
		  ORDER BY at_ms DESC LIMIT 1`, key, sinceMs, int(sev), ReasonShadow).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("attention: query shadow: %w", err)
	}
	return id, true, nil
}

// pruneDecisions deletes decisions older than retention, except undelivered
// digest items younger than keepPendingDigest (they have not been told to the
// owner yet), shadow rows older than shadowRetention, and state rows not
// touched within retention.
func pruneDecisions(ctx context.Context, db *sql.DB, now time.Time, retention, keepPendingDigest, shadowRetention time.Duration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("attention: prune: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	cutoff := now.Add(-retention).UnixMilli()
	keepSince := now.Add(-keepPendingDigest).UnixMilli()
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM attention_decisions
		  WHERE at_ms < ?
		    AND NOT (disposition = 'digest' AND delivered = 0 AND at_ms >= ?)`,
		cutoff, keepSince); err != nil {
		return fmt.Errorf("attention: prune decisions: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM attention_decisions WHERE reason = ? AND at_ms < ?`,
		ReasonShadow, now.Add(-shadowRetention).UnixMilli()); err != nil {
		return fmt.Errorf("attention: prune shadow: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM attention_state WHERE last_sent_ms < ?`, cutoff); err != nil {
		return fmt.Errorf("attention: prune state: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("attention: prune: %w", err)
	}
	return nil
}
