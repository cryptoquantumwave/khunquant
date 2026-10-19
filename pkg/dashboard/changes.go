package dashboard

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/identity"
)

// ApprovalItem is a proposal as the live stream shows it: a projection, with no params,
// hash, nonce, rationale, quote, code or card ids. Summary is written by code.
type ApprovalItem struct {
	ID       string    `json:"id"`
	At       time.Time `json:"at"` // the last change the table timestamps
	Kind     string    `json:"kind"`
	Group    string    `json:"group"` // T1, T2, T3, memory or reflection
	Reducing bool      `json:"reducing"`
	Status   string    `json:"status"`
	Origin   string    `json:"origin"`
	Summary  string    `json:"summary"`
	// DecidedBy is "web", "channel" (the owner in a chat) or "" (not decided).
	DecidedBy string `json:"decided_by"`
}

// Change is one item of the feed. Type is "attention.decision" or "approval.<status>";
// Data is an Intervention or an ApprovalItem. Cursor is the feed's position after this
// item, to resume from it.
type Change struct {
	Type   string `json:"type"`
	Cursor string `json:"cursor"`
	Data   any    `json:"data"`
}

// feedCursor is where the feed stands: the last decision's rowid, and the last proposal
// change as (time, rowid). It is the cursor's whole content, so that nothing is kept
// between polls. A decision's rowid (not its at_ms, which can commit out of order) is
// the position of the decision stream; a proposal is updated in place, so its position is
// its latest timestamp (created, decided or finished) with the rowid as the tiebreak.
// The executing step carries no timestamp: it is not a change of its own.
type feedCursor struct{ dec, apMs, apRow int64 }

func (c feedCursor) String() string { return fmt.Sprintf("%d.%d.%d", c.dec, c.apMs, c.apRow) }

func parseFeedCursor(s string) (feedCursor, error) {
	parts := strings.Split(s, ".")
	var v [3]int64
	if len(parts) != 3 {
		return feedCursor{}, badf("not a cursor")
	}
	for i, p := range parts {
		n, err := strconv.ParseInt(p, 10, 64)
		if err != nil || n < 0 {
			return feedCursor{}, badf("not a cursor")
		}
		v[i] = n
	}
	return feedCursor{v[0], v[1], v[2]}, nil
}

const proposalChanged = `MAX(created_at_ms, COALESCE(decided_at_ms, 0), COALESCE(finished_at_ms, 0))`

// Changes returns the decisions and proposal changes after cursor, oldest first, at most
// limit (MaxLimit at most), and the cursor to continue from. An empty cursor returns no
// items and the current head, so a new stream starts at "now". Each item is returned
// once: the cursor moves only over the items returned.
func (r *Reader) Changes(ctx context.Context, cursor string, limit int) (items []Change, next string, err error) {
	if limit <= 0 || limit > MaxLimit {
		limit = MaxLimit
	}
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	hasDec, err := r.hasTable(ctx, "attention_decisions")
	if err != nil {
		return nil, "", err
	}
	hasAp, err := r.hasTable(ctx, "proposals")
	if err != nil {
		return nil, "", err
	}
	if cursor == "" {
		var head feedCursor
		if hasDec {
			err = r.DB.QueryRowContext(ctx, `SELECT COALESCE(MAX(rowid), 0) FROM attention_decisions`).Scan(&head.dec)
		}
		if err == nil && hasAp {
			err = r.DB.QueryRowContext(ctx, `SELECT c, rowid FROM (SELECT rowid, `+proposalChanged+` AS c FROM proposals)
				ORDER BY c DESC, rowid DESC LIMIT 1`).Scan(&head.apMs, &head.apRow)
			if errors.Is(err, sql.ErrNoRows) {
				err = nil
			}
		}
		return []Change{}, head.String(), err
	}
	cur, err := parseFeedCursor(cursor)
	if err != nil {
		return nil, "", err
	}
	var ds []Intervention
	var aps []proposalRow
	if hasDec {
		if ds, err = r.decisions(ctx, `rowid > ?`, `rowid`, limit, cur.dec); err != nil {
			return nil, "", err
		}
	}
	if hasAp {
		if aps, err = r.proposals(ctx, cur, limit); err != nil {
			return nil, "", err
		}
	}
	// Merge by time, keeping each stream in its own order: the cursor of a stream is the
	// last item taken from it, so a stream must never be taken out of order.
	items = make([]Change, 0, min(limit, len(ds)+len(aps)))
	for len(items) < limit && (len(ds) > 0 || len(aps) > 0) {
		if len(aps) == 0 || (len(ds) > 0 && !aps[0].item.At.Before(ds[0].At)) {
			cur.dec = ds[0].rowid
			items = append(items, Change{Type: "attention.decision", Cursor: cur.String(), Data: ds[0]})
			ds = ds[1:]
		} else {
			cur.apMs, cur.apRow = aps[0].ms, aps[0].row
			items = append(items, Change{Type: "approval." + aps[0].item.Status, Cursor: cur.String(), Data: aps[0].item})
			aps = aps[1:]
		}
	}
	return items, cur.String(), nil
}

type proposalRow struct {
	item    ApprovalItem
	ms, row int64
}

// proposals reads the proposals changed after the cursor, oldest change first.
func (r *Reader) proposals(ctx context.Context, cur feedCursor, limit int) ([]proposalRow, error) {
	type raw struct {
		row, ms, reducing                      int64
		kind, status, origin, summary, decider string
		created                                int64
	}
	rows, err := r.DB.QueryContext(ctx,
		`SELECT rowid, `+proposalChanged+`, reducing, kind, status, origin, summary, decided_by, created_at_ms FROM proposals
		  WHERE `+proposalChanged+` > ? OR (`+proposalChanged+` = ? AND rowid > ?)
		  ORDER BY `+proposalChanged+`, rowid LIMIT ?`, cur.apMs, cur.apMs, cur.apRow, limit)
	if err != nil {
		return nil, fmt.Errorf("dashboard: read proposals: %w", err)
	}
	var raws []raw
	for rows.Next() {
		var x raw
		if err := rows.Scan(&x.row, &x.ms, &x.reducing, &x.kind, &x.status, &x.origin, &x.summary, &x.decider, &x.created); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("dashboard: read proposals: %w", err)
		}
		raws = append(raws, x)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	out := make([]proposalRow, 0, len(raws))
	for _, x := range raws {
		it := ApprovalItem{
			ID: "a" + strconv.FormatInt(x.row, 10), At: time.UnixMilli(x.ms).UTC(), Kind: clip(cleanLine(x.kind), maxCodeRunes),
			Group: ApprovalGroup(approval.Kind(x.kind), x.reducing == 1), Reducing: x.reducing == 1,
			Status: clip(cleanLine(x.status), maxCodeRunes), Origin: clip(cleanLine(x.origin), maxCodeRunes),
			Summary: clip(cleanLine(x.summary), maxSummaryRunes),
		}
		switch x.decider {
		case "":
		case identity.WebOwner:
			it.DecidedBy = "web"
		default:
			it.DecidedBy = "channel"
		}
		out = append(out, proposalRow{item: it, ms: x.ms, row: x.row})
	}
	return out, nil
}
