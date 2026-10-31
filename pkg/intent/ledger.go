package intent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// SystemExpiry is the by_sender of the event the ledger writes when an intent
// reaches its ExpiresAt.
const SystemExpiry = "system:expiry"

// SystemBudget is the by_sender of the event written when the last fire of a
// budget is spent.
const SystemBudget = "system:budget"

const (
	openStatusesSQL = `'proposed','active','paused'`

	intentCols = `id, type, title, params_json, trigger_json, expected_json, mandate_id, status, source,
	provenance_json, fire_budget, fires_used, cooldown_ms, lifetime_ms, expires_at_ms, valid_from_ms,
	invalid_at_ms, superseded_by, created_at_ms, updated_at_ms`
)

// SetClock makes the store read the time from now (nil restores time.Now). It
// affects this Store value only; it is for tests.
func (s *Store) SetClock(now func() time.Time) { s.now = now }

func (s *Store) clock() time.Time {
	//simclock:wall the default when no clock is injected
	t := time.Now
	if s.now != nil {
		t = s.now
	}
	return t().UTC().Truncate(time.Millisecond)
}

// byName is the by_sender of an event for provenance p.
func byName(p Provenance) string {
	if p.SenderID != "" {
		return p.SenderID
	}
	return "turn:" + string(p.TurnKind)
}

func ms(t time.Time) int64 { return t.UnixMilli() }

func nullMS(t *time.Time) sql.NullInt64 {
	if t == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: ms(*t), Valid: true}
}

// initialExpiry is the ExpiresAt a new intent gets: an active one lives for its
// Lifetime (forever when 0); a proposed one waits ProposalTTL, or less when its
// own Lifetime is shorter.
func initialExpiry(st Status, life time.Duration, now time.Time) *time.Time {
	var d time.Duration
	switch st {
	case StatusActive:
		d = life
	case StatusProposed:
		d = ProposalTTL
		if life > 0 && life < d {
			d = life
		}
	}
	if d <= 0 {
		return nil
	}
	t := now.Add(d)
	return &t
}

type rowScanner interface{ Scan(dest ...any) error }

// scanIntent reads one row and applies lazy expiry: an open intent whose
// ExpiresAt has passed reads as expired, whether or not the sweeper has run.
func scanIntent(r rowScanner, now time.Time) (Intent, error) {
	var (
		it                                     Intent
		typ, status, source                    string
		params, trig, exp, prov                string
		cooldown, lifetime, validFrom, created int64
		updated                                int64
		expiresAt, invalidAt                   sql.NullInt64
		budget, used                           int
	)
	if err := r.Scan(&it.ID, &typ, &it.Title, &params, &trig, &exp, &it.MandateID, &status, &source,
		&prov, &budget, &used, &cooldown, &lifetime, &expiresAt, &validFrom, &invalidAt,
		&it.SupersededBy, &created, &updated); err != nil {
		return Intent{}, err
	}
	it.Type, it.Status, it.Source = IntentType(typ), Status(status), Source(source)
	it.FireBudget, it.FiresUsed = budget, used
	it.Cooldown, it.Lifetime = time.Duration(cooldown)*time.Millisecond, time.Duration(lifetime)*time.Millisecond
	it.ValidFrom = time.UnixMilli(validFrom).UTC()
	it.CreatedAt, it.UpdatedAt = time.UnixMilli(created).UTC(), time.UnixMilli(updated).UTC()
	if expiresAt.Valid {
		t := time.UnixMilli(expiresAt.Int64).UTC()
		it.ExpiresAt = &t
	}
	if invalidAt.Valid {
		t := time.UnixMilli(invalidAt.Int64).UTC()
		it.InvalidAt = &t
	}
	if params != "" {
		it.Params = json.RawMessage(params)
	}
	if trig != "" {
		var ts TriggerSpec
		if err := json.Unmarshal([]byte(trig), &ts); err != nil {
			return Intent{}, fmt.Errorf("intent: decode stored trigger of %s: %w", it.ID, err)
		}
		it.Trigger = &ts
	}
	if exp != "" {
		var e Expectation
		if err := json.Unmarshal([]byte(exp), &e); err != nil {
			return Intent{}, fmt.Errorf("intent: decode stored expectation of %s: %w", it.ID, err)
		}
		it.Expected = &e
	}
	if err := json.Unmarshal([]byte(prov), &it.Provenance); err != nil {
		return Intent{}, fmt.Errorf("intent: decode stored provenance of %s: %w", it.ID, err)
	}
	if it.Status.Open() && it.ExpiresAt != nil && !it.ExpiresAt.After(now) {
		it.Status = StatusExpired
		inv := *it.ExpiresAt
		it.InvalidAt = &inv
	}
	return it, nil
}

func encodeJSON(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// insertIntent writes it and its creation event inside tx.
func insertIntent(ctx context.Context, tx *sql.Tx, it Intent) error {
	var trig, exp string
	var err error
	if it.Trigger != nil {
		if trig, err = encodeJSON(it.Trigger); err != nil {
			return fmt.Errorf("intent: encode trigger: %w", err)
		}
	}
	if it.Expected != nil {
		if exp, err = encodeJSON(it.Expected); err != nil {
			return fmt.Errorf("intent: encode expectation: %w", err)
		}
	}
	prov, err := encodeJSON(it.Provenance)
	if err != nil {
		return fmt.Errorf("intent: encode provenance: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO intents (`+intentCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		it.ID, string(it.Type), it.Title, string(it.Params), trig, exp, it.MandateID, string(it.Status),
		string(it.Source), prov, it.FireBudget, it.FiresUsed, it.Cooldown.Milliseconds(), it.Lifetime.Milliseconds(),
		nullMS(it.ExpiresAt), ms(it.ValidFrom), nullMS(it.InvalidAt), it.SupersededBy,
		ms(it.CreatedAt), ms(it.UpdatedAt)); err != nil {
		return fmt.Errorf("intent: store intent: %w", err)
	}
	return insertEvent(ctx, tx, it.ID, "", it.Status, byName(it.Provenance), "created", it.CreatedAt)
}

func insertEvent(ctx context.Context, tx *sql.Tx, id string, from, to Status, by, reason string, at time.Time) error {
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO intent_events (intent_id, at_ms, from_status, to_status, by_sender, reason) VALUES (?,?,?,?,?,?)`,
		id, ms(at), string(from), string(to), cleanAuthor(by), cleanReason(reason)); err != nil {
		return fmt.Errorf("intent: record event: %w", err)
	}
	return nil
}

// getTx reads one intent inside tx, with lazy expiry applied.
func getTx(ctx context.Context, tx *sql.Tx, id string, now time.Time) (Intent, error) {
	it, err := scanIntent(tx.QueryRowContext(ctx, `SELECT `+intentCols+` FROM intents WHERE id = ?`, id), now)
	if errors.Is(err, sql.ErrNoRows) {
		return Intent{}, fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	if err != nil {
		return Intent{}, fmt.Errorf("intent: read intent: %w", err)
	}
	return it, nil
}

// expireDueTx moves every open intent whose ExpiresAt is at or before now to
// expired, with an event each, inside tx. It reads the due rows fully before it
// writes.
func expireDueTx(ctx context.Context, tx *sql.Tx, now time.Time) (int, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT id, status, expires_at_ms FROM intents
		  WHERE status IN (`+openStatusesSQL+`) AND expires_at_ms IS NOT NULL AND expires_at_ms <= ?
		  ORDER BY expires_at_ms, id`, ms(now))
	if err != nil {
		return 0, fmt.Errorf("intent: find expired intents: %w", err)
	}
	type due struct {
		id   string
		from Status
		at   int64
	}
	var list []due
	for rows.Next() {
		var d due
		var st string
		if err := rows.Scan(&d.id, &st, &d.at); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("intent: find expired intents: %w", err)
		}
		d.from = Status(st)
		list = append(list, d)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, fmt.Errorf("intent: find expired intents: %w", err)
	}
	_ = rows.Close()

	for _, d := range list {
		res, err := tx.ExecContext(ctx,
			`UPDATE intents SET status = ?, invalid_at_ms = ?, updated_at_ms = ? WHERE id = ? AND status = ?`,
			string(StatusExpired), d.at, ms(now), d.id, string(d.from))
		if err != nil {
			return 0, fmt.Errorf("intent: expire intent: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue
		}
		if err := insertEvent(ctx, tx, d.id, d.from, StatusExpired, SystemExpiry, "reached its expiry", now); err != nil {
			return 0, err
		}
	}
	return len(list), nil
}

// checkCaps refuses an insert that would take the ledger past MaxOpenIntents, or
// past MaxProposedIntents when the new intent is a proposal. releaseOpen and
// releaseProposed say how many open and proposed intents the same transaction
// ends (a supersede). Call it after expireDueTx, so a lapsed intent frees its
// place.
func checkCaps(ctx context.Context, tx *sql.Tx, adding Status, releaseOpen, releaseProposed int) error {
	var open, proposed int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(status = 'proposed'), 0) FROM intents WHERE status IN (`+openStatusesSQL+`)`,
	).Scan(&open, &proposed); err != nil {
		return fmt.Errorf("intent: count intents: %w", err)
	}
	if open-releaseOpen+1 > MaxOpenIntents {
		return fmt.Errorf("%w: there are already %d open intents (the limit is %d). "+
			"Cancel some that are no longer wanted (update_intent with action cancel; list_intents shows them) and try again",
			ErrTooManyOpen, open, MaxOpenIntents)
	}
	if adding == StatusProposed && proposed-releaseProposed+1 > MaxProposedIntents {
		return fmt.Errorf("%w: %d suggested intents are already waiting for the owner (the limit is %d). "+
			"Ask the owner to confirm or cancel some (update_intent with action confirm or cancel; list_intents shows them); "+
			"unanswered ones expire after %d days",
			ErrTooManyProposed, proposed, MaxProposedIntents, int(ProposalTTL/(24*time.Hour)))
	}
	return nil
}

// CreateIntent validates in and stores it with a new ID, returning the stored
// row. Status must be proposed (the default) or active; Source defaults to
// inferred. The ledger enforces a floor under activation whatever the caller
// decided: an active intent must be explicit and carry the provenance of a user
// turn with user-authored trust and a sender (ErrNotOwnerAuthored otherwise).
// Whether that sender is THE OWNER is for the caller, which knows the turn.
//
// MandateID must be empty: an inferred intent can never carry a mandate
// (ErrInferredMandate), and until mandates exist no intent can
// (ErrMandatesUnavailable).
//
// The caps (MaxOpenIntents, MaxProposedIntents) are checked in the same
// transaction as the insert, after lapsed intents have been expired.
func (s *Store) CreateIntent(ctx context.Context, in Intent) (Intent, error) {
	it, err := normalizeNew(in)
	if err != nil {
		return Intent{}, err
	}
	now := s.clock()
	it.ID = proactive.NewID()
	it.CreatedAt, it.UpdatedAt, it.ValidFrom = now, now, now
	it.ExpiresAt = initialExpiry(it.Status, it.Lifetime, now)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Intent{}, fmt.Errorf("intent: create intent: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := expireDueTx(ctx, tx, now); err != nil {
		return Intent{}, err
	}
	if err := checkCaps(ctx, tx, it.Status, 0, 0); err != nil {
		return Intent{}, err
	}
	if err := insertIntent(ctx, tx, it); err != nil {
		return Intent{}, err
	}
	if err := tx.Commit(); err != nil {
		return Intent{}, fmt.Errorf("intent: create intent: %w", err)
	}
	return it, nil
}

// GetIntent returns one intent. An intent past its ExpiresAt reads as expired.
func (s *Store) GetIntent(ctx context.Context, id string) (Intent, error) {
	it, err := scanIntent(s.db.QueryRowContext(ctx, `SELECT `+intentCols+` FROM intents WHERE id = ?`, id), s.clock())
	if errors.Is(err, sql.ErrNoRows) {
		return Intent{}, fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	if err != nil {
		return Intent{}, fmt.Errorf("intent: read intent: %w", err)
	}
	return it, nil
}

// filterSQL renders the WHERE clause of f at time now: statuses are matched
// against the effective status, so an intent past its ExpiresAt counts as
// expired.
func filterSQL(f IntentFilter, now time.Time) (where string, args []any, err error) {
	var conds []string
	if len(f.Statuses) > 0 {
		marks := make([]string, len(f.Statuses))
		args = append(args, ms(now))
		for i, st := range f.Statuses {
			if !st.Valid() {
				return "", nil, fmt.Errorf("intent: unknown status %q", st)
			}
			marks[i] = "?"
			args = append(args, string(st))
		}
		conds = append(conds, `(CASE WHEN status IN (`+openStatusesSQL+`) AND expires_at_ms IS NOT NULL AND expires_at_ms <= ?
			THEN 'expired' ELSE status END) IN (`+strings.Join(marks, ",")+`)`)
	}
	if len(f.Types) > 0 {
		marks := make([]string, len(f.Types))
		for i, t := range f.Types {
			if !t.Valid() {
				return "", nil, fmt.Errorf("intent: unknown type %q", t)
			}
			marks[i] = "?"
			args = append(args, string(t))
		}
		conds = append(conds, `type IN (`+strings.Join(marks, ",")+`)`)
	}
	if len(conds) == 0 {
		return "", nil, nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args, nil
}

// ListIntents returns the intents f selects, oldest first unless f.NewestFirst. Every row has lazy
// expiry applied, and f.Statuses is matched against the effective status.
func (s *Store) ListIntents(ctx context.Context, f IntentFilter) ([]Intent, error) {
	now := s.clock()
	where, args, err := filterSQL(f, now)
	if err != nil {
		return nil, err
	}
	limit := f.Limit
	if limit < 1 || limit > MaxListLimit {
		limit = MaxListLimit
	}
	args = append(args, limit)
	order := `created_at_ms, id`
	if f.NewestFirst {
		order = `created_at_ms DESC, id DESC`
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+intentCols+` FROM intents`+where+` ORDER BY `+order+` LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("intent: list intents: %w", err)
	}
	defer rows.Close()
	var out []Intent
	for rows.Next() {
		it, err := scanIntent(rows, now)
		if err != nil {
			return nil, fmt.Errorf("intent: list intents: %w", err)
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("intent: list intents: %w", err)
	}
	return out, nil
}

// CountIntents returns how many intents f selects (f.Limit is ignored).
func (s *Store) CountIntents(ctx context.Context, f IntentFilter) (int, error) {
	where, args, err := filterSQL(f, s.clock())
	if err != nil {
		return 0, err
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM intents`+where, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("intent: count intents: %w", err)
	}
	return n, nil
}

// Events returns the history of one intent, oldest first: its creation and every
// status change.
func (s *Store) Events(ctx context.Context, id string) ([]IntentEvent, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, intent_id, at_ms, from_status, to_status, by_sender, reason
		   FROM intent_events WHERE intent_id = ? ORDER BY id`, id)
	if err != nil {
		return nil, fmt.Errorf("intent: read events: %w", err)
	}
	defer rows.Close()
	var out []IntentEvent
	for rows.Next() {
		var e IntentEvent
		var at int64
		var from, to string
		if err := rows.Scan(&e.ID, &e.IntentID, &at, &from, &to, &e.By, &e.Reason); err != nil {
			return nil, fmt.Errorf("intent: read events: %w", err)
		}
		e.At, e.FromStatus, e.ToStatus = time.UnixMilli(at).UTC(), Status(from), Status(to)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("intent: read events: %w", err)
	}
	return out, nil
}

// Transition moves an intent to a new status if the table allows it, and writes
// an event row in the same transaction. by is who asked; reason is a short note
// for the event. proposed to active is the owner's confirmation, so by must be
// the provenance of a user turn with user-authored trust and a sender
// (ErrNotOwnerAuthored otherwise); whether that sender is the owner is for the
// caller. Activation starts the intent's Lifetime clock.
func (s *Store) Transition(ctx context.Context, id string, to Status, by Provenance, reason string) error {
	if !to.Valid() {
		return fmt.Errorf("intent: unknown status %q", to)
	}
	by, err := cleanProvenance(by)
	if err != nil {
		return err
	}
	now := s.clock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("intent: transition: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	// Expire what has lapsed first, so a move on a lapsed intent is refused as a
	// move from expired, and so its expiry is on record.
	if _, err := expireDueTx(ctx, tx, now); err != nil {
		return err
	}
	cur, err := getTx(ctx, tx, id, now)
	if err != nil {
		return err
	}
	if !CanTransition(cur.Status, to) {
		return fmt.Errorf("%w: a %s intent cannot become %s", ErrBadTransition, cur.Status, to)
	}
	if cur.Status == StatusProposed && to == StatusActive && !ownerAuthored(by) {
		return ErrNotOwnerAuthored
	}

	set := `status = ?, updated_at_ms = ?`
	args := []any{string(to), ms(now)}
	switch {
	case !to.Open():
		set += `, invalid_at_ms = ?`
		args = append(args, ms(now))
	case cur.Status == StatusProposed && to == StatusActive:
		if cur.Lifetime > 0 {
			set += `, expires_at_ms = ?`
			args = append(args, ms(now.Add(cur.Lifetime)))
		} else {
			set += `, expires_at_ms = NULL`
		}
	}
	args = append(args, id, string(cur.Status))
	res, err := tx.ExecContext(ctx, `UPDATE intents SET `+set+` WHERE id = ? AND status = ?`, args...)
	if err != nil {
		return fmt.Errorf("intent: transition: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("%w: the intent changed while it was being moved", ErrBadTransition)
	}
	if err := insertEvent(ctx, tx, id, cur.Status, to, byName(by), reason, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("intent: transition: %w", err)
	}
	return nil
}

// Supersede replaces an open intent with next, keeping history: the old row ends
// as cancelled with SupersededBy pointing at the new one, and both get event
// rows, in one transaction. next obeys every rule of CreateIntent. The old
// intent's place is free for the new one, so a full ledger can still be edited.
func (s *Store) Supersede(ctx context.Context, id string, next Intent, by Provenance) (Intent, error) {
	it, err := normalizeNew(next)
	if err != nil {
		return Intent{}, err
	}
	by, err = cleanProvenance(by)
	if err != nil {
		return Intent{}, err
	}
	now := s.clock()
	it.ID = proactive.NewID()
	it.CreatedAt, it.UpdatedAt, it.ValidFrom = now, now, now
	it.ExpiresAt = initialExpiry(it.Status, it.Lifetime, now)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Intent{}, fmt.Errorf("intent: supersede: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := expireDueTx(ctx, tx, now); err != nil {
		return Intent{}, err
	}
	old, err := getTx(ctx, tx, id, now)
	if err != nil {
		return Intent{}, err
	}
	if !CanTransition(old.Status, StatusCancelled) {
		return Intent{}, fmt.Errorf("%w: a %s intent cannot be superseded", ErrBadTransition, old.Status)
	}
	relProposed := 0
	if old.Status == StatusProposed {
		relProposed = 1
	}
	if err := checkCaps(ctx, tx, it.Status, 1, relProposed); err != nil {
		return Intent{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE intents SET status = ?, invalid_at_ms = ?, superseded_by = ?, updated_at_ms = ? WHERE id = ? AND status = ?`,
		string(StatusCancelled), ms(now), it.ID, ms(now), id, string(old.Status)); err != nil {
		return Intent{}, fmt.Errorf("intent: supersede: %w", err)
	}
	if err := insertEvent(ctx, tx, id, old.Status, StatusCancelled, byName(by), "superseded by "+it.ID, now); err != nil {
		return Intent{}, err
	}
	if err := insertIntent(ctx, tx, it); err != nil {
		return Intent{}, err
	}
	if err := tx.Commit(); err != nil {
		return Intent{}, fmt.Errorf("intent: supersede: %w", err)
	}
	return it, nil
}

// RecordFire counts one fire of an active intent and returns the updated row;
// when it spends the last fire of a budget the intent becomes done in the same
// transaction (with an event). The check and the increment are one conditional
// UPDATE, so concurrent callers, in this process or another, can never spend
// more than the budget: the loser gets ErrBudgetSpent (or ErrNotActive).
func (s *Store) RecordFire(ctx context.Context, id string) (Intent, error) {
	now := s.clock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Intent{}, fmt.Errorf("intent: record fire: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := expireDueTx(ctx, tx, now); err != nil {
		return Intent{}, err
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE intents SET fires_used = fires_used + 1, updated_at_ms = ?
		  WHERE id = ? AND status = 'active' AND (fire_budget = 0 OR fires_used < fire_budget)`, ms(now), id)
	if err != nil {
		return Intent{}, fmt.Errorf("intent: record fire: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		cur, err := getTx(ctx, tx, id, now)
		if err != nil {
			return Intent{}, err
		}
		if cur.Status != StatusActive {
			return Intent{}, fmt.Errorf("%w: it is %s", ErrNotActive, cur.Status)
		}
		return Intent{}, ErrBudgetSpent
	}
	cur, err := getTx(ctx, tx, id, now)
	if err != nil {
		return Intent{}, err
	}
	if cur.FireBudget > 0 && cur.FiresUsed >= cur.FireBudget {
		if _, err := tx.ExecContext(ctx,
			`UPDATE intents SET status = 'done', invalid_at_ms = ?, updated_at_ms = ? WHERE id = ? AND status = 'active'`,
			ms(now), ms(now), id); err != nil {
			return Intent{}, fmt.Errorf("intent: record fire: %w", err)
		}
		if err := insertEvent(ctx, tx, id, StatusActive, StatusDone, SystemBudget,
			fmt.Sprintf("spent its %d fires", cur.FireBudget), now); err != nil {
			return Intent{}, err
		}
		cur.Status = StatusDone
		cur.InvalidAt = &now
	}
	if err := tx.Commit(); err != nil {
		return Intent{}, fmt.Errorf("intent: record fire: %w", err)
	}
	return cur, nil
}

// ExpireDue moves every open intent whose ExpiresAt is at or before now to
// expired, writing the event rows, and returns how many it moved. The engine
// calls it about once a minute while the pipeline runs; reads do not wait for it
// (they apply the same rule themselves), so it only keeps the stored rows and
// the event history current.
func (s *Store) ExpireDue(ctx context.Context, now time.Time) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("intent: expire: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	n, err := expireDueTx(ctx, tx, now.UTC().Truncate(time.Millisecond))
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("intent: expire: %w", err)
	}
	return n, nil
}
