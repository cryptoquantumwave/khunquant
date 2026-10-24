// Package dashboard is the read side of the launcher's Proactive page: what the assistant
// noticed, said and did (interventions), how well that worked (metrics), and what changed
// since a cursor (the feed behind the live stream).
//
// It READS ONLY. It opens nothing and writes nothing: the gateway hands it the shared
// proactive database it already holds, so the launcher process never opens a database
// for these views. Every table it reads belongs to another package and may not exist (a
// package whose feature is off never created its table): a missing table is "no data",
// never an error.
//
// What leaves is a projection, not rows. Never returned: a candidate's JSON (only its
// title is extracted), its Quote or Evidence, the dedup key, the full trigger id, a
// proposal's params, hash, nonce, rationale or card ids, or who decided a proposal. A
// title and a proposal summary can carry text a model or another person wrote: they are
// data, and every consumer renders them as plain text.
package dashboard

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// Paging bounds. A page and a poll are never larger than MaxLimit.
const (
	DefaultLimit = 50
	MaxLimit     = 200

	maxTitleRunes   = 200
	maxReasonRunes  = 300
	maxSummaryRunes = 300
	maxCodeRunes    = 64
	queryTimeout    = 10 * time.Second
)

// ErrBadRequest marks a request the caller got wrong (a bad cursor, filter or number).
var ErrBadRequest = errors.New("dashboard: bad request")

func badf(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrBadRequest, fmt.Sprintf(format, a...))
}

// Response is what the owner did with a message: a kind and a short code, never typed text.
type Response struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

// Hindsight is how a market alert looked afterwards at one horizon. A nil value means
// nothing reliable was found; it is not zero.
type Hindsight struct {
	Horizon      string   `json:"horizon"`
	PctChange    *float64 `json:"pct_change"`
	PortfolioPct *float64 `json:"portfolio_pct"`
	Trades       *int     `json:"trades"`
}

// Intervention is one attention decision as the dashboard shows it. ID is opaque and
// stable; it is also the paging cursor that follows the row.
type Intervention struct {
	ID           string      `json:"id"`
	At           time.Time   `json:"at"`
	TriggerKind  string      `json:"trigger_kind"`
	Severity     int         `json:"severity"`
	SeverityName string      `json:"severity_name"`
	Disposition  string      `json:"disposition"`
	Reason       string      `json:"reason"`
	Title        string      `json:"title"` // may be model-written: plain text only
	Delivered    bool        `json:"delivered"`
	Shadow       bool        `json:"shadow"`
	Holdout      bool        `json:"holdout"`
	Responses    []Response  `json:"responses"`
	Hindsight    []Hindsight `json:"hindsight"`

	rowid int64
	key   string // COALESCE(intervention_id, id): joins the feedback tables
}

// Filter selects interventions. Zero fields do not filter.
type Filter struct {
	From, To    time.Time // At in [From, To)
	Disposition string
	Trigger     string // a trigger kind such as "price_cross", matched whole, never as a pattern
	Cursor      string
	Limit       int
}

// Reader reads the shared proactive database. Zone is the owner's time zone (nil is UTC)
// and Harm the harm guardrail's source (nil while learning is off).
type Reader struct {
	DB   *sql.DB
	Zone func(ctx context.Context) *time.Location
	Harm HarmSource
	Now  func() time.Time
}

func (r *Reader) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Reader) loc(ctx context.Context) *time.Location {
	if r.Zone != nil {
		if l := r.Zone(ctx); l != nil {
			return l
		}
	}
	return time.UTC
}

func (r *Reader) hasTable(ctx context.Context, name string) (bool, error) {
	var one int
	err := r.DB.QueryRowContext(ctx, `SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

const decisionCols = `rowid, at_ms, trigger_id, severity, disposition, reason, shadow, holdout, delivered,
	CASE WHEN json_valid(candidate_json) THEN json_extract(candidate_json, '$.Title') END, COALESCE(intervention_id, id)`

// Interventions returns a page of decisions, newest first, each joined with its feedback
// responses and hindsight rows, and the cursor of the next page ("" at the end). A page
// is at most MaxLimit rows. Without the attention table the page is empty.
func (r *Reader) Interventions(ctx context.Context, f Filter) (items []Intervention, next string, err error) {
	if f.Limit <= 0 {
		f.Limit = DefaultLimit
	}
	if f.Limit > MaxLimit {
		return nil, "", badf("limit is at most %d", MaxLimit)
	}
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	if ok, err := r.hasTable(ctx, "attention_decisions"); err != nil || !ok {
		return []Intervention{}, "", err
	}
	where, args := []string{"1 = 1"}, []any{}
	if !f.From.IsZero() {
		where, args = append(where, "at_ms >= ?"), append(args, f.From.UnixMilli())
	}
	if !f.To.IsZero() {
		where, args = append(where, "at_ms < ?"), append(args, f.To.UnixMilli())
	}
	if f.Disposition != "" {
		where, args = append(where, "disposition = ?"), append(args, f.Disposition)
	}
	if f.Trigger != "" {
		// A literal prefix compare, never LIKE: a % or _ in the input must not widen it.
		where = append(where, "(trigger_id = ? OR substr(trigger_id, 1, ?) = ?)")
		args = append(args, f.Trigger, len(f.Trigger)+1, f.Trigger+":")
	}
	if f.Cursor != "" {
		at, rowid, err := parseRowCursor(f.Cursor)
		if err != nil {
			return nil, "", err
		}
		where, args = append(where, "(at_ms < ? OR (at_ms = ? AND rowid < ?))"), append(args, at, at, rowid)
	}
	rows, err := r.decisions(ctx, strings.Join(where, " AND "), "at_ms DESC, rowid DESC", f.Limit+1, args...)
	if err != nil {
		return nil, "", err
	}
	if len(rows) > f.Limit {
		rows = rows[:f.Limit]
		next = rows[len(rows)-1].ID
	}
	return rows, next, nil
}

// decisions reads decision rows by a fixed where/order and joins their feedback.
func (r *Reader) decisions(ctx context.Context, where, order string, limit int, args ...any) ([]Intervention, error) {
	q := `SELECT ` + decisionCols + ` FROM attention_decisions WHERE ` + where + ` ORDER BY ` + order + ` LIMIT ?`
	rows, err := r.DB.QueryContext(ctx, q, append(args, limit)...)
	if err != nil {
		return nil, fmt.Errorf("dashboard: read decisions: %w", err)
	}
	out := []Intervention{}
	for rows.Next() {
		var (
			it                       Intervention
			at                       int64
			trig, disp, reason, key  string
			title                    sql.NullString
			shadow, holdout, deliver int
		)
		if err := rows.Scan(&it.rowid, &at, &trig, &it.Severity, &disp, &reason, &shadow, &holdout, &deliver, &title, &key); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("dashboard: read decisions: %w", err)
		}
		it.ID, it.At = rowCursor(at, it.rowid), time.UnixMilli(at).UTC()
		it.TriggerKind = clip(cleanLine(strings.SplitN(trig, ":", 2)[0]), maxCodeRunes)
		it.Disposition, it.Reason = clip(cleanLine(disp), maxCodeRunes), clip(cleanLine(reason), maxReasonRunes)
		it.Title = clip(cleanLine(title.String), maxTitleRunes)
		it.Delivered, it.Shadow, it.Holdout, it.key = deliver != 0, shadow != 0, holdout != 0, key
		it.Responses, it.Hindsight = []Response{}, []Hindsight{}
		out = append(out, it)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		out[i].SeverityName = proactive.Severity(out[i].Severity).String()
	}
	return out, r.join(ctx, out)
}

// join adds each decision's feedback responses and hindsight rows: one query for each
// table, not one per row. A table that does not exist adds nothing.
func (r *Reader) join(ctx context.Context, items []Intervention) error {
	if len(items) == 0 {
		return nil
	}
	by := map[string][]int{}
	keys := make([]any, 0, len(items))
	for i, it := range items {
		if _, seen := by[it.key]; !seen {
			keys = append(keys, it.key)
		}
		by[it.key] = append(by[it.key], i)
	}
	in := strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",")
	if ok, err := r.hasTable(ctx, "feedback_responses"); err != nil {
		return err
	} else if ok {
		rs, err := r.DB.QueryContext(ctx, `SELECT intervention_id, kind, value FROM feedback_responses WHERE intervention_id IN (`+in+`) ORDER BY at_ms, id`, keys...)
		if err != nil {
			return fmt.Errorf("dashboard: read responses: %w", err)
		}
		for rs.Next() {
			var id string
			var resp Response
			if err := rs.Scan(&id, &resp.Kind, &resp.Value); err != nil {
				_ = rs.Close()
				return fmt.Errorf("dashboard: read responses: %w", err)
			}
			resp.Kind, resp.Value = clip(cleanLine(resp.Kind), maxCodeRunes), clip(cleanLine(resp.Value), maxCodeRunes)
			for _, i := range by[id] {
				items[i].Responses = append(items[i].Responses, resp)
			}
		}
		if err := rs.Close(); err != nil {
			return err
		}
	}
	if ok, err := r.hasTable(ctx, "feedback_hindsight"); err != nil {
		return err
	} else if ok {
		hs, err := r.DB.QueryContext(ctx, `SELECT intervention_id, horizon, pct_change, portfolio_pct, trades FROM feedback_hindsight WHERE intervention_id IN (`+in+`) ORDER BY computed_at_ms`, keys...)
		if err != nil {
			return fmt.Errorf("dashboard: read hindsight: %w", err)
		}
		for hs.Next() {
			var id string
			var h Hindsight
			var pct, port sql.NullFloat64
			var trades sql.NullInt64
			if err := hs.Scan(&id, &h.Horizon, &pct, &port, &trades); err != nil {
				_ = hs.Close()
				return fmt.Errorf("dashboard: read hindsight: %w", err)
			}
			h.Horizon = clip(cleanLine(h.Horizon), maxCodeRunes)
			if pct.Valid {
				h.PctChange = &pct.Float64
			}
			if port.Valid {
				h.PortfolioPct = &port.Float64
			}
			if trades.Valid {
				n := int(trades.Int64)
				h.Trades = &n
			}
			for _, i := range by[id] {
				items[i].Hindsight = append(items[i].Hindsight, h)
			}
		}
		return hs.Close()
	}
	return nil
}

func rowCursor(atMs, rowid int64) string { return fmt.Sprintf("%d.%d", atMs, rowid) }

func parseRowCursor(s string) (atMs, rowid int64, err error) {
	a, b, ok := strings.Cut(s, ".")
	if atMs, err = strconv.ParseInt(a, 10, 64); !ok || err != nil || atMs < 0 {
		return 0, 0, badf("not a cursor")
	}
	if rowid, err = strconv.ParseInt(b, 10, 64); err != nil || rowid < 0 {
		return 0, 0, badf("not a cursor")
	}
	return atMs, rowid, nil
}

// cleanLine collapses a value to one printable line.
func cleanLine(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return ' '
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

func clip(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max]) + "..."
}
