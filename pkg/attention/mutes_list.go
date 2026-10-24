package attention

import (
	"context"
	"fmt"
	"time"
)

// MuteInfo is one active mute as the launcher lists it.
type MuteInfo struct {
	// Key is the stored key: a trigger/dedup prefix, or "=" + a dedup key for an
	// exact mute (a snooze, or the [Mute] button).
	Key     string
	Exact   bool
	Until   time.Time
	Created time.Time
	Reason  string
}

// maxListedMutes bounds the list.
const maxListedMutes = 200

// MuteLister is implemented by a gate that can list its active mutes.
type MuteLister interface {
	ListMutes(ctx context.Context) ([]MuteInfo, error)
}

var _ MuteLister = (*Governor)(nil)

// ListMutes returns the mutes that are still in force, soonest to expire first. Mutes
// live in the database only (attention keeps none in memory), and a decision removes
// an expired one, so an expired row that is still stored is not listed.
func (g *Governor) ListMutes(ctx context.Context) ([]MuteInfo, error) {
	rows, err := g.db.QueryContext(ctx,
		`SELECT key, exact, until_ms, created_ms, reason FROM attention_mutes
		  WHERE until_ms > ? ORDER BY until_ms, key LIMIT ?`, g.clock().UnixMilli(), maxListedMutes)
	if err != nil {
		return nil, fmt.Errorf("attention: list mutes: %w", err)
	}
	defer rows.Close()
	var out []MuteInfo
	for rows.Next() {
		var (
			m                 MuteInfo
			exact             int
			untilMS, createMS int64
		)
		if err := rows.Scan(&m.Key, &exact, &untilMS, &createMS, &m.Reason); err != nil {
			return nil, fmt.Errorf("attention: list mutes: %w", err)
		}
		m.Exact, m.Until, m.Created = exact == 1, time.UnixMilli(untilMS).UTC(), time.UnixMilli(createMS).UTC()
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("attention: list mutes: %w", err)
	}
	return out, nil
}

// ListMutes lists the active mutes. It returns ErrNoMutes when the outbox's gate cannot.
func (o *Outbox) ListMutes(ctx context.Context) ([]MuteInfo, error) {
	l, ok := o.g.(MuteLister)
	if !ok {
		return nil, ErrNoMutes
	}
	return l.ListMutes(ctx)
}
