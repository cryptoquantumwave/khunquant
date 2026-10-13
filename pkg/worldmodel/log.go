package worldmodel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// signalPrefix marks a change-log entry that is a fired signal: its subject is the
// series, its key is signalPrefix + detector and its new value is the signal's
// value.
const signalPrefix = "signal:"

// logEntry is one row of the change log.
type logEntry struct {
	Seq      int64
	AtMs     int64
	Subject  string
	Key      string
	Old, New json.RawMessage
	Trust    proactive.Trust
}

var jsonNull = json.RawMessage("null")

func marshalValue(v any) json.RawMessage {
	if v == nil {
		return jsonNull
	}
	b, err := json.Marshal(v)
	if err != nil {
		return jsonNull
	}
	return b
}

// logChange appends to the change log. Sequence numbers are assigned here, in
// memory, so a mark can name a position in the log before the log is written. The
// caller holds the write lock. At the memory bound the OLDEST entries are dropped
// and counted.
func (s *Store) logChange(at time.Time, subject, key string, old, nw any, trust proactive.Trust) {
	s.seq++
	s.buf = append(s.buf, logEntry{
		Seq: s.seq, AtMs: at.UnixMilli(), Subject: subject, Key: key,
		Old: marshalValue(old), New: marshalValue(nw), Trust: trust,
	})
	s.c.changesLogged.Add(1)
	if over := len(s.buf) - MaxLogBuffer; over > 0 {
		s.lowSeq = max(s.lowSeq, s.buf[over-1].Seq+1)
		n := copy(s.buf, s.buf[over:])
		clear(s.buf[n:])
		s.buf = s.buf[:n]
		s.c.changesDropped.Add(uint64(over))
		s.noteDropped.Store(true)
	}
}

// Cursor is the sequence number of the newest change-log entry. A mark set at the
// cursor sees only what happens after.
func (s *Store) Cursor() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.seq
}

// SetMark names "now" in the change log. SinceMark(name) later returns what changed
// after it. Marks are kept in memory and written at the next checkpoint.
func (s *Store) SetMark(ctx context.Context, name string) error {
	return s.setMark(ctx, name, -1, true, 0)
}

// EnsureMark is SetMark unless the mark already exists.
func (s *Store) EnsureMark(ctx context.Context, name string) error {
	return s.setMark(ctx, name, -1, false, 0)
}

// SetMarkAt moves a mark forward to a position in the log taken earlier from
// Cursor, never backward and never past the cursor. A reader uses it to say "I
// have seen everything up to here" without losing what arrived while it read.
func (s *Store) SetMarkAt(ctx context.Context, name string, seq int64) error {
	return s.setMark(ctx, name, seq, true, 0)
}

// SetMarkAtTime is SetMarkAt for a reader that took the position at a known time:
// the mark's time (MarkInfo) becomes at, not the moment of this call, so that the
// window from the mark to the next read starts when the position was taken, and
// what happened while the reader worked is inside it. The time moves forward only,
// and moves even when the position does not (nothing was logged since).
func (s *Store) SetMarkAtTime(ctx context.Context, name string, seq int64, at time.Time) error {
	return s.setMark(ctx, name, seq, true, at.UnixMilli())
}

// setMark sets a mark. A seq below zero means the cursor. atMs is the time to
// record; zero or less means now.
func (s *Store) setMark(_ context.Context, name string, seq int64, replace bool, atMs int64) error {
	if !validToken(name, 32) {
		return fmt.Errorf("worldmodel: invalid mark name")
	}
	explicitTime := atMs > 0
	if !explicitTime {
		atMs = s.clock.Now().UnixMilli()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, exists := s.marks[name]
	if exists && !replace {
		return nil
	}
	if !exists && len(s.marks) >= MaxMarks {
		return fmt.Errorf("worldmodel: too many marks")
	}
	at := s.seq
	if seq >= 0 {
		at = min(seq, s.seq)
		if exists && at <= cur.seq {
			// The position stays; a later explicit time still moves (see SetMarkAtTime).
			if !explicitTime || atMs <= cur.atMs {
				return nil
			}
			at = cur.seq
		}
	}
	s.marks[name] = markState{seq: at, atMs: atMs}
	s.ver++
	s.dirtyMarks[name] = s.ver
	return nil
}

// MarkInfo is where a mark stands: the position in the change log it names and
// the time it was last set or moved. ok is false for a mark that was never set. The
// time is the store's clock, so it is the start of the window SinceMark reads.
func (s *Store) MarkInfo(name string) (seq int64, at time.Time, ok bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.marks[name]
	if !ok {
		return 0, time.Time{}, false
	}
	return m.seq, time.UnixMilli(m.atMs), true
}

// SinceMark returns what changed after the mark, aggregated by (subject, key): the
// first old value and the last new value, so that a fact that moved three times
// reads as one move, and a fact that ended where it started does not appear at all.
// Fired signals are aggregated per series and detector (the strongest value and a
// count in Detail["count"]). The result is sorted.
//
// A mark that was never set gives ErrNoMark. A mark older than the change log
// (pruned after seven days, or dropped at the memory bound) returns what is still
// there with Truncated set. The database is read with ctx and no lock held.
func (s *Store) SinceMark(ctx context.Context, name string) (Diff, error) {
	now := s.clock.Now()
	s.mu.RLock()
	m, ok := s.marks[name]
	var mem []logEntry
	for i := range s.buf {
		if s.buf[i].Seq > m.seq {
			mem = append(mem, s.buf[i])
		}
	}
	flushed, low := s.flushedSeq, s.lowSeq
	s.mu.RUnlock()
	if !ok {
		return Diff{}, ErrNoMark
	}

	truncated := m.seq+1 < low
	var all []logEntry
	if s.db != nil && m.seq < flushed {
		rows, err := s.readLog(ctx, m.seq, flushed)
		if err != nil {
			return Diff{}, err
		}
		if len(rows) >= maxScan {
			truncated = true
		}
		all = rows
	}
	all = append(all, mem...)
	d := aggregate(all)
	d.At, d.Truncated = now, truncated
	return d, nil
}

// aggregate folds log entries (in sequence order) into a Diff.
func aggregate(entries []logEntry) Diff {
	type key struct{ subject, key string }
	type acc struct {
		old, nw json.RawMessage
		trust   proactive.Trust
		n       int
		best    float64
	}
	by := map[key]*acc{}
	var order []key
	for _, e := range entries {
		k := key{e.Subject, e.Key}
		a := by[k]
		if a == nil {
			a = &acc{old: e.Old, trust: e.Trust}
			by[k] = a
			order = append(order, k)
		} else {
			a.trust = proactive.MinTrust(a.trust, e.Trust)
		}
		a.nw = e.New
		a.n++
		if strings.HasPrefix(e.Key, signalPrefix) {
			var v float64
			if json.Unmarshal(e.New, &v) == nil && math.Abs(v) >= math.Abs(a.best) {
				a.best = v
			}
		}
	}
	var d Diff
	for _, k := range order {
		a := by[k]
		if det, ok := strings.CutPrefix(k.key, signalPrefix); ok {
			d.Signals = append(d.Signals, Signal{
				Series: k.subject, Detector: det, Value: a.best, Fired: true,
				Detail: map[string]float64{"count": float64(a.n)}, Trust: a.trust,
			})
			continue
		}
		if bytes.Equal(a.old, a.nw) {
			continue // it ended where it started
		}
		d.Changes = append(d.Changes, Change{Subject: k.subject, Key: k.key, Old: unmarshalValue(a.old), New: unmarshalValue(a.nw), Trust: a.trust})
	}
	sort.Slice(d.Changes, func(i, j int) bool {
		a, b := d.Changes[i], d.Changes[j]
		if a.Subject != b.Subject {
			return a.Subject < b.Subject
		}
		return a.Key < b.Key
	})
	sort.Slice(d.Signals, func(i, j int) bool {
		a, b := d.Signals[i], d.Signals[j]
		if a.Series != b.Series {
			return a.Series < b.Series
		}
		return a.Detector < b.Detector
	})
	return d
}

func unmarshalValue(b json.RawMessage) any {
	if len(b) == 0 || bytes.Equal(b, jsonNull) {
		return nil
	}
	var v any
	if json.Unmarshal(b, &v) != nil {
		return nil
	}
	return v
}

// readLog reads the rows with after < seq <= upTo, fully, before returning.
func (s *Store) readLog(ctx context.Context, after, upTo int64) ([]logEntry, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT seq, at_ms, subject, key, old_json, new_json, trust FROM wm_changes WHERE seq > ? AND seq <= ? ORDER BY seq LIMIT ?`,
		after, upTo, maxScan)
	if err != nil {
		return nil, fmt.Errorf("worldmodel: read change log: %w", err)
	}
	defer rows.Close()
	var out []logEntry
	for rows.Next() {
		var e logEntry
		var oldS, newS string
		var trust int
		if err := rows.Scan(&e.Seq, &e.AtMs, &e.Subject, &e.Key, &oldS, &newS, &trust); err != nil {
			return nil, fmt.Errorf("worldmodel: read change log: %w", err)
		}
		e.Old, e.New, e.Trust = json.RawMessage(oldS), json.RawMessage(newS), trustFrom(trust)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("worldmodel: read change log: %w", err)
	}
	return out, nil
}

// trustFrom reads a stored trust; anything out of range is untrusted.
func trustFrom(v int) proactive.Trust {
	t := proactive.Trust(v)
	if v < int(proactive.TrustUntrusted) || t > proactive.TrustUserAuthored {
		return proactive.TrustUntrusted
	}
	return t
}
