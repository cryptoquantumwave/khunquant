package attention

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// The daily digest.
//
// Decisions with disposition digest are queued in attention_digest_queue by
// Decide, in the same transaction as the decision. Once per owner-local day a
// caller asks PendingDigest whether a digest is due and, if it is, gets the
// unflushed items; it sends one message and reports back with CompleteDigest.
//
// When the digest is due. Each local day d has a digest instant S(d): the first
// minute at or after Profile.DigestTime on d that is inside the active hours
// (when DigestTime itself is active that is DigestTime; when it falls in quiet
// hours it is the first active minute after it, which for a DigestTime past the
// end of the active hours is on the next day). A digest is due when the owner's
// clock is inside the active hours now, S(d) has passed for some day d newer
// than the last digest day, and the newest such d is taken. The last digest day
// is stored in attention_meta, so a restart neither repeats a digest nor skips
// one, and a process that was down at S(d) sends it on its first tick inside
// the active hours. An item queued after a day's digest waits for the next one.
// An empty queue sends nothing and still marks the day done.
//
// A digest handed out is not handed out again for digestRetry, whether or not
// its send succeeded: that is both the retry spacing after a failed send and
// the lease that keeps two callers from sending it twice. The time of the last
// hand-out is in attention_meta too, so a restart does not retry sooner.

const (
	// digestRetry is the least time between two digest attempts.
	digestRetry = 10 * time.Minute

	metaDigestDay     = "digest_last_day"
	metaDigestAttempt = "digest_attempt_ms"

	// Rendering limits.
	maxDigestItems    = 12
	maxDigestLineRune = 160
	maxDigestSubject  = 60
	digestNoSubject   = "General"
)

// Digest is the batch of queued items a digest message is made from.
type Digest struct {
	// Day is the local calendar day ("2006-01-02") this digest is for.
	Day string
	// Items are the unflushed digest decisions, oldest first.
	Items []Decision
}

// PendingDigest reports whether a digest is due now and returns its items. It
// returns nil, with no error, when none is due, when digests are disabled or
// the governor is not in live mode (shadow never delivers), or when the queue is
// empty (in which case the day is marked done). After a non-nil result the caller
// must report the send with CompleteDigest.
//
// With AttentionConfig.Digest false it never returns a digest, yet Decide still
// queues items for quiet hours, Info severity and the budget: they are recorded
// and never sent. The gateway logs one WARN about that at assembly.
func (g *Governor) PendingDigest(ctx context.Context) (*Digest, error) {
	if g.mode != modeLive || !g.cfg.Digest {
		return nil, nil
	}
	now := g.clock()
	prof, loc := g.profile(ctx, now) // before the transaction: another package's store
	day, due := digestDay(prof, loc, now)
	if !due {
		return nil, nil
	}

	tx, err := g.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("attention: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	last, err := getMeta(ctx, tx, metaDigestDay)
	if err != nil {
		return nil, err
	}
	if last >= day {
		return nil, nil
	}
	attempt, err := getMeta(ctx, tx, metaDigestAttempt)
	if err != nil {
		return nil, err
	}
	if ms, perr := strconv.ParseInt(attempt, 10, 64); perr == nil && now.UnixMilli() < ms+digestRetry.Milliseconds() {
		return nil, nil
	}

	items, err := queuedDigestItems(ctx, tx)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		if err := setMeta(ctx, tx, metaDigestDay, day); err != nil {
			return nil, err
		}
		return nil, commit(tx)
	}
	if err := setMeta(ctx, tx, metaDigestAttempt, strconv.FormatInt(now.UnixMilli(), 10)); err != nil {
		return nil, err
	}
	if err := commit(tx); err != nil {
		return nil, err
	}
	return &Digest{Day: day, Items: items}, nil
}

// CompleteDigest reports how sending dg went. With Delivered=true every item is
// marked delivered (through the same path as MarkDelivered, so each situation
// dedups from now on), flushed from the queue, and the day is recorded as done,
// all in one transaction. With Delivered=false nothing changes: the items stay
// queued and PendingDigest hands them out again after digestRetry.
func (g *Governor) CompleteDigest(ctx context.Context, dg *Digest, r Delivery) error {
	if g.mode != modeLive || dg == nil || !r.Delivered {
		return nil
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

	for _, it := range dg.Items {
		if err := markDeliveredTx(ctx, tx, it.ID, at, r.InterventionID); err != nil {
			if errors.Is(err, errUnknownDecision) {
				continue // pruned while the digest was being sent
			}
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE attention_digest_queue SET flushed_ms = ? WHERE decision_id = ? AND flushed_ms IS NULL`,
			at.UnixMilli(), it.ID); err != nil {
			return fmt.Errorf("attention: flush digest item: %w", err)
		}
	}
	if err := setMeta(ctx, tx, metaDigestDay, dg.Day); err != nil {
		return err
	}
	return commit(tx)
}

// digestDay returns the local day whose digest is due at now, as "2006-01-02".
// See the comment at the top of the file for the rule. It reads the profile and
// is therefore called before any transaction opens.
func digestDay(prof intent.Profile, loc *time.Location, now time.Time) (string, bool) {
	if !prof.InActiveHours(now) {
		return "", false
	}
	hour, minute := digestClock(prof.DigestTime)
	local := now.In(loc)
	best := ""
	for back := 1; back >= 0; back-- { // yesterday first: a later day wins
		d := local.AddDate(0, 0, -back)
		at := time.Date(d.Year(), d.Month(), d.Day(), hour, minute, 0, 0, loc)
		for t := at; !t.After(now); t = t.Add(time.Minute) {
			if prof.InActiveHours(t) {
				best = at.Format("2006-01-02")
				break
			}
		}
	}
	return best, best != ""
}

// digestClock parses a zero-padded "HH:MM", falling back to the default digest
// time for anything else.
func digestClock(s string) (hour, minute int) {
	t, err := time.Parse("15:04", strings.TrimSpace(s))
	if err != nil {
		t, _ = time.Parse("15:04", intent.DefaultDigestTime)
	}
	return t.Hour(), t.Minute()
}

// queuedDigestItems reads the unflushed digest decisions, oldest first. A row
// whose stored candidate no longer parses still yields an item, carrying only
// what the columns say.
func queuedDigestItems(ctx context.Context, tx *sql.Tx) ([]Decision, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT d.id, d.at_ms, d.reason, d.dedup_key, d.trigger_id, d.severity, d.candidate_json
		   FROM attention_digest_queue q
		   JOIN attention_decisions d ON d.id = q.decision_id
		  WHERE q.flushed_ms IS NULL AND d.delivered = 0 AND d.disposition = 'digest'
		  ORDER BY q.enqueued_ms, q.decision_id`)
	if err != nil {
		return nil, fmt.Errorf("attention: read digest queue: %w", err)
	}
	var items []Decision
	for rows.Next() {
		var (
			id, reason, key, trig, js string
			atMs                      int64
			sev                       int
		)
		if err := rows.Scan(&id, &atMs, &reason, &key, &trig, &sev, &js); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("attention: read digest queue: %w", err)
		}
		c, ok := decodeCandidate(js)
		if !ok {
			c = proactive.Candidate{DedupKey: key, TriggerID: trig, Severity: proactive.Severity(sev)}
		}
		items = append(items, Decision{
			ID:          id,
			Disposition: proactive.DispDigest,
			Reason:      reason,
			Candidate:   c,
			At:          time.UnixMilli(atMs),
		})
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("attention: read digest queue: %w", err)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("attention: read digest queue: %w", err)
	}
	return items, nil
}

type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// getMeta returns the attention_meta value for key, "" when absent.
func getMeta(ctx context.Context, q rowQuerier, key string) (string, error) {
	var v string
	err := q.QueryRowContext(ctx, `SELECT value FROM attention_meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("attention: read meta %s: %w", key, err)
	}
	return v, nil
}

func setMeta(ctx context.Context, tx *sql.Tx, key, value string) error {
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO attention_meta (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value); err != nil {
		return fmt.Errorf("attention: write meta %s: %w", key, err)
	}
	return nil
}

// ---- rendering -------------------------------------------------------------

// RenderDigest renders the items of a digest as a title and a body, without any
// I/O. Items are ranked by severity (highest first), then salience, then age
// (oldest first); the first maxDigestItems are shown, grouped by Subject in the
// order their best item ranks, and the rest are summarised as "and N more".
// Titles and subjects can carry text from outside the process, so each is
// reduced to one clean line here: a newline in a title never becomes a second
// item line. The message carries no actions and no footer.
func RenderDigest(items []Decision) (title, body string) {
	n := len(items)
	if n == 0 {
		return "", ""
	}
	ranked := append([]Decision(nil), items...)
	sort.SliceStable(ranked, func(i, j int) bool { return ranksBefore(ranked[i], ranked[j]) })

	shown := ranked[:min(n, maxDigestItems)]
	var order []string
	bySubject := map[string][]string{}
	for _, d := range shown {
		subject := oneLine(d.Candidate.Subject, maxDigestSubject)
		if subject == "" {
			subject = digestNoSubject
		}
		line := oneLine(d.Candidate.Title, maxDigestLineRune)
		if line == "" {
			line = oneLine(d.Candidate.DedupKey, maxDigestLineRune)
		}
		if line == "" {
			line = "(no title)"
		}
		if _, seen := bySubject[subject]; !seen {
			order = append(order, subject)
		}
		bySubject[subject] = append(bySubject[subject], "• "+line)
	}

	var b strings.Builder
	for _, subject := range order {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(subject)
		for _, line := range bySubject[subject] {
			b.WriteString("\n" + line)
		}
	}
	if n > maxDigestItems {
		b.WriteString("\n\nand " + strconv.Itoa(n-maxDigestItems) + " more")
	}

	noun := "items"
	if n == 1 {
		noun = "item"
	}
	return "Daily digest — " + strconv.Itoa(n) + " " + noun, b.String()
}

// ranksBefore orders decisions by severity (high first), salience (high first),
// then time (old first), then ID, so that the order is total.
func ranksBefore(a, b Decision) bool {
	if a.Candidate.Severity != b.Candidate.Severity {
		return a.Candidate.Severity > b.Candidate.Severity
	}
	if a.Candidate.Salience != b.Candidate.Salience {
		return a.Candidate.Salience > b.Candidate.Salience
	}
	if !a.At.Equal(b.At) {
		return a.At.Before(b.At)
	}
	return a.ID < b.ID
}

// oneLine reduces s to a single line of at most limit runes: control and
// invisible format characters (which include every line break) are dropped or
// turned into a space, runs of white space collapse, and a long line ends with an
// ellipsis. notify cleans text again before sending; this keeps a title from
// becoming more than one item line before it gets there.
func oneLine(s string, limit int) string {
	s = strings.ToValidUTF8(s, "")
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case unicode.IsSpace(r):
			b.WriteRune(' ')
		case unicode.IsControl(r), unicode.Is(unicode.Cf, r):
			// dropped
		default:
			b.WriteRune(r)
		}
	}
	s = strings.Join(strings.Fields(b.String()), " ")
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	return strings.TrimRight(string([]rune(s)[:limit-1]), " ") + "…"
}
