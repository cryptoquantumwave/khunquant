package attention

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrNoMutes is returned by the Outbox's mute methods when its gate keeps no mutes
// (a test fake).
var ErrNoMutes = errors.New("attention: this gate keeps no mutes")

// maxInterventionKeys bounds how many dedup keys one message can answer with: a
// digest carries many decisions, and snoozing it must not mute an unbounded list.
const maxInterventionKeys = 20

// InterventionLog is implemented by a gate that can say what a delivered message
// was about.
type InterventionLog interface {
	InterventionKeys(ctx context.Context, interventionID string) ([]string, error)
}

var _ InterventionLog = (*Governor)(nil)

// ExactMuter is implemented by a gate that can mute one dedup key and nothing longer.
type ExactMuter interface {
	MuteExact(ctx context.Context, dedupKey string, until time.Time, reason string) error
}

var _ ExactMuter = (*Governor)(nil)

// InterventionKeys returns the distinct dedup keys of the decisions that were
// delivered in the message interventionID names (decisions delivered together, a
// digest or a group, share one), oldest first and at most maxInterventionKeys. A
// decision delivered on its own is its own message: its decision ID is the
// intervention ID. An unknown ID answers an empty list.
//
// It is how a reply to a proactive message ("/snooze") is mapped back to the
// topic to mute: notify maps the platform message ID to the intervention ID, and
// this maps that to the decision log.
func (g *Governor) InterventionKeys(ctx context.Context, interventionID string) ([]string, error) {
	if interventionID == "" {
		return nil, nil
	}
	rows, err := g.db.QueryContext(ctx,
		`SELECT dedup_key FROM attention_decisions
		  WHERE (intervention_id = ? OR id = ?) AND delivered = 1 AND shadow = 0 AND dedup_key <> ''
		  GROUP BY dedup_key
		  ORDER BY MIN(at_ms), dedup_key
		  LIMIT ?`,
		interventionID, interventionID, maxInterventionKeys)
	if err != nil {
		return nil, fmt.Errorf("attention: read intervention: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, fmt.Errorf("attention: read intervention: %w", err)
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("attention: read intervention: %w", err)
	}
	return out, nil
}

// Mute silences keyPrefix until the given time (see Governor.Mute). It returns
// ErrNoMutes when the outbox's gate cannot.
func (o *Outbox) Mute(ctx context.Context, keyPrefix string, until time.Time, reason string) error {
	m, ok := o.g.(Muter)
	if !ok {
		return ErrNoMutes
	}
	return m.Mute(ctx, keyPrefix, until, reason)
}

// Unmute removes the mute with exactly this prefix (see Governor.Unmute). It
// returns ErrNoMutes when the outbox's gate cannot.
func (o *Outbox) Unmute(ctx context.Context, keyPrefix string) error {
	m, ok := o.g.(Muter)
	if !ok {
		return ErrNoMutes
	}
	return m.Unmute(ctx, keyPrefix)
}

// MuteExact silences exactly one dedup key (see Governor.MuteExact). It returns
// ErrNoMutes when the outbox's gate cannot.
func (o *Outbox) MuteExact(ctx context.Context, dedupKey string, until time.Time, reason string) error {
	m, ok := o.g.(ExactMuter)
	if !ok {
		return ErrNoMutes
	}
	return m.MuteExact(ctx, dedupKey, until, reason)
}

// InterventionKeys reads the decision log (see Governor.InterventionKeys). It
// returns ErrNoDecisionLog when the outbox's gate cannot.
func (o *Outbox) InterventionKeys(ctx context.Context, interventionID string) ([]string, error) {
	r, ok := o.g.(InterventionLog)
	if !ok {
		return nil, ErrNoDecisionLog
	}
	return r.InterventionKeys(ctx, interventionID)
}
