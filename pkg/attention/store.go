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
//
// intervention_id (v2) names the message a delivered decision went out in:
// decisions delivered together share one. It is NULL on rows from before v2 and
// until delivery; read it as COALESCE(intervention_id, id). base_asset (v2) is
// the upper-case base asset of the candidate, for inhibition.
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
	{
		Component: Component,
		Version:   2,
		// Additive only: other readers (the web launcher's decisions endpoint)
		// select from attention_decisions, so nothing is renamed or dropped.
		SQL: `ALTER TABLE attention_decisions ADD COLUMN intervention_id TEXT;
ALTER TABLE attention_decisions ADD COLUMN base_asset TEXT;
CREATE INDEX idx_attention_decisions_base ON attention_decisions(base_asset, at_ms) WHERE base_asset IS NOT NULL;
CREATE INDEX idx_attention_decisions_intervention ON attention_decisions(intervention_id) WHERE intervention_id IS NOT NULL;
CREATE TABLE attention_digest_queue (
	decision_id TEXT PRIMARY KEY REFERENCES attention_decisions(id) ON DELETE CASCADE,
	enqueued_ms INTEGER NOT NULL,
	flushed_ms  INTEGER
);
CREATE INDEX idx_attention_digest_queue_pending ON attention_digest_queue(flushed_ms, enqueued_ms);
INSERT INTO attention_digest_queue (decision_id, enqueued_ms)
	SELECT id, at_ms FROM attention_decisions WHERE disposition = 'digest' AND delivered = 0;
CREATE TABLE attention_meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);`,
	},
	{
		Component: Component,
		Version:   3,
		// exact marks a mute that matches one dedup key and nothing longer (a
		// snooze); every existing row is a prefix mute and stays one. An exact
		// mute is stored under "=" + the dedup key, so it can never be the same
		// row as a prefix mute of the same text.
		SQL: `ALTER TABLE attention_mutes ADD COLUMN exact INTEGER NOT NULL DEFAULT 0;`,
	},
	{
		Component: Component,
		Version:   4,
		// Learning (learning.go). All five are NULL, or 0 for holdout, on a row made
		// while learning is off and on every row from before. propensity is the
		// probability the stored disposition had under the live policy; holdout is 1 for
		// a Notice held back by the holdout; shadow_disposition is the candidate
		// policy's verdict, stored and never acted on; learned_disposition and
		// learned_reason are what learning would have decided when adapt is off and it
		// would have decided differently.
		SQL: `ALTER TABLE attention_decisions ADD COLUMN propensity REAL;
ALTER TABLE attention_decisions ADD COLUMN holdout INTEGER NOT NULL DEFAULT 0;
ALTER TABLE attention_decisions ADD COLUMN shadow_disposition TEXT;
ALTER TABLE attention_decisions ADD COLUMN learned_disposition TEXT;
ALTER TABLE attention_decisions ADD COLUMN learned_reason TEXT;`,
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
// DedupKey or full TriggerID. An exact mute (a snooze) matches the candidate's
// DedupKey and nothing longer; it is by definition the full key.
func isMuted(ctx context.Context, tx *sql.Tx, c proactive.Candidate, nowMs int64) (bool, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT key, exact FROM attention_mutes
		  WHERE until_ms > ?
		    AND ((exact = 0 AND (substr(?, 1, length(key)) = key OR substr(?, 1, length(key)) = key))
		      OR (exact = 1 AND substr(key, 2) = ?))`,
		nowMs, c.DedupKey, c.TriggerID, c.DedupKey)
	if err != nil {
		return false, fmt.Errorf("attention: read mutes: %w", err)
	}
	var keys []string
	exactHit := false
	for rows.Next() {
		var k string
		var exact int
		if err := rows.Scan(&k, &exact); err != nil {
			_ = rows.Close()
			return false, fmt.Errorf("attention: read mutes: %w", err)
		}
		if exact == 1 {
			exactHit = true
			continue
		}
		keys = append(keys, k)
	}
	if err := rows.Close(); err != nil {
		return false, fmt.Errorf("attention: read mutes: %w", err)
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("attention: read mutes: %w", err)
	}
	if exactHit {
		return true, nil
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

// sendsToday counts the MESSAGES, not the decisions, that the non-critical,
// non-protective notify/escalate decisions of the day amount to.
//
// Delivered decisions inside [dayStartMs, dayEndMs) count once per distinct
// intervention ID (a row without one is its own message). Decisions still in
// flight (decided within the lease, neither delivered nor reported failed) are
// not yet in a message, so they are grouped the way the outbox will group them:
// those decided within groupWindowMs of the oldest of the group are one pending
// message; with no window each is its own. Counting the in-flight ones is what
// stops concurrent callers overspending the budget: a decision reserves its
// slot when it is made and gives it back only when its send is reported failed.
func sendsToday(ctx context.Context, tx *sql.Tx, dayStartMs, dayEndMs, leaseStartMs, groupWindowMs, nowMs int64) (sends int, joinsPending bool, err error) {
	var delivered int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(DISTINCT COALESCE(intervention_id, id)) FROM attention_decisions
		  WHERE delivered = 1 AND delivered_at_ms >= ? AND delivered_at_ms < ?
		    AND disposition IN `+sentDispositions+`
		    AND severity < ? AND reason <> ?`,
		dayStartMs, dayEndMs, int(proactive.SeverityCritical), ReasonProtective).Scan(&delivered); err != nil {
		return 0, false, fmt.Errorf("attention: count sends: %w", err)
	}

	rows, err := tx.QueryContext(ctx,
		`SELECT at_ms FROM attention_decisions
		  WHERE delivered = 0 AND delivered_at_ms IS NULL AND at_ms >= ?
		    AND disposition IN `+sentDispositions+`
		    AND severity < ? AND reason <> ?
		  ORDER BY at_ms`,
		leaseStartMs, int(proactive.SeverityCritical), ReasonProtective)
	if err != nil {
		return 0, false, fmt.Errorf("attention: count in-flight sends: %w", err)
	}
	var pending []int64
	for rows.Next() {
		var at int64
		if err := rows.Scan(&at); err != nil {
			_ = rows.Close()
			return 0, false, fmt.Errorf("attention: count in-flight sends: %w", err)
		}
		pending = append(pending, at)
	}
	if err := rows.Close(); err != nil {
		return 0, false, fmt.Errorf("attention: count in-flight sends: %w", err)
	}
	if err := rows.Err(); err != nil {
		return 0, false, fmt.Errorf("attention: count in-flight sends: %w", err)
	}
	n, lastStart := pendingMessages(pending, groupWindowMs)
	// A candidate decided now joins the newest pending message, at no extra cost,
	// when that message is still open: the outbox takes decisions into a group
	// until the window that the group's first decision opened has elapsed.
	joins := n > 0 && groupWindowMs > 0 && nowMs-lastStart < groupWindowMs
	return delivered + n, joins, nil
}

// pendingMessages is how many messages the in-flight decisions decided at
// atMs (ascending) will become, and when the newest of them opened: a message
// opens at its first decision and takes every decision made less than windowMs
// after that one. With no window every decision is a message of its own.
func pendingMessages(atMs []int64, windowMs int64) (n int, lastStart int64) {
	for i, at := range atMs {
		if i == 0 || windowMs <= 0 || at-lastStart >= windowMs {
			n++
			lastStart = at
		}
	}
	return n, lastStart
}

// inhibitorKey returns the dedup key of the freshest message that explains a
// candidate with base asset base under rule, or "" when none does. An inhibitor
// is a notify/escalate decision whose dedup key starts with rule.WhenPrefix,
// decided after sinceMs, that was delivered or is still in flight (decided after
// leaseStartMs and not reported failed). A decision whose send failed has
// delivered = 0 and delivered_at_ms set, so it matches neither arm. Prefixes are
// compared literally (substr, not LIKE).
func inhibitorKey(ctx context.Context, tx *sql.Tx, rule InhibitRule, base string, sinceMs, leaseStartMs int64) (string, error) {
	q := `SELECT dedup_key FROM attention_decisions
	       WHERE disposition IN ` + sentDispositions + `
	         AND at_ms > ?
	         AND substr(dedup_key, 1, length(?)) = ?
	         AND (delivered = 1 OR (delivered = 0 AND delivered_at_ms IS NULL AND at_ms > ?))`
	args := []any{sinceMs, rule.WhenPrefix, rule.WhenPrefix, leaseStartMs}
	if rule.SameBaseAsset {
		q += ` AND base_asset = ?`
		args = append(args, base)
	}
	q += ` ORDER BY at_ms DESC LIMIT 1`
	var key string
	err := tx.QueryRowContext(ctx, q, args...).Scan(&key)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("attention: query inhibitor: %w", err)
	}
	return key, nil
}

// ---- writes ----------------------------------------------------------------

func insertDecision(ctx context.Context, tx *sql.Tx, d Decision, lr learnRow) error {
	shadow := 0
	if d.Reason == ReasonShadow {
		shadow = 1
	}
	var base any // NULL when the candidate has no base asset
	if b := baseAsset(d.Candidate); b != "" {
		base = b
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO attention_decisions
		   (id, at_ms, dedup_key, trigger_id, severity, disposition, reason, shadow, candidate_json, base_asset,
		    propensity, holdout, shadow_disposition, learned_disposition, learned_reason)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.ID, d.At.UnixMilli(), d.Candidate.DedupKey, d.Candidate.TriggerID,
		int(d.Candidate.Severity), string(d.Disposition), d.Reason, shadow,
		encodeCandidate(d.Candidate), base,
		lr.propensity, lr.holdout, lr.shadow, lr.learned, lr.learnedReason)
	if err != nil {
		return fmt.Errorf("attention: record decision: %w", err)
	}
	if d.Disposition == proactive.DispDigest {
		// Same transaction as the decision: a digest decision without its queue
		// row would be told to nobody.
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO attention_digest_queue (decision_id, enqueued_ms) VALUES (?, ?)`,
			d.ID, d.At.UnixMilli()); err != nil {
			return fmt.Errorf("attention: queue digest item: %w", err)
		}
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
