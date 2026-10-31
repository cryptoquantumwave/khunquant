// Package reflection is the assistant's nightly review of itself. Once a day it looks at
// what it sent the owner, what became of it and what the owner said, and PROPOSES
// adjustments ("you dismissed 4 of 4 SET move alerts; raise the floor to 5%?"). It never
// edits anything: every change is an approval.Proposal the owner decides in the next
// morning's digest, and an approved one is applied by code (Applier) through the
// triggers, intent and attention stores.
//
// What the model does is call propose_reflection once. Everything it says is checked in
// code (Validate) against the data it was shown: a target must exist, a change must lie
// inside the bounds its trigger kind declares, evidence must be decisions of today, and
// an invalid item is dropped and counted. Of the six item types the design sketches only
// four exist here (threshold_change, intent_cleanup, memory_fact, mute_trigger);
// new_watch and profile_update would let one tap create an active watch or change the
// profile, and are left to a later change.
//
// The package cannot place an order, propose one or create a mandate: it does not import
// the order side of approval, and TestReflection_CannotProposeAnOrder keeps it so.
package reflection

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex"
)

// MinInterventions is the least number of messages that reached the owner in a day for
// the day to be worth a model call when the owner also said nothing.
const MinInterventions = 3

// DefaultAtLocal is when the reflection runs, in the owner's time zone.
const DefaultAtLocal = "21:00"

// Outcomes of a day, as stored and logged.
const (
	OutcomeDone   = "done"
	OutcomeQuiet  = "quiet"  // fewer than MinInterventions and the owner said nothing: no model call
	OutcomeBudget = "budget" // the cognition budget is spent: skipped for the day
	OutcomeFailed = "failed"
)

var migrations = []sqlitex.Migration{{
	Component: "reflection",
	Version:   1,
	SQL: `CREATE TABLE reflection_runs (
	day         TEXT    PRIMARY KEY,
	ran_at_ms   INTEGER NOT NULL,
	outcome     TEXT    NOT NULL,
	valid       INTEGER NOT NULL DEFAULT 0,
	dropped     INTEGER NOT NULL DEFAULT 0,
	fingerprint TEXT    NOT NULL DEFAULT ''
)`,
}}

type (
	// Outcome is what the turn declared.
	TurnOutcome struct {
		Items  []proactive.ReflectionItem
		Called bool
	}
	// Turn runs the reflection's model turn (agent.AgentLoop.RunReflectionTurn). It
	// returns an error wrapping proactive.ErrTurnSkipped when the budget is spent.
	Turn interface {
		RunReflectionTurn(ctx context.Context, input string) (TurnOutcome, error)
	}
	// Proposer makes the proposals (*approval.Service).
	Proposer interface {
		ProposeReflection(ctx context.Context, r approval.ReflectionRequest) (approval.Proposal, error)
		ProposeMemory(ctx context.Context, r approval.MemoryRequest) (approval.Proposal, error)
	}
	// Clock is the time source; tests use a fake.
	Clock interface {
		Now() time.Time
		After(d time.Duration) <-chan time.Time
	}
)

// Options are the dependencies of a Service.
type Options struct {
	DB       *sql.DB
	Sources  Sources
	Turn     Turn
	Proposer Proposer
	// AtLocal is "HH:MM" in the owner's zone; empty is DefaultAtLocal.
	AtLocal string
	// Zone is the owner's time zone; nil or a nil result is UTC.
	Zone  func() *time.Location
	Clock Clock
}

// Service runs the daily reflection.
type Service struct {
	o  Options
	at int // minutes after local midnight
}

type realClock struct{}

//simclock:wall the wall clock itself
func (realClock) Now() time.Time { return time.Now() }

//simclock:wall the wall clock itself
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// New builds a Service and migrates its table.
func New(ctx context.Context, o Options) (*Service, error) {
	switch {
	case o.DB == nil:
		return nil, errors.New("reflection: nil database")
	case o.Turn == nil || o.Proposer == nil:
		return nil, errors.New("reflection: a turn runner and a proposer are required")
	}
	if err := sqlitex.Migrate(ctx, o.DB, migrations...); err != nil {
		return nil, fmt.Errorf("reflection: migrate: %w", err)
	}
	if o.Clock == nil {
		o.Clock = realClock{}
	}
	if o.Zone == nil {
		o.Zone = func() *time.Location { return time.UTC }
	}
	at := strings.TrimSpace(o.AtLocal)
	if at == "" {
		at = DefaultAtLocal
	}
	var h, m int
	if _, err := fmt.Sscanf(at, "%d:%d", &h, &m); err != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return nil, fmt.Errorf("reflection: at_local %q must be HH:MM", o.AtLocal)
	}
	return &Service{o: o, at: h*60 + m}, nil
}

func (s *Service) zone() *time.Location {
	if l := s.o.Zone(); l != nil {
		return l
	}
	return time.UTC
}

func dayKey(t time.Time) string { return t.Format("2006-01-02") }

// atOn returns the reflection time on the local day of t.
func (s *Service) atOn(t time.Time) time.Time {
	y, mo, d := t.Date()
	return time.Date(y, mo, d, s.at/60, s.at%60, 0, 0, t.Location())
}

// state reads today's row (if any) and the fingerprint of the last completed run.
func (s *Service) state(ctx context.Context, day string) (ran bool, fp string, err error) {
	var n int
	if err = s.o.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM reflection_runs WHERE day = ?`, day).Scan(&n); err != nil {
		return false, "", err
	}
	err = s.o.DB.QueryRowContext(ctx,
		`SELECT fingerprint FROM reflection_runs WHERE fingerprint <> '' ORDER BY day DESC LIMIT 1`).Scan(&fp)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return n > 0, fp, err
}

func (s *Service) record(ctx context.Context, day string, at time.Time, outcome string, valid, dropped int, fp string) {
	_, err := s.o.DB.ExecContext(ctx,
		`INSERT OR REPLACE INTO reflection_runs (day, ran_at_ms, outcome, valid, dropped, fingerprint) VALUES (?, ?, ?, ?, ?, ?)`,
		day, at.UnixMilli(), outcome, valid, dropped, fp)
	if err != nil {
		logger.WarnCF("reflection", "Could not record the day's reflection", map[string]any{"error": err.Error()})
	}
}

// Result is what one run did.
type Result struct {
	Day           string
	Outcome       string
	Interventions int
	Valid         int // items that passed validation
	Dropped       int // items that did not
	Proposed      int // proposals created
	Refused       int // valid items the approval bounds or a failure kept from becoming proposals
	Sections      map[string]int
}

// RunDay reflects on the owner-local day that now is in, once: a day that already has a
// row is not run again (a reload after the hour does not repeat it). It returns an error
// only when it could not even read the stores; every other end is an Outcome.
func (s *Service) RunDay(ctx context.Context) (Result, error) {
	now := s.o.Clock.Now()
	loc := s.zone()
	local := now.In(loc)
	day := dayKey(local)
	res := Result{Day: day}
	ran, lastFP, err := s.state(ctx, day)
	if err != nil {
		return res, fmt.Errorf("reflection: state: %w", err)
	}
	if ran {
		res.Outcome = "already_ran"
		return res, nil
	}
	from := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	in, err := BuildInput(ctx, s.o.Sources, from, now, loc, lastFP)
	if err != nil {
		return res, err
	}
	res.Interventions, res.Sections = in.Interventions, in.Sections
	finish := func(outcome string, fp string) (Result, error) {
		res.Outcome = outcome
		s.record(ctx, day, now, outcome, res.Valid, res.Dropped, fp)
		logger.InfoCF("reflection", "Reflection finished", map[string]any{"day": day, "outcome": outcome,
			"interventions": res.Interventions, "valid": res.Valid, "dropped": res.Dropped, "proposed": res.Proposed, "refused": res.Refused})
		return res, nil
	}
	if in.Interventions < MinInterventions && !in.OwnerSpoke {
		return finish(OutcomeQuiet, lastFP)
	}
	out, err := s.o.Turn.RunReflectionTurn(ctx, in.Text)
	switch {
	case errors.Is(err, proactive.ErrTurnSkipped):
		return finish(OutcomeBudget, lastFP)
	case err != nil:
		logger.WarnCF("reflection", "The reflection turn failed", map[string]any{"error": err.Error()})
		return finish(OutcomeFailed, lastFP)
	}
	verdict := Validate(out.Items, in.World)
	res.Valid, res.Dropped = len(verdict.Valid), verdict.Dropped
	if !out.Called {
		logger.WarnCF("reflection", "The reflection turn did not call propose_reflection", nil)
	}
	for reason, n := range verdict.Reasons {
		logger.InfoCF("reflection", "Reflection items dropped", map[string]any{"reason": reason, "count": n})
	}
	for _, c := range verdict.Valid {
		var perr error
		if c.Memory != nil {
			_, perr = s.o.Proposer.ProposeMemory(ctx, *c.Memory)
		} else {
			_, perr = s.o.Proposer.ProposeReflection(ctx, c.Request)
		}
		if perr != nil {
			res.Refused++
			logger.InfoCF("reflection", "A valid reflection item became no proposal", map[string]any{"error": perr.Error()})
			continue
		}
		res.Proposed++
	}
	return finish(OutcomeDone, in.Fingerprint)
}

// nextWait is how long until the next run is due; zero or less means now.
func (s *Service) nextWait(ctx context.Context) time.Duration {
	now := s.o.Clock.Now().In(s.zone())
	due := s.atOn(now)
	if ran, _, err := s.state(ctx, dayKey(now)); err == nil && ran {
		due = s.atOn(now.AddDate(0, 0, 1))
	}
	return due.Sub(now)
}

// Run runs the reflection every day at the configured time until ctx ends. A run due
// when the process starts (it is past the hour and today has no row) happens at once.
func (s *Service) Run(ctx context.Context) {
	for {
		wait := s.nextWait(ctx)
		if wait <= 0 {
			if _, err := s.RunDay(ctx); err != nil {
				logger.WarnCF("reflection", "Reflection could not run", map[string]any{"error": err.Error()})
			}
			wait = time.Minute // today has a row now; look again in a minute
		}
		select {
		case <-ctx.Done():
			return
		case <-s.o.Clock.After(min(wait, time.Hour)):
		}
	}
}
