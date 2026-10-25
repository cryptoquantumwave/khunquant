package snapshot

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"
)

// MaxUnpriced bounds the list of unpriced assets an automatic snapshot records.
const MaxUnpriced = 500

// unpricedPrefix leads the note of an automatic snapshot that was saved although
// some assets could not be priced: AutoNote, then this, then the sorted list
// "exchange/account:ASSET" separated by commas.
const unpricedPrefix = AutoNote + "; unpriced="

// AutoNoteWith returns the note of an automatic snapshot whose valuation leaves
// out the given assets ("exchange/account:ASSET"). The list is sorted, without
// duplicates, and at most MaxUnpriced long; an entry that is empty or holds a comma
// or a line break is dropped. With nothing to record it is AutoNote.
func AutoNoteWith(unpriced []string) string {
	clean := make([]string, 0, len(unpriced))
	for _, u := range unpriced {
		if u == "" || strings.ContainsAny(u, ",\r\n") {
			continue
		}
		clean = append(clean, u)
	}
	sort.Strings(clean)
	out := clean[:0]
	for i, u := range clean {
		if i == 0 || u != clean[i-1] {
			out = append(out, u)
		}
	}
	if len(out) == 0 {
		return AutoNote
	}
	if len(out) > MaxUnpriced {
		out = out[:MaxUnpriced]
	}
	return unpricedPrefix + strings.Join(out, ",")
}

// IsAutoNote reports whether note is the note of an automatic snapshot: exactly
// AutoNote, or AutoNote followed by the unpriced list written by AutoNoteWith. It
// is an exact match on that shape, not a substring test.
func IsAutoNote(note string) bool {
	return note == AutoNote || strings.HasPrefix(note, unpricedPrefix)
}

// UnpricedFromNote returns the unpriced assets recorded in the note of an
// automatic snapshot, sorted, or nil when it records none (or is not an automatic
// note).
func UnpricedFromNote(note string) []string {
	rest, ok := strings.CutPrefix(note, unpricedPrefix)
	if !ok || rest == "" {
		return nil
	}
	return strings.Split(rest, ",")
}

// IsAuto reports whether a snapshot is an automatic one: the reserved label and
// an automatic note. A row that carries the label with any other note (one saved
// before the label was reserved) is not.
func IsAuto(label, note string) bool { return label == LabelAuto && IsAutoNote(note) }

// LatestAuto returns the most recent automatic snapshot with its positions, or nil
// when there is none. It is the one read that returns automatic rows without the
// caller naming their label: QuerySnapshots and SnapshotSummary keep hiding them.
// It exists for the one caller that decides for itself whether an automatic row is
// newer than the owner's own (the portfolio section of a chat turn's prompt).
func (s *Store) LatestAuto(ctx context.Context) (*Snapshot, error) {
	cond, args := autoSQL("")
	var id int64
	err := s.db.QueryRowContext(ctx,
		`SELECT id FROM snapshots WHERE `+cond+` ORDER BY taken_at DESC, id DESC LIMIT 1`, args...,
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s.GetSnapshot(ctx, id)
}

// autoSQL is the SQL condition that selects automatic snapshots, for a table
// alias such as "s", and its arguments.
func autoSQL(alias string) (string, []any) {
	p := alias
	if p != "" {
		p += "."
	}
	return "(" + p + "label = ? AND (" + p + "note = ? OR substr(" + p + "note, 1, ?) = ?))",
		[]any{LabelAuto, AutoNote, len(unpricedPrefix), unpricedPrefix}
}

// excludeAuto adds to a WHERE list (over the snapshots table aliased "s") the
// condition that leaves automatic snapshots out, unless the filter's label is
// exactly the reserved one. Every other filter, whether by time, source, asset or
// another label, therefore never sees them: they are baselines for the pipeline,
// not the owner's snapshots, and at 96 a day they would bury those.
func excludeAuto(label string, where []string, args []any) ([]string, []any) {
	if label == LabelAuto {
		return where, args
	}
	cond, a := autoSQL("s")
	return append(where, "NOT "+cond), append(args, a...)
}
