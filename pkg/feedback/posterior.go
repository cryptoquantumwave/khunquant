package feedback

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/attention"
	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
)

// Acceptance posteriors.
//
// For each trigger kind and time of day (six 4-hour buckets in the owner's zone) the
// package keeps a Beta(alpha, beta) belief that the owner finds a message useful. It
// starts at the Beta(2, 2) prior. Each owner response adds to it once:
//
//	useful, acted_on, approved        alpha += 1
//	not_useful, mute, rejected        beta  += 1
//	snooze                            beta  += 0.5
//	none (no answer in 24 hours)      beta  += 0.2
//
// (reply, reaction and expired add nothing.) Both parameters decay toward the prior with
// a 30-day half-life, applied lazily from the row's updated_at with the injected clock,
// so a bucket that stops getting evidence drifts back to 0.5 and stops demoting. Burst
// fatigue is the attention budget's job, not this one's.
//
// Which message counts. A response teaches the posterior only when it is to a message
// that interrupted the owner: an intervention with a notify or escalate decision. A
// response to a digest says nothing about whether the item deserved an interruption (and
// every digest gets a "none" a day). When one message carries decisions of several kinds
// or buckets, each distinct (kind, bucket) takes the response once. The kind is the
// trigger kind of the stored decision (attention.KindOf) and the bucket is that of the
// decision's time in the owner's zone, which is how attention reads it back.
//
// Exactly once. feedback_posterior_applied holds the id of every response that has been
// applied, in the same transaction as the update, so Update called twice, or a response
// replayed, changes nothing. A marker is dropped only when its response row is gone.
// responses counts the responses a posterior rests on; it does not decay, and it is what
// the evidence floor (attention.MinEvidence) reads.

// The prior, and how fast evidence fades.
const (
	priorAlpha = 2.0
	priorBeta  = 2.0
	// HalfLife is the time in which a posterior moves halfway back to the prior.
	HalfLife = 30 * 24 * time.Hour
)

// Posterior is a Beta(Alpha, Beta) belief as of UpdatedAt.
type Posterior struct {
	Alpha, Beta float64
	UpdatedAt   time.Time
	// Responses is how many responses it rests on.
	Responses int
}

// Mean is Alpha / (Alpha + Beta): the probability that the next message is found useful.
func (p Posterior) Mean() float64 { return p.Alpha / (p.Alpha + p.Beta) }

// decayedTo returns p with its distance from the prior halved for every HalfLife
// between UpdatedAt and now. A time not after UpdatedAt changes nothing.
func (p Posterior) decayedTo(now time.Time) Posterior {
	dt := now.Sub(p.UpdatedAt)
	if dt <= 0 {
		return p
	}
	k := math.Exp2(-float64(dt) / float64(HalfLife))
	p.Alpha = priorAlpha + (p.Alpha-priorAlpha)*k
	p.Beta = priorBeta + (p.Beta-priorBeta)*k
	p.UpdatedAt = now
	return p
}

// delta is what a response kind adds, and whether it counts at all.
func delta(kind string) (da, db float64, counts bool) {
	switch Kind(kind) {
	case KindUseful, KindActedOn, KindApproved:
		return 1, 0, true
	case KindNotUseful, KindMute, KindRejected:
		return 0, 1, true
	case KindSnooze:
		return 0, 0.5, true
	case KindNone:
		return 0, 0.2, true
	}
	return 0, 0, false
}

const countedKinds = `('useful','acted_on','approved','not_useful','mute','rejected','snooze','none')`

// Store keeps the posteriors. Service builds one over its database.
type Store struct {
	db    *sql.DB
	clock func() time.Time
	zone  func(context.Context) *time.Location
	pub   Publisher
}

// Posteriors returns the posterior store of the service. Safe on nil: it returns nil, and
// a nil *Store answers every question with the prior.
func (s *Service) Posteriors() *Store {
	if s == nil {
		return nil
	}
	return s.post
}

func prior(now time.Time) Posterior {
	return Posterior{Alpha: priorAlpha, Beta: priorBeta, UpdatedAt: now}
}

// Posterior returns the posterior for a trigger kind and hour bucket as of now: the
// prior when nothing was recorded.
func (s *Store) Posterior(ctx context.Context, kind string, hourBucket int) (Posterior, error) {
	if s == nil {
		//simclock:wall a nil store has no clock
		return prior(time.Now()), nil
	}
	return readPosterior(ctx, s.db, kind, hourBucket, s.clock())
}

func readPosterior(ctx context.Context, q attention.Queryer, kind string, bucket int, now time.Time) (Posterior, error) {
	var p Posterior
	var ms int64
	err := q.QueryRowContext(ctx,
		`SELECT alpha, beta, responses, updated_at_ms FROM feedback_posteriors WHERE kind = ? AND bucket = ?`,
		kind, bucket).Scan(&p.Alpha, &p.Beta, &p.Responses, &ms)
	if errors.Is(err, sql.ErrNoRows) {
		return prior(now), nil
	}
	if err != nil {
		return Posterior{}, fmt.Errorf("feedback: read posterior: %w", err)
	}
	p.UpdatedAt = time.UnixMilli(ms)
	return p.decayedTo(now), nil
}

// Acceptance implements attention.AcceptanceSource: the mean and the response count of
// the posterior as of now, read through q (the governor's own transaction).
func (s *Store) Acceptance(ctx context.Context, q attention.Queryer, kind string, bucket int, now time.Time) (float64, int, error) {
	if s == nil {
		return 0.5, 0, nil
	}
	p, err := readPosterior(ctx, q, kind, bucket, now)
	if err != nil {
		return 0.5, 0, err
	}
	return p.Mean(), p.Responses, nil
}

var _ attention.AcceptanceSource = (*Store)(nil)

type target struct {
	kind   string
	bucket int
}

// Update applies, once each, the responses recorded for interventionID that have not been
// applied yet. It is safe to call again and from several goroutines.
func (s *Store) Update(ctx context.Context, interventionID string) error {
	if s == nil {
		return nil
	}
	interventionID = strings.TrimSpace(interventionID)
	if interventionID == "" {
		return errors.New("feedback: update needs an intervention")
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dbTimeout)
	defer cancel()
	loc := time.UTC
	if s.zone != nil {
		if l := s.zone(ctx); l != nil {
			loc = l
		}
	}
	now := s.clock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("feedback: update: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	ids, da, db, n, err := unapplied(ctx, tx, interventionID)
	if err != nil || len(ids) == 0 {
		return err
	}
	targets, err := targetsOf(ctx, tx, interventionID, loc)
	if err != nil {
		return err
	}
	var updated []Posterior
	if da != 0 || db != 0 {
		for _, t := range targets {
			p, err := readPosterior(ctx, tx, t.kind, t.bucket, now)
			if err != nil {
				return err
			}
			p.Alpha += da
			p.Beta += db
			p.Responses += n
			p.UpdatedAt = now
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO feedback_posteriors (kind, bucket, alpha, beta, responses, updated_at_ms) VALUES (?, ?, ?, ?, ?, ?)
				 ON CONFLICT(kind, bucket) DO UPDATE SET alpha = excluded.alpha, beta = excluded.beta,
				   responses = excluded.responses, updated_at_ms = excluded.updated_at_ms`,
				t.kind, t.bucket, p.Alpha, p.Beta, p.Responses, now.UnixMilli()); err != nil {
				return fmt.Errorf("feedback: write posterior: %w", err)
			}
			updated = append(updated, p)
		}
	}
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO feedback_posterior_applied (response_id, applied_at_ms) VALUES (?, ?) ON CONFLICT(response_id) DO NOTHING`,
			id, now.UnixMilli()); err != nil {
			return fmt.Errorf("feedback: mark applied: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("feedback: update: %w", err)
	}
	for i, p := range updated {
		s.publish(targets[i], p)
	}
	return nil
}

// unapplied returns the counted responses of the intervention that have no marker, and
// what together they add.
func unapplied(ctx context.Context, tx *sql.Tx, interventionID string) (ids []string, da, db float64, n int, err error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT f.id, f.kind FROM feedback_responses f
		  WHERE f.intervention_id = ? AND f.kind IN `+countedKinds+`
		    AND NOT EXISTS (SELECT 1 FROM feedback_posterior_applied a WHERE a.response_id = f.id)
		  ORDER BY f.at_ms, f.id`, interventionID)
	if err != nil {
		return nil, 0, 0, 0, fmt.Errorf("feedback: read responses: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, kind string
		if err := rows.Scan(&id, &kind); err != nil {
			return nil, 0, 0, 0, fmt.Errorf("feedback: read responses: %w", err)
		}
		a, b, ok := delta(kind)
		if !ok {
			continue
		}
		ids = append(ids, id)
		da, db, n = da+a, db+b, n+1
	}
	return ids, da, db, n, rows.Err()
}

// targetsOf is the distinct (kind, bucket) pairs of the interrupting decisions of the
// intervention. An intervention that is a message of its own is named by its decision id.
func targetsOf(ctx context.Context, tx *sql.Tx, interventionID string, loc *time.Location) ([]target, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT trigger_id, dedup_key, at_ms FROM attention_decisions
		  WHERE (intervention_id = ? OR (intervention_id IS NULL AND id = ?)) AND disposition IN ('notify','escalate')
		  ORDER BY at_ms, id`, interventionID, interventionID)
	if err != nil {
		return nil, fmt.Errorf("feedback: read decisions: %w", err)
	}
	defer rows.Close()
	seen := map[target]bool{}
	var out []target
	for rows.Next() {
		var trig, key string
		var ms int64
		if err := rows.Scan(&trig, &key, &ms); err != nil {
			return nil, fmt.Errorf("feedback: read decisions: %w", err)
		}
		k := attention.KindOf(trig, key)
		if k == "" {
			continue
		}
		t := target{kind: k, bucket: attention.HourBucket(time.UnixMilli(ms).In(loc))}
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out, rows.Err()
}

func (s *Store) publish(t target, p Posterior) {
	if s.pub == nil {
		return
	}
	s.pub.PublishNonBlocking(events.Event{
		Kind:     events.KindFeedbackPosterior,
		Source:   events.Source{Component: Component},
		Severity: events.SeverityInfo,
		Payload: map[string]any{
			"kind": t.kind, "bucket": t.bucket, "mean": math.Round(p.Mean()*100) / 100,
		},
	})
}

// dropOrphanMarkers deletes the markers whose response row has been pruned.
func dropOrphanMarkers(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx,
		`DELETE FROM feedback_posterior_applied WHERE response_id NOT IN (SELECT id FROM feedback_responses)`); err != nil {
		logger.DebugCF("feedback", "Could not drop old posterior markers", map[string]any{"error": err.Error()})
		return err
	}
	return nil
}
