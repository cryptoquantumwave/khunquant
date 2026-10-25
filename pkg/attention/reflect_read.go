package attention

import (
	"context"
	"fmt"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// DayDecision is one row of the decision log as the nightly reflection reads it: the id
// it cites as evidence, the keys, the numbers and what became of it. Delivered is the
// only authority on whether the owner was told (see the note on attention_decisions).
type DayDecision struct {
	ID          string
	At          time.Time
	TriggerID   string
	DedupKey    string
	Severity    proactive.Severity
	Disposition proactive.Disposition
	Reason      string
	Delivered   bool
}

// MuteEvent is a mute or snooze created in a window.
type MuteEvent struct {
	Key     string // the prefix, or the exact dedup key of a snooze
	Exact   bool
	Created time.Time
	Until   time.Time
	Reason  string
}

// DayReader is implemented by a gate that can read its decision log for a day.
type DayReader interface {
	DecisionsBetween(ctx context.Context, from, to time.Time, limit int) ([]DayDecision, error)
	MuteEventsBetween(ctx context.Context, from, to time.Time) ([]MuteEvent, error)
}

var _ DayReader = (*Governor)(nil)

// DecisionsBetween returns the decisions recorded in [from, to), oldest first, at most
// limit (never more than MaxRecentDecisions), without the rows of shadow mode.
func (g *Governor) DecisionsBetween(ctx context.Context, from, to time.Time, limit int) ([]DayDecision, error) {
	if limit <= 0 || limit > MaxRecentDecisions {
		limit = MaxRecentDecisions
	}
	rows, err := g.db.QueryContext(ctx,
		`SELECT id, at_ms, trigger_id, dedup_key, severity, disposition, reason, delivered
		   FROM attention_decisions
		  WHERE at_ms >= ? AND at_ms < ? AND shadow = 0
		  ORDER BY at_ms, id
		  LIMIT ?`, from.UnixMilli(), to.UnixMilli(), limit)
	if err != nil {
		return nil, fmt.Errorf("attention: read the day's decisions: %w", err)
	}
	defer rows.Close()
	var out []DayDecision
	for rows.Next() {
		var d DayDecision
		var at int64
		var sev, delivered int
		var disp string
		if err := rows.Scan(&d.ID, &at, &d.TriggerID, &d.DedupKey, &sev, &disp, &d.Reason, &delivered); err != nil {
			return nil, fmt.Errorf("attention: read the day's decisions: %w", err)
		}
		d.At, d.Severity, d.Disposition, d.Delivered = time.UnixMilli(at), proactive.Severity(sev), proactive.Disposition(disp), delivered == 1
		out = append(out, d)
	}
	return out, rows.Err()
}

// MuteEventsBetween returns the mutes and snoozes created in [from, to), oldest first.
// A mute that has since expired is gone from the table and is not reported.
func (g *Governor) MuteEventsBetween(ctx context.Context, from, to time.Time) ([]MuteEvent, error) {
	rows, err := g.db.QueryContext(ctx,
		`SELECT key, exact, created_ms, until_ms, reason FROM attention_mutes
		  WHERE created_ms >= ? AND created_ms < ? ORDER BY created_ms, key LIMIT 50`, from.UnixMilli(), to.UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("attention: read the day's mutes: %w", err)
	}
	defer rows.Close()
	var out []MuteEvent
	for rows.Next() {
		var m MuteEvent
		var exact int
		var created, until int64
		if err := rows.Scan(&m.Key, &exact, &created, &until, &m.Reason); err != nil {
			return nil, fmt.Errorf("attention: read the day's mutes: %w", err)
		}
		m.Exact, m.Created, m.Until = exact == 1, time.UnixMilli(created), time.UnixMilli(until)
		if m.Exact {
			m.Key = m.Key[1:] // stored as "=" + the dedup key
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// DecisionsBetween reads the governor's decision log (see Governor.DecisionsBetween). It
// returns ErrNoDecisionLog when the outbox's gate cannot.
func (o *Outbox) DecisionsBetween(ctx context.Context, from, to time.Time, limit int) ([]DayDecision, error) {
	r, ok := o.g.(DayReader)
	if !ok {
		return nil, ErrNoDecisionLog
	}
	return r.DecisionsBetween(ctx, from, to, limit)
}

// MuteEventsBetween reads the mutes created in a window (see Governor.MuteEventsBetween).
func (o *Outbox) MuteEventsBetween(ctx context.Context, from, to time.Time) ([]MuteEvent, error) {
	r, ok := o.g.(DayReader)
	if !ok {
		return nil, ErrNoDecisionLog
	}
	return r.MuteEventsBetween(ctx, from, to)
}
