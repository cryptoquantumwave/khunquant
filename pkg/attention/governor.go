// Package attention is the governor of the proactive agent's interruptions: one
// deterministic component that decides, for every candidate, whether the owner
// hears about it now, later in a digest, or never, and whether the LLM must be
// woken to compose it.
//
// The rule table lives in rules.go. This file wires it to the clock, the
// owner's profile and the database. Every decision except mode "off" is
// recorded in attention_decisions: silence is an output.
//
// Dependency rule (A.4): this package imports only pkg/proactive, pkg/intent,
// pkg/config, pkg/sqlitex and pkg/logger. It does not know how a message is
// delivered; callers report the outcome through MarkDelivered.
package attention

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex"
)

const (
	modeOff    = config.ProactiveModeOff
	modeShadow = config.ProactiveModeShadow
	modeLive   = config.ProactiveModeLive

	// defaultDedupWindow applies when neither the candidate nor the config
	// names a window.
	defaultDedupWindow = 24 * time.Hour

	// pendingLease bounds how long a notify/escalate decision that has not been
	// reported on keeps reserving its budget slot and dedup key. A caller that
	// crashes between Decide and MarkDelivered must not hold them forever.
	pendingLease = 10 * time.Minute

	// decisionRetention and pendingDigestKeep drive pruning; see prune.
	decisionRetention = 180 * 24 * time.Hour
	pendingDigestKeep = 7 * 24 * time.Hour
	pruneEvery        = 24 * time.Hour

	// Shadow rows are coalesced within shadowCoalesce and pruned after
	// shadowRetention: shadow-mode triggers fire every minute and a small device
	// cannot keep a row for each.
	shadowCoalesce  = 60 * time.Minute
	shadowRetention = 30 * 24 * time.Hour

	// profileWarnEvery rate-limits the "profile source failed" WARN: a failing
	// source and a candidate firing every minute must not flood the log.
	profileWarnEvery = 10 * time.Minute

	maxMuteReasonRunes = 200
	minMutePrefixRunes = 2
)

// ProfileSource supplies the owner's profile. *intent.Store satisfies it.
type ProfileSource interface {
	Profile(ctx context.Context) (intent.Profile, error)
}

// Decision is the governor's verdict on one candidate.
type Decision struct {
	ID          string // ULID (proactive.NewID); becomes the InterventionID
	Disposition proactive.Disposition
	Reason      string // machine-readable; see the Reason* constants
	Candidate   proactive.Candidate
	// Previous is the last delivered candidate for the same dedup key, when one
	// exists, so a re-alert can say "update: now X, previously Y".
	Previous *proactive.Candidate
	At       time.Time
}

// Delivery is the outcome of an attempt to put a Decision in front of the
// owner. It is local to this package so attention need not import pkg/notify.
type Delivery struct {
	Delivered bool
	At        time.Time // zero = now
	// InterventionID names the message the decision went out in; decisions
	// delivered together share one, and the daily budget counts distinct IDs.
	// Empty means the decision was a message of its own (its ID is used).
	InterventionID string
}

// Decider is what the pipeline needs from the governor to decide and report.
//
// Deliver notify and escalate decisions through an Outbox, not one by one: the
// budget counts in-flight decisions as the messages the outbox will make of them
// (see sendsToday), so a caller that sends every decision as a message of its
// own, concurrently, can exceed the daily budget.
type Decider interface {
	Decide(ctx context.Context, c proactive.Candidate) (Decision, error)
	MarkDelivered(ctx context.Context, d Decision, r Delivery) error
}

// Muter is what the owner-facing commands need: snooze and unsnooze.
type Muter interface {
	Mute(ctx context.Context, keyPrefix string, until time.Time, reason string) error
	Unmute(ctx context.Context, keyPrefix string) error
}

var (
	_ Decider = (*Governor)(nil)
	_ Muter   = (*Governor)(nil)
)

// tzWarnOnce makes the "unknown time zone" WARN fire once per process.
var tzWarnOnce sync.Once

// Governor decides what the owner hears. It is safe for concurrent use.
type Governor struct {
	db       *sql.DB
	profiles ProfileSource
	cfg      config.AttentionConfig
	mode     string
	clock    func() time.Time
	inhibits []InhibitRule

	// tunables; fields rather than constants so tests can exercise pruning.
	retention         time.Duration
	pendingDigestKeep time.Duration
	shadowRetention   time.Duration

	mu              sync.Mutex
	lastPrune       time.Time
	lastProfileWarn time.Time
}

// New builds a governor over db and runs the attention migrations. effectiveMode
// is cfg.Proactive.Effective().AttentionMode ("off", "shadow" or "live"): the
// governor is constructed with the effective mode, never the raw config field.
// A nil clock means time.Now.
func New(db *sql.DB, p ProfileSource, cfg config.AttentionConfig, effectiveMode string, clock func() time.Time) (*Governor, error) {
	if db == nil {
		return nil, errors.New("attention: nil database")
	}
	if p == nil {
		return nil, errors.New("attention: nil profile source")
	}
	switch effectiveMode {
	case modeOff, modeShadow, modeLive:
	default:
		return nil, fmt.Errorf("attention: unknown mode %q (want off, shadow or live)", effectiveMode)
	}
	if clock == nil {
		clock = time.Now
	}
	if err := sqlitex.Migrate(context.Background(), db, migrations...); err != nil {
		return nil, fmt.Errorf("attention: %w", err)
	}
	return &Governor{
		db:                db,
		profiles:          p,
		cfg:               cfg,
		mode:              effectiveMode,
		clock:             clock,
		inhibits:          defaultInhibits,
		retention:         decisionRetention,
		pendingDigestKeep: pendingDigestKeep,
		shadowRetention:   shadowRetention,
	}, nil
}

// profileFacts is what Decide needs from the owner's profile. It is computed
// before any transaction opens: the profile is another package's store.
type profileFacts struct {
	inActive   bool
	budget     int
	dayStartMs int64
	dayEndMs   int64
}

// loadProfile fetches the profile and reduces it to profileFacts. A failing
// source degrades to intent's defaults and cfg.DailyBudget, with a WARN: the
// governor must neither go silent for a protective Critical nor open the
// floodgates, and the default 07:00-22:00 Bangkok window keeps quiet hours.
func (g *Governor) loadProfile(ctx context.Context, now time.Time) profileFacts {
	prof, loc := g.profile(ctx, now)
	nl := now.In(loc)
	start := time.Date(nl.Year(), nl.Month(), nl.Day(), 0, 0, 0, 0, loc)
	end := time.Date(nl.Year(), nl.Month(), nl.Day()+1, 0, 0, 0, 0, loc)
	return profileFacts{
		inActive:   prof.InActiveHours(now),
		budget:     max(prof.NotificationBudgetPerDay, 0),
		dayStartMs: start.UnixMilli(),
		dayEndMs:   end.UnixMilli(),
	}
}

// profile fetches the owner's profile and its time zone. A source that fails
// degrades to intent's defaults and cfg.DailyBudget (with a rate-limited WARN),
// and a time zone that cannot be loaded to UTC (with a one-time WARN), so every
// caller falls back the same way.
func (g *Governor) profile(ctx context.Context, now time.Time) (intent.Profile, *time.Location) {
	prof, err := g.profiles.Profile(ctx)
	if err != nil {
		g.warnProfile(now, err)
		prof = intent.DefaultProfile()
		prof.NotificationBudgetPerDay = g.cfg.DailyBudget
	}
	loc, ok := prof.Location()
	if !ok {
		tzWarnOnce.Do(func() {
			logger.WarnCF("attention", "profile time zone cannot be loaded; using UTC", map[string]any{"timezone": prof.Timezone})
		})
	}
	return prof, loc
}

func (g *Governor) warnProfile(now time.Time, err error) {
	g.mu.Lock()
	due := g.lastProfileWarn.IsZero() || now.Sub(g.lastProfileWarn) >= profileWarnEvery
	if due {
		g.lastProfileWarn = now
	}
	g.mu.Unlock()
	if due {
		logger.WarnCF("attention", "profile unavailable; deciding with defaults", map[string]any{"error": err.Error()})
	}
}

// normalize validates c and fills ID and At.
func (g *Governor) normalize(c proactive.Candidate, now time.Time) (proactive.Candidate, error) {
	if c.DedupKey == "" {
		return c, errors.New("attention: candidate has no dedup key")
	}
	if c.Severity < proactive.SeverityInfo || c.Severity > proactive.SeverityCritical {
		return c, fmt.Errorf("attention: candidate severity %s out of range", c.Severity)
	}
	if c.ID == "" {
		c.ID = proactive.NewID()
	}
	if c.At.IsZero() {
		c.At = now
	}
	return c, nil
}

func (g *Governor) dedupWindow(c proactive.Candidate) time.Duration {
	if c.DedupWindow > 0 {
		return c.DedupWindow
	}
	if h := g.cfg.DedupWindowHours; h > 0 {
		return time.Duration(h) * time.Hour
	}
	return defaultDedupWindow
}

// Decide applies the rule table to c and records the outcome. Reads and the
// decision row share one transaction, so two concurrent calls cannot both pass
// the budget or dedup check. Mode "off" returns a drop and writes nothing.
func (g *Governor) Decide(ctx context.Context, c proactive.Candidate) (Decision, error) {
	now := g.clock()
	c, err := g.normalize(c, now)
	if err != nil {
		return Decision{}, err
	}
	d := Decision{ID: proactive.NewID(), Candidate: c, At: now}

	if g.mode == modeOff {
		d.Disposition, d.Reason = evaluate(c, facts{Mode: modeOff})
		return d, nil
	}

	f := facts{Mode: g.mode}
	live := g.mode == modeLive && !c.Shadow
	var pf profileFacts
	if live {
		pf = g.loadProfile(ctx, now) // before the transaction: another package's store
		f.InActiveHours = pf.inActive
		f.Budget = pf.budget
	}

	tx, err := g.db.BeginTx(ctx, nil)
	if err != nil {
		return Decision{}, fmt.Errorf("attention: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if live {
		prev, err := g.gather(ctx, tx, c, now, pf, &f)
		if err != nil {
			return Decision{}, err
		}
		d.Previous = prev
	}
	d.Disposition, d.Reason = evaluate(c, f)
	if d.Reason == ReasonShadow {
		// Coalesce: the same situation was already logged at this severity or
		// higher within the hour. Return that decision and write nothing.
		id, found, err := recentShadow(ctx, tx, c.DedupKey, c.Severity, now.Add(-shadowCoalesce).UnixMilli())
		if err != nil {
			return Decision{}, err
		}
		if found {
			d.ID = id
			return d, nil
		}
	}
	if err := insertDecision(ctx, tx, d); err != nil {
		return Decision{}, err
	}
	if err := tx.Commit(); err != nil {
		return Decision{}, fmt.Errorf("attention: commit decision: %w", err)
	}
	g.maybePrune(ctx, now)
	return d, nil
}

// gather reads everything the rules need inside tx and fills f.
func (g *Governor) gather(ctx context.Context, tx *sql.Tx, c proactive.Candidate, now time.Time, pf profileFacts, f *facts) (*proactive.Candidate, error) {
	nowMs := now.UnixMilli()
	if err := deleteExpiredMutes(ctx, tx, nowMs); err != nil {
		return nil, err
	}
	var err error
	if f.Muted, err = isMuted(ctx, tx, c, nowMs); err != nil {
		return nil, err
	}

	window := g.dedupWindow(c)
	leaseStart := now.Add(-pendingLease).UnixMilli()
	st, err := readState(ctx, tx, c.DedupKey)
	if err != nil {
		return nil, err
	}
	if st.exists && st.lastSeverity >= c.Severity && nowMs-st.lastSentMs < window.Milliseconds() {
		f.DedupHit = true
	}
	// A protective Critical is not held back by an in-flight decision: if its
	// caller crashed before reporting, the lease would silence the one alert that
	// must not wait. A duplicate liquidation warning costs less than a missed one.
	if !f.DedupHit && !isProtectiveCritical(c) {
		if f.DedupHit, err = inFlight(ctx, tx, c.DedupKey, c.Severity, leaseStart); err != nil {
			return nil, err
		}
	}
	if f.QueuedHit, err = queuedDigest(ctx, tx, c.DedupKey, c.Severity, now.Add(-window).UnixMilli()); err != nil {
		return nil, err
	}
	if f.SendsToday, f.JoinsPending, err = sendsToday(ctx, tx, pf.dayStartMs, pf.dayEndMs, leaseStart, g.groupWindow().Milliseconds(), nowMs); err != nil {
		return nil, err
	}
	if !isProtectiveCritical(c) {
		if f.InhibitedBy, err = g.inhibitedBy(ctx, tx, c, now, leaseStart); err != nil {
			return nil, err
		}
	}
	return st.previous, nil
}

// groupWindow is the outbox's grouping window, which the budget needs to count
// in-flight decisions as the messages they will become. Zero means no grouping.
func (g *Governor) groupWindow() time.Duration {
	return time.Duration(max(g.cfg.GroupWindowSeconds, 0)) * time.Second
}

// inhibitedBy returns the dedup key of the message that explains c under the
// first matching inhibit rule, or "" when none does.
func (g *Governor) inhibitedBy(ctx context.Context, tx *sql.Tx, c proactive.Candidate, now time.Time, leaseStart int64) (string, error) {
	var base string
	var haveBase bool
	for _, r := range g.inhibits {
		if r.SuppressPrefix == "" || r.WhenPrefix == "" || !strings.HasPrefix(c.DedupKey, r.SuppressPrefix) {
			continue
		}
		if r.SameBaseAsset {
			if !haveBase {
				base, haveBase = baseAsset(c), true
			}
			if base == "" {
				continue
			}
		}
		key, err := inhibitorKey(ctx, tx, r, base, now.Add(-r.For).UnixMilli(), leaseStart)
		if err != nil {
			return "", err
		}
		if key != "" {
			return key, nil
		}
	}
	return "", nil
}

// MarkDelivered reports how the delivery of d went.
//
// With Delivered=true it records the send: attention_state is upserted (so the
// situation dedups from now on) and the decision row is marked delivered, which
// is what counts against the daily budget. The decision is looked up by d.ID and
// read back from the database; the caller's copy of the candidate is not
// trusted. Repeating the call for the same decision changes nothing. Decisions
// delivered in one message should pass the same r.InterventionID: the budget
// counts distinct IDs.
//
// With Delivered=false the send failed: no state is written and the decision's
// budget and dedup reservation is released, so the situation can be retried and
// consumes nothing. (The release is recorded as delivered_at_ms on a row whose
// delivered is still 0.)
func (g *Governor) MarkDelivered(ctx context.Context, d Decision, r Delivery) error {
	if g.mode == modeOff {
		return nil // off leaves no footprint, and its decisions have no row
	}
	if d.ID == "" {
		return errors.New("attention: decision has no id")
	}
	at := r.At
	if at.IsZero() {
		at = g.clock()
	}

	tx, err := g.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("attention: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if !r.Delivered {
		var disp string
		var delivered int
		err = tx.QueryRowContext(ctx,
			`SELECT disposition, delivered FROM attention_decisions WHERE id = ?`, d.ID).Scan(&disp, &delivered)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w %q", errUnknownDecision, d.ID)
		}
		if err != nil {
			return fmt.Errorf("attention: read decision: %w", err)
		}
		if (disp == string(proactive.DispNotify) || disp == string(proactive.DispEscalate)) && delivered == 0 {
			if _, err := tx.ExecContext(ctx,
				`UPDATE attention_decisions SET delivered_at_ms = ? WHERE id = ? AND delivered = 0`,
				at.UnixMilli(), d.ID); err != nil {
				return fmt.Errorf("attention: release decision: %w", err)
			}
		}
		return commit(tx)
	}

	if err := markDeliveredTx(ctx, tx, d.ID, at, r.InterventionID); err != nil {
		return err
	}
	return commit(tx)
}

var errUnknownDecision = errors.New("attention: unknown decision")

// markDeliveredTx records inside tx that decision id reached the owner at the
// given time, in the message named interventionID (empty = a message of its
// own, named after the decision). It upserts attention_state, so the situation
// dedups from now on. A decision already delivered is left as it is.
func markDeliveredTx(ctx context.Context, tx *sql.Tx, id string, at time.Time, interventionID string) error {
	var (
		disp, key, js string
		sev           int
		delivered     int
	)
	err := tx.QueryRowContext(ctx,
		`SELECT disposition, dedup_key, severity, candidate_json, delivered
		   FROM attention_decisions WHERE id = ?`, id).Scan(&disp, &key, &sev, &js, &delivered)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w %q", errUnknownDecision, id)
	}
	if err != nil {
		return fmt.Errorf("attention: read decision: %w", err)
	}
	sendable := disp == string(proactive.DispNotify) || disp == string(proactive.DispEscalate)
	if !sendable && disp != string(proactive.DispDigest) {
		return fmt.Errorf("attention: decision %q is %s and cannot be delivered", id, disp)
	}
	if delivered == 1 {
		return nil // idempotent: already counted
	}
	if interventionID == "" {
		interventionID = id
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE attention_decisions SET delivered = 1, delivered_at_ms = ?, intervention_id = ? WHERE id = ?`,
		at.UnixMilli(), interventionID, id); err != nil {
		return fmt.Errorf("attention: mark delivered: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO attention_state (dedup_key, last_severity, last_sent_ms, last_candidate_json, sends)
		 VALUES (?, ?, ?, ?, 1)
		 ON CONFLICT(dedup_key) DO UPDATE SET
		   last_severity = excluded.last_severity,
		   last_sent_ms = excluded.last_sent_ms,
		   last_candidate_json = excluded.last_candidate_json,
		   sends = sends + 1`,
		key, sev, at.UnixMilli(), js); err != nil {
		return fmt.Errorf("attention: update state: %w", err)
	}
	return nil
}

func commit(tx *sql.Tx) error {
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("attention: commit: %w", err)
	}
	return nil
}

// Mute silences candidates whose dedup key or trigger ID starts with keyPrefix
// until the given time. Re-muting a prefix replaces its deadline.
//
// The prefix must be at least two characters: a shorter one would silence
// almost everything. Even so, a mute never hides a protective Critical candidate
// unless the prefix equals its full dedup key or trigger ID.
func (g *Governor) Mute(ctx context.Context, keyPrefix string, until time.Time, reason string) error {
	keyPrefix = strings.TrimSpace(keyPrefix)
	if utf8.RuneCountInString(keyPrefix) < minMutePrefixRunes {
		return fmt.Errorf("attention: mute prefix must be at least %d characters", minMutePrefixRunes)
	}
	now := g.clock()
	if !until.After(now) {
		return errors.New("attention: mute deadline is not in the future")
	}
	reason = clampRunes(reason, maxMuteReasonRunes)
	_, err := g.db.ExecContext(ctx,
		`INSERT INTO attention_mutes (key, until_ms, reason, created_ms) VALUES (?, ?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET until_ms = excluded.until_ms, reason = excluded.reason, created_ms = excluded.created_ms`,
		keyPrefix, until.UnixMilli(), reason, now.UnixMilli())
	if err != nil {
		return fmt.Errorf("attention: mute: %w", err)
	}
	return nil
}

// Unmute removes the mute with exactly this prefix. Unmuting a prefix that is
// not muted is not an error.
func (g *Governor) Unmute(ctx context.Context, keyPrefix string) error {
	keyPrefix = strings.TrimSpace(keyPrefix)
	if keyPrefix == "" {
		return errors.New("attention: unmute needs a prefix")
	}
	if _, err := g.db.ExecContext(ctx, `DELETE FROM attention_mutes WHERE key = ?`, keyPrefix); err != nil {
		return fmt.Errorf("attention: unmute: %w", err)
	}
	return nil
}

// maybePrune trims old rows at most once per pruneEvery. It runs after the
// decision committed and its failure only logs: housekeeping must not cost a
// decision.
func (g *Governor) maybePrune(ctx context.Context, now time.Time) {
	g.mu.Lock()
	due := g.lastPrune.IsZero() || now.Sub(g.lastPrune) >= pruneEvery
	if due {
		g.lastPrune = now
	}
	g.mu.Unlock()
	if !due {
		return
	}
	if err := pruneDecisions(ctx, g.db, now, g.retention, g.pendingDigestKeep, g.shadowRetention); err != nil {
		logger.WarnCF("attention", "prune failed", map[string]any{"error": err.Error()})
	}
}
