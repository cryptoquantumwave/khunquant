package labjournal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// MaxWait is the longest one WaitFor holds a caller: the HTTP server writes with a
// 30 s timeout, so longer waits are loops of calls. lab.CallTimeout is this value.
const MaxWait = 25 * time.Second

const (
	maxWaitEntries = 100                    // entries a WaitResult carries; Count is exact
	ctxMargin      = 500 * time.Millisecond // WaitFor ends this long before its context's deadline
)

// ErrBadMatch is returned (wrapped) for a Match that cannot be evaluated.
var ErrBadMatch = errors.New("labjournal: bad match")

// pollEvery is the fallback re-check when no commit signal arrives (a variable for tests).
var pollEvery = 250 * time.Millisecond

// Match is an expectation on journal entries. Fields are dotted paths into the stored
// payload ("event.payload.symbol"; an observer entry is {"recordable":…,"event":{…}});
// a string value matches by substring, any other value by equality.
//
// Count is how many entries must match: "1" (exactly one), ">= 1" (the default), ">", "<=",
// "<", "==". A count with an upper bound and no lower one ("0", "<= 2") is an absence
// check: it holds only once the whole window has passed, unless the bound is already
// exceeded, which ends the wait with matched=false at once.
type Match struct {
	Kind    string         `json:"kind"` // exact or "prefix.*"; empty = any
	Subject string         `json:"subject,omitempty"`
	Fields  map[string]any `json:"fields,omitempty"`
	Count   string         `json:"count,omitempty"`
}

// WaitResult is what WaitFor saw. NextSeq is the cursor to continue from.
type WaitResult struct {
	Matched bool    `json:"matched"`
	Count   int     `json:"count"`
	Entries []Entry `json:"entries"`
	NextSeq int64   `json:"next_seq"`
}

// parseCount reads a Match.Count into the range [lo, hi] of acceptable counts (hi < 0:
// no upper bound).
func parseCount(s string) (lo, hi int, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 1, -1, nil
	}
	op := "=="
	for _, p := range []string{">=", "<=", "==", ">", "<"} {
		if t, ok := strings.CutPrefix(s, p); ok {
			op, s = p, strings.TrimSpace(t)
			break
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 || (op == "<" && n == 0) {
		return 0, 0, fmt.Errorf("%w: count %q", ErrBadMatch, s)
	}
	switch op {
	case ">=":
		return n, -1, nil
	case ">":
		return n + 1, -1, nil
	case "<=":
		return 0, n, nil
	case "<":
		return 0, n - 1, nil
	}
	return n, n, nil
}

// Bounds is the range [lo, hi] of entry counts that satisfy m.Count (hi < 0: no upper
// bound). An absence check has lo == 0 and hi >= 0: it holds only once the window is over.
func (m Match) Bounds() (lo, hi int, err error) { return parseCount(m.Count) }

// clampWait bounds a requested wait to [0, MaxWait].
func clampWait(d time.Duration) time.Duration { return min(max(d, 0), MaxWait) }

// WaitFor returns when the entries after since that satisfy m meet m.Count, or after
// wall (at most MaxWait, and never past the context's deadline), whichever is first.
// It re-reads on every journal commit, with pollEvery as a fallback. A context that
// expires ends the wait with what was seen; any other cancellation is an error.
func (j *Journal) WaitFor(ctx context.Context, m Match, since int64, wall time.Duration) (WaitResult, error) {
	lo, hi, err := parseCount(m.Count)
	if err != nil {
		return WaitResult{}, err
	}
	var fields map[string]any // the fields through JSON, so a Go int equals the stored number
	if b, err := json.Marshal(m.Fields); err != nil || json.Unmarshal(b, &fields) != nil {
		return WaitResult{}, fmt.Errorf("%w: fields are not JSON", ErrBadMatch)
	}
	q := Query{Subject: m.Subject, Limit: MaxLimit}
	if m.Kind != "" {
		q.Kinds = []string{m.Kind}
	}
	end := time.Now().Add(clampWait(wall))
	if dl, ok := ctx.Deadline(); ok && dl.Add(-ctxMargin).Before(end) {
		end = dl.Add(-ctxMargin)
	}

	res := WaitResult{NextSeq: since}
	for {
		sig := j.commitSignal()
		for {
			q.AfterSeq = res.NextSeq
			es, err := j.Query(ctx, q)
			if err != nil {
				return res, err
			}
			for _, e := range es {
				res.NextSeq = e.Seq
				if fieldsMatch(e.Payload, fields) {
					if res.Count++; len(res.Entries) < maxWaitEntries {
						res.Entries = append(res.Entries, e)
					}
				}
			}
			if len(es) < MaxLimit {
				break
			}
		}
		holds := res.Count >= lo && (hi < 0 || res.Count <= hi)
		res.Matched = holds
		if (hi >= 0 && res.Count > hi) || (holds && (lo > 0 || hi < 0)) {
			return res, nil
		}
		left := time.Until(end)
		if left <= 0 {
			return res, nil
		}
		t := time.NewTimer(min(left, pollEvery))
		select {
		case <-sig:
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return res, nil
			}
			return res, ctx.Err()
		}
		t.Stop()
	}
}

// fieldsMatch reports whether every dotted path in want holds in the JSON payload.
func fieldsMatch(payload json.RawMessage, want map[string]any) bool {
	if len(want) == 0 {
		return true
	}
	var doc any
	if json.Unmarshal(payload, &doc) != nil {
		return false
	}
	for path, w := range want {
		got, ok := lookup(doc, path)
		if !ok {
			return false
		}
		if ws, isStr := w.(string); isStr {
			if gs, isStr := got.(string); !isStr || !strings.Contains(gs, ws) {
				return false
			}
		} else if !reflect.DeepEqual(got, w) {
			return false
		}
	}
	return true
}

func lookup(v any, path string) (any, bool) {
	for _, part := range strings.Split(path, ".") {
		switch t := v.(type) {
		case map[string]any:
			var ok bool
			if v, ok = t[part]; !ok {
				return nil, false
			}
		case []any:
			i, err := strconv.Atoi(part)
			if err != nil || i < 0 || i >= len(t) {
				return nil, false
			}
			v = t[i]
		default:
			return nil, false
		}
	}
	return v, true
}
