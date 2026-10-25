package cognition

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex"
)

const (
	dayLayout    = "2006-01-02"
	accountLimit = 5 * time.Second
)

var migrations = []sqlitex.Migration{{
	Component: "cognition",
	Version:   1,
	SQL: `CREATE TABLE cognition_usage (
	day           TEXT    NOT NULL,
	kind          TEXT    NOT NULL,
	wakes         INTEGER NOT NULL DEFAULT 0,
	input_tokens  INTEGER NOT NULL DEFAULT 0,
	output_tokens INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (day, kind)
)`,
}}

// BudgetUse is what the Budgeted jobs of one kind spent on one day.
type BudgetUse struct {
	Wakes        int
	InputTokens  int
	OutputTokens int
}

// budgetStore enforces WakesPerHour and TokensPerDay over every Budgeted kind
// together. The token total is persisted per (day, kind), so it survives a
// restart; the rolling hour of wakes is kept in memory and starts empty after a
// restart. A limit of 0 (or less) allows no budgeted work at all: the owner who
// sets it is asking for no background model spend, and reading it as "unlimited"
// would fail open on cost.
type budgetStore struct {
	db    *sql.DB
	cfg   config.CognitionConfig
	clock Clock
	zone  func() *time.Location

	mu     sync.Mutex
	wakes  []time.Time       // wake times within the last hour
	logged map[string]string // kind -> day the exhaustion was last logged
}

func newBudgetStore(ctx context.Context, db *sql.DB, cfg config.CognitionConfig, clock Clock,
	zone func() *time.Location,
) (*budgetStore, error) {
	if db == nil {
		return nil, errors.New("cognition: nil database")
	}
	if err := sqlitex.Migrate(ctx, db, migrations...); err != nil {
		return nil, fmt.Errorf("cognition: migrate: %w", err)
	}
	return &budgetStore{db: db, cfg: cfg, clock: clock, zone: zone, logged: map[string]string{}}, nil
}

// dayKey is the owner-local calendar day of t.
func (b *budgetStore) dayKey(t time.Time) string {
	loc := b.zone()
	if loc == nil {
		loc = time.UTC
	}
	return t.In(loc).Format(dayLayout)
}

// check decides whether a Budgeted job of kind may run now. On success it
// records the wake and returns the day to charge the job to; otherwise it
// returns the Fallback reason. It fails closed: a budget that cannot be read is
// treated as spent.
func (b *budgetStore) check(ctx context.Context, kind proactive.TurnKind) (day, reason string) {
	now := b.clock.Now()
	day = b.dayKey(now)

	b.mu.Lock()
	defer b.mu.Unlock()
	b.pruneLocked(now)
	if len(b.wakes) >= max(b.cfg.WakesPerHour, 0) {
		b.logExhaustedLocked(kind, day, "wakes_per_hour")
		return day, ReasonBudgetExhausted
	}
	limit := max(b.cfg.TokensPerDay, 0)
	if limit == 0 {
		b.logExhaustedLocked(kind, day, "tokens_per_day")
		return day, ReasonBudgetExhausted
	}
	used, err := b.dayTokens(ctx, day)
	if err != nil {
		if ctx.Err() == nil { // else cancelled mid-check: the caller reports that
			logger.WarnCF("cognition", "budget unreadable, treating as exhausted",
				map[string]any{"kind": string(kind), "error": err.Error()})
		}
		return day, ReasonBudgetExhausted
	}
	if used >= limit {
		b.logExhaustedLocked(kind, day, "tokens_per_day")
		return day, ReasonBudgetExhausted
	}
	b.wakes = append(b.wakes, now)
	return day, ""
}

func (b *budgetStore) pruneLocked(now time.Time) {
	cut := now.Add(-time.Hour)
	i := 0
	for i < len(b.wakes) && !b.wakes[i].After(cut) {
		i++
	}
	b.wakes = b.wakes[i:]
}

// logExhaustedLocked logs budget_exhausted once per day per kind.
func (b *budgetStore) logExhaustedLocked(kind proactive.TurnKind, day, limit string) {
	if b.logged[string(kind)] == day {
		return
	}
	b.logged[string(kind)] = day
	logger.InfoCF("cognition", ReasonBudgetExhausted,
		map[string]any{"kind": string(kind), "day": day, "limit": limit})
}

func (b *budgetStore) dayTokens(ctx context.Context, day string) (int, error) {
	var n int
	err := b.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(input_tokens + output_tokens), 0) FROM cognition_usage WHERE day = ?`,
		day).Scan(&n)
	return n, err
}

// add charges one finished job to (day, kind). It must survive a cancelled
// job context: the tokens were spent either way.
func (b *budgetStore) add(day string, kind proactive.TurnKind, u Usage) {
	ctx, cancel := context.WithTimeout(context.Background(), accountLimit)
	defer cancel()
	_, err := b.db.ExecContext(ctx,
		`INSERT INTO cognition_usage (day, kind, wakes, input_tokens, output_tokens) VALUES (?, ?, 1, ?, ?)
		 ON CONFLICT (day, kind) DO UPDATE SET
		   wakes = wakes + 1,
		   input_tokens = input_tokens + excluded.input_tokens,
		   output_tokens = output_tokens + excluded.output_tokens`,
		day, string(kind), max(u.InputTokens, 0), max(u.OutputTokens, 0))
	if err != nil {
		logger.WarnCF("cognition", "recording usage failed",
			map[string]any{"kind": string(kind), "day": day, "error": err.Error()})
	}
}

// snapshot reports today's day, the wakes of the last hour and today's use by
// kind.
func (b *budgetStore) snapshot() (day string, wakes int, use map[string]BudgetUse) {
	now := b.clock.Now()
	day = b.dayKey(now)
	use = map[string]BudgetUse{}

	b.mu.Lock()
	b.pruneLocked(now)
	wakes = len(b.wakes)
	b.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), accountLimit)
	defer cancel()
	rows, err := b.db.QueryContext(ctx,
		`SELECT kind, wakes, input_tokens, output_tokens FROM cognition_usage WHERE day = ?`, day)
	if err != nil {
		logger.DebugCF("cognition", "stats read failed", map[string]any{"error": err.Error()})
		return day, wakes, use
	}
	defer rows.Close()
	for rows.Next() {
		var kind string
		var u BudgetUse
		if err := rows.Scan(&kind, &u.Wakes, &u.InputTokens, &u.OutputTokens); err != nil {
			break
		}
		use[kind] = u
	}
	return day, wakes, use
}
