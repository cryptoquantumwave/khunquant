package attention

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// MaxRecentDecisions bounds one DecisionsSince read.
const MaxRecentDecisions = 500

// ErrNoDecisionLog is returned by Outbox.DecisionsSince when the outbox's gate
// keeps no log of its decisions (a test fake).
var ErrNoDecisionLog = errors.New("attention: this gate keeps no decision log")

// RecordedDecision is what the decision log holds about one candidate the governor
// has decided, without the candidate's text: the keys and the numbers the heartbeat
// sweep needs to know which topics have already been decided.
type RecordedDecision struct {
	At          time.Time
	TriggerID   string
	DedupKey    string
	BaseAsset   string // upper case; "" when the candidate has no base asset
	Severity    proactive.Severity
	Disposition proactive.Disposition
	Reason      string
}

// DecisionReader is implemented by a gate that can read its decision log.
type DecisionReader interface {
	DecisionsSince(ctx context.Context, since time.Time, excludeTriggerID string, limit int) ([]RecordedDecision, error)
}

var _ DecisionReader = (*Governor)(nil)

// DecisionsSince returns the decisions recorded at or after since, newest first, at
// most limit (and never more than MaxRecentDecisions), leaving out the rows of
// shadow mode (a candidate that was only logged, and told the owner nothing) and
// the rows whose trigger ID is excludeTriggerID when that is not empty.
//
// What remains is every topic about which the governor really decided: the owner
// was told, will be told in the digest, or had it dropped as a repeat, muted or
// inhibited. It is one read; no transaction is held after it returns.
func (g *Governor) DecisionsSince(
	ctx context.Context, since time.Time, excludeTriggerID string, limit int,
) ([]RecordedDecision, error) {
	if limit <= 0 || limit > MaxRecentDecisions {
		limit = MaxRecentDecisions
	}
	rows, err := g.db.QueryContext(ctx,
		`SELECT at_ms, trigger_id, dedup_key, COALESCE(base_asset, ''), severity, disposition, reason
		   FROM attention_decisions
		  WHERE at_ms >= ? AND shadow = 0 AND trigger_id <> ?
		  ORDER BY at_ms DESC, id DESC
		  LIMIT ?`,
		since.UnixMilli(), excludeTriggerID, limit)
	if err != nil {
		return nil, fmt.Errorf("attention: read decisions: %w", err)
	}
	defer rows.Close()

	var out []RecordedDecision
	for rows.Next() {
		var r RecordedDecision
		var atMs int64
		var sev int
		var disp string
		if err := rows.Scan(&atMs, &r.TriggerID, &r.DedupKey, &r.BaseAsset, &sev, &disp, &r.Reason); err != nil {
			return nil, fmt.Errorf("attention: read decisions: %w", err)
		}
		r.At, r.Severity, r.Disposition = time.UnixMilli(atMs), proactive.Severity(sev), proactive.Disposition(disp)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("attention: read decisions: %w", err)
	}
	return out, nil
}

// DecisionsSince reads the governor's decision log (see Governor.DecisionsSince).
// It returns ErrNoDecisionLog when the outbox's gate cannot.
func (o *Outbox) DecisionsSince(
	ctx context.Context, since time.Time, excludeTriggerID string, limit int,
) ([]RecordedDecision, error) {
	r, ok := o.g.(DecisionReader)
	if !ok {
		return nil, ErrNoDecisionLog
	}
	return r.DecisionsSince(ctx, since, excludeTriggerID, limit)
}
