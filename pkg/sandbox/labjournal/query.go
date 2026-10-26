package labjournal

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// MaxLimit is the most entries one Query returns.
const MaxLimit = 1000

// Query selects entries. Kinds are exact ("chat.in") or a prefix ("chat.*"); Since and
// Until bound the sim time; AfterSeq is the cursor (Seq > AfterSeq). The result is the
// oldest Limit matches, in Seq order.
type Query struct {
	Kinds    []string
	Since    time.Time
	Until    time.Time
	Subject  string
	AfterSeq int64
	Limit    int // default 100, at most MaxLimit
}

// Query flushes first, so an entry appended before the call is returned.
func (j *Journal) Query(ctx context.Context, q Query) ([]Entry, error) {
	if err := j.Flush(ctx); err != nil {
		return nil, err
	}
	var (
		where, kinds []string
		args         []any
	)
	add := func(cond string, arg any) { where, args = append(where, cond), append(args, arg) }
	for _, k := range q.Kinds {
		if p, ok := strings.CutSuffix(k, ".*"); ok {
			kinds, args = append(kinds, `kind LIKE ? ESCAPE '\'`), append(args, likeEscape(p+".")+"%")
		} else {
			kinds, args = append(kinds, `kind = ?`), append(args, k)
		}
	}
	if len(kinds) > 0 { // the kind terms are bound first: their arguments are already in args
		where = append(where, "("+strings.Join(kinds, " OR ")+")")
	}
	if !q.Since.IsZero() {
		add(`sim_ns >= ?`, q.Since.UnixNano())
	}
	if !q.Until.IsZero() {
		add(`sim_ns <= ?`, q.Until.UnixNano())
	}
	if q.Subject != "" {
		add(`subject = ?`, q.Subject)
	}
	if q.AfterSeq > 0 {
		add(`seq > ?`, q.AfterSeq)
	}
	text := "SELECT seq, sim_ns, wall_ns, kind, source, subject, payload FROM lab_journal"
	if len(where) > 0 {
		text += " WHERE " + strings.Join(where, " AND ")
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}
	text += fmt.Sprintf(" ORDER BY seq LIMIT %d", min(limit, MaxLimit))

	rows, err := j.db.QueryContext(ctx, text, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var e Entry
		var sim, wall int64
		var payload []byte
		if err := rows.Scan(&e.Seq, &sim, &wall, &e.Kind, &e.Source, &e.Subject, &payload); err != nil {
			return nil, err
		}
		e.SimTime, e.WallTime, e.Payload = time.Unix(0, sim), time.Unix(0, wall), payload
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}
