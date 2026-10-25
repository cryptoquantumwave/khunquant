package worldmodel

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex"
	"github.com/cryptoquantumwave/khunquant/pkg/worldmodel/detect"
)

// Component is the migration component name of the world model's tables.
const Component = "worldmodel"

// migrations declares the four tables. wm_changes has one column beyond the
// plan's: trust, so that a change read back from the log carries the trust of its
// inputs as it did when it was made.
var migrations = []sqlitex.Migration{{
	Component: Component,
	Version:   1,
	SQL: `CREATE TABLE wm_facts (
	subject    TEXT    NOT NULL,
	key        TEXT    NOT NULL,
	value_json TEXT    NOT NULL,
	at_ms      INTEGER NOT NULL,
	source     TEXT    NOT NULL DEFAULT '',
	trust      INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (subject, key)
);
CREATE TABLE wm_detectors (
	series     TEXT    NOT NULL,
	detector   TEXT    NOT NULL,
	state_json TEXT    NOT NULL,
	updated_ms INTEGER NOT NULL,
	PRIMARY KEY (series, detector)
);
CREATE TABLE wm_changes (
	seq      INTEGER PRIMARY KEY AUTOINCREMENT,
	at_ms    INTEGER NOT NULL,
	subject  TEXT    NOT NULL,
	key      TEXT    NOT NULL,
	old_json TEXT    NOT NULL,
	new_json TEXT    NOT NULL,
	trust    INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_wm_changes_at ON wm_changes(at_ms);
CREATE TABLE wm_marks (
	name  TEXT    PRIMARY KEY,
	seq   INTEGER NOT NULL,
	at_ms INTEGER NOT NULL
);`,
}}

// Row identities. A row is "dirty" from the moment its state changes until a
// checkpoint has written that state; a row whose state no longer exists (an
// evicted series) is dirty until the checkpoint has deleted it.
type rowKind uint8

const (
	rkPrice rowKind = iota + 1
	rkPortfolio
	rkFutures
	rkPlan
	rkFill
	rkDay
)

type rowKey struct {
	kind rowKind
	a, b string
	n    int64
}

// The detector rows of one series.
const (
	detMinute uint8 = iota + 1
	detHour
	detCUSUM
)

var detNames = map[uint8]string{detMinute: "ret_1m", detHour: "ret_1h", detCUSUM: "cusum_1m"}

type detKey struct {
	k     seriesKey
	which uint8
}

type closeInfo = detect.BucketClose

type factRow struct {
	subject, key, value, source string
	atMs                        int64
	trust                       int
}

type detRow struct {
	series, detector, state string
	updatedMs               int64
}

type markRow struct {
	name      string
	seq, atMs int64
}

// subjectKey is the wm_facts (subject, key) of a row identity.
func (k rowKey) subjectKey() (subject, key string) {
	switch k.kind {
	case rkPrice:
		return "price:" + k.a + ":" + k.b, "last"
	case rkPortfolio:
		return "portfolio", "state"
	case rkFutures:
		return "futures:" + k.a, "positions"
	case rkPlan:
		return "plan:" + k.a + ":" + strconv.FormatInt(k.n, 10), "view"
	case rkFill:
		return "fill:" + k.a + ":" + k.b, "last"
	default:
		return "clock", "day"
	}
}

// ---- opening --------------------------------------------------------------

func (s *Store) open(ctx context.Context) error {
	if err := sqlitex.Migrate(ctx, s.db, migrations...); err != nil {
		return fmt.Errorf("worldmodel: %w", err)
	}
	facts, err := s.readFacts(ctx)
	if err != nil {
		return err
	}
	dets, err := s.readDetectors(ctx)
	if err != nil {
		return err
	}
	marks, err := s.readMarks(ctx)
	if err != nil {
		return err
	}
	maxSeq, minSeq, tableSeq, err := s.readSeqs(ctx)
	if err != nil {
		return err
	}

	now := s.clock.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range facts {
		s.restoreFact(f)
	}
	for _, r := range dets {
		s.restoreDetector(r, now)
	}
	// A detector row whose series was not restored is an orphan; the next
	// checkpoint deletes it.
	for _, r := range dets {
		if p, ok := strings.CutPrefix(r.series, "price:"); ok {
			if prov, sym, ok := strings.Cut(p, ":"); ok {
				if _, live := s.series[seriesKey{prov, sym}]; !live {
					for w := range detNames {
						s.touchDet(detKey{seriesKey{prov, sym}, w})
					}
				}
			}
		}
	}
	s.marks = marks
	s.seq = max(maxSeq, tableSeq)
	for _, m := range marks {
		s.seq = max(s.seq, m.seq)
	}
	s.flushedSeq = s.seq
	if minSeq > 0 {
		s.lowSeq = minSeq
	} else {
		s.lowSeq = s.seq + 1
	}
	return nil
}

type storedFact struct {
	subject, key, value, source string
	atMs                        int64
	trust                       int
}

func (s *Store) readFacts(ctx context.Context) ([]storedFact, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT subject, key, value_json, at_ms, source, trust FROM wm_facts`)
	if err != nil {
		return nil, fmt.Errorf("worldmodel: load facts: %w", err)
	}
	defer rows.Close()
	var out []storedFact
	for rows.Next() {
		var f storedFact
		if err := rows.Scan(&f.subject, &f.key, &f.value, &f.atMs, &f.source, &f.trust); err != nil {
			return nil, fmt.Errorf("worldmodel: load facts: %w", err)
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("worldmodel: load facts: %w", err)
	}
	return out, nil
}

func (s *Store) readDetectors(ctx context.Context) ([]detRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT series, detector, state_json, updated_ms FROM wm_detectors`)
	if err != nil {
		return nil, fmt.Errorf("worldmodel: load detectors: %w", err)
	}
	defer rows.Close()
	var out []detRow
	for rows.Next() {
		var r detRow
		if err := rows.Scan(&r.series, &r.detector, &r.state, &r.updatedMs); err != nil {
			return nil, fmt.Errorf("worldmodel: load detectors: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("worldmodel: load detectors: %w", err)
	}
	return out, nil
}

func (s *Store) readMarks(ctx context.Context) (map[string]markState, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, seq, at_ms FROM wm_marks`)
	if err != nil {
		return nil, fmt.Errorf("worldmodel: load marks: %w", err)
	}
	defer rows.Close()
	out := map[string]markState{}
	for rows.Next() {
		var name string
		var m markState
		if err := rows.Scan(&name, &m.seq, &m.atMs); err != nil {
			return nil, fmt.Errorf("worldmodel: load marks: %w", err)
		}
		if validToken(name, 32) && len(out) < MaxMarks {
			out[name] = m
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("worldmodel: load marks: %w", err)
	}
	return out, nil
}

// readSeqs returns the highest and lowest sequence number in the change log and
// the highest AUTOINCREMENT ever issued (which survives pruning of every row).
func (s *Store) readSeqs(ctx context.Context) (maxSeq, minSeq, tableSeq int64, err error) {
	if err = s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0), COALESCE(MIN(seq), 0) FROM wm_changes`).Scan(&maxSeq, &minSeq); err != nil {
		return 0, 0, 0, fmt.Errorf("worldmodel: load change log bounds: %w", err)
	}
	err = s.db.QueryRowContext(ctx, `SELECT seq FROM sqlite_sequence WHERE name = 'wm_changes'`).Scan(&tableSeq)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, 0, 0, fmt.Errorf("worldmodel: load change log bounds: %w", err)
	}
	return maxSeq, minSeq, tableSeq, nil
}

// restoreFact loads one persisted fact. A row it cannot make sense of is skipped.
// The caller holds the write lock.
func (s *Store) restoreFact(f storedFact) {
	at := time.UnixMilli(f.atMs).UTC()
	trust := trustFrom(f.trust)
	ok := false
	switch {
	case strings.HasPrefix(f.subject, "price:"):
		prov, sym, found := strings.Cut(strings.TrimPrefix(f.subject, "price:"), ":")
		var v struct{ P float64 }
		if found && validToken(prov, MaxTokenLen) && validSymbol(sym, MaxSymbolLen) && len(s.series) < MaxSeries &&
			json.Unmarshal([]byte(f.value), &v) == nil && finite(v.P) && v.P > 0 {
			sr := newSeries()
			sr.price, sr.at, sr.trust, sr.src = v.P, at, trust, f.source
			s.series[seriesKey{prov, sym}] = sr
			ok = true
		}
	case f.subject == "portfolio":
		var p portfolioState
		if json.Unmarshal([]byte(f.value), &p) == nil && validPortfolio(&p) {
			p.Trust, p.Src = trust, f.source
			s.pf = &p
			ok = true
		}
	case strings.HasPrefix(f.subject, "futures:"):
		acct := strings.TrimPrefix(f.subject, "futures:")
		var vs []FuturesView
		if validSymbol(acct, 2*MaxTokenLen+1) && len(s.futures) < MaxFuturesAccounts && json.Unmarshal([]byte(f.value), &vs) == nil && len(vs) <= MaxFuturesPositions {
			fa := &futuresAccount{at: at, trust: trust, src: f.source, positions: map[string]FuturesView{}}
			for _, v := range vs {
				if validSymbol(v.Symbol, MaxSymbolLen) && nonNegative(v.Contracts) && v.Contracts > 0 {
					v.Account = acct
					fa.positions[posKey(v.Symbol, v.Side)] = v
				}
			}
			s.futures[acct] = fa
			ok = true
		}
	case strings.HasPrefix(f.subject, "plan:"):
		kind, id, found := strings.Cut(strings.TrimPrefix(f.subject, "plan:"), ":")
		n, err := strconv.ParseInt(id, 10, 64)
		var v PlanView
		if found && err == nil && validToken(kind, MaxTokenLen) && len(s.plans) < MaxPlans && json.Unmarshal([]byte(f.value), &v) == nil {
			v.Kind, v.ID, v.At, v.Trust = kind, n, at, trust
			s.plans[planKey{kind, n}] = &planState{view: v, src: f.source}
			ok = true
		}
	case strings.HasPrefix(f.subject, "fill:"):
		acct, sym, found := strings.Cut(strings.TrimPrefix(f.subject, "fill:"), ":")
		var v FillView
		if found && validSymbol(acct, 2*MaxTokenLen+1) && validSymbol(sym, MaxSymbolLen) && len(s.fills) < MaxFills && json.Unmarshal([]byte(f.value), &v) == nil {
			v.At, v.Trust = at, trust
			s.fills[fillKey{acct, sym}] = &fillState{view: v, desc: fillText(v), src: f.source}
			ok = true
		}
	case f.subject == "clock":
		var h dayHint
		if json.Unmarshal([]byte(f.value), &h) == nil {
			s.day = h
			ok = true
		}
	}
	if ok {
		s.c.restoredRows.Add(1)
	}
}

func validPortfolio(p *portfolioState) bool {
	if !nonNegative(p.TotalUSD) || !nonNegative(p.DayStartUSD) || len(p.ByClass) > MaxClasses ||
		len(p.Positions) > MaxPortfolioPositions || len(p.Peaks) > 31 {
		return false
	}
	for _, v := range p.ByClass {
		if !nonNegative(v) {
			return false
		}
	}
	for _, q := range p.Positions {
		if !nonNegative(q.Value) || !nonNegative(q.Qty) {
			return false
		}
	}
	for _, d := range p.Peaks {
		if !nonNegative(d.Max) {
			return false
		}
	}
	return true
}

// restoreDetector loads a detector state, or resets it when it is older than its
// bound (a number of its own buckets before the restart) or not sane. The series'
// price and timestamp stay as they were saved. The caller holds the write lock.
func (s *Store) restoreDetector(r detRow, now time.Time) {
	p, ok := strings.CutPrefix(r.series, "price:")
	if !ok {
		return
	}
	prov, sym, ok := strings.Cut(p, ":")
	if !ok {
		return
	}
	sr := s.series[seriesKey{prov, sym}]
	if sr == nil {
		return
	}
	age := now.Sub(time.UnixMilli(r.updatedMs))
	switch r.detector {
	case "ret_1m":
		rs := newMinuteSeries()
		if s.loadReturnSeries(&rs, r.state, age, minuteStaleBuckets*minuteBucket) {
			sr.m1 = rs
		}
	case "ret_1h":
		rs := newHourSeries()
		if s.loadReturnSeries(&rs, r.state, age, hourStaleBuckets*hourBucket) {
			sr.h1 = rs
		}
	case "cusum_1m":
		c := newCUSUM()
		if age > minuteStaleBuckets*minuteBucket {
			s.c.detectorsReset.Add(1)
		} else if json.Unmarshal([]byte(r.state), &c) == nil && finite(c.Pos) && finite(c.Neg) && c.Pos >= 0 && c.Neg >= 0 {
			c.K, c.H = cusumK, cusumH
			sr.cusum = c
		}
	}
}

// loadReturnSeries decodes a saved return series into rs, whose configuration
// (bucket, half-life, ...) is then reset to the current constants. It reports
// false, leaving rs a fresh series, when the state is stale or not sane.
func (s *Store) loadReturnSeries(rs *detect.ReturnSeries, state string, age, bound time.Duration) bool {
	fresh := *rs
	if age > bound {
		s.c.detectorsReset.Add(1)
		return false
	}
	if json.Unmarshal([]byte(state), rs) != nil || !finite(rs.E.Mean) || !finite(rs.E.Var) || rs.E.Var < 0 || rs.E.N < 0 ||
		!finite(rs.Last) || !finite(rs.Prev) || rs.Last < 0 || rs.Prev < 0 {
		*rs = fresh
		s.c.detectorsReset.Add(1)
		return false
	}
	rs.Bucket, rs.MaxGap = fresh.Bucket, fresh.MaxGap
	rs.E.HalfLife, rs.E.MinSamples, rs.E.MinSigma = fresh.E.HalfLife, fresh.E.MinSamples, fresh.E.MinSigma
	return true
}

// ---- checkpoint -----------------------------------------------------------

// batch is what one checkpoint writes.
type batch struct {
	facts    []factRow
	delFacts [][2]string
	dets     []detRow
	delDets  [][2]string
	marks    []markRow
	changes  []logEntry

	factVer map[rowKey]uint64
	detVer  map[detKey]uint64
	markVer map[string]uint64
	maxSeq  int64
	prune   int64 // delete log rows older than this (ms)
}

func (b *batch) empty() bool {
	return len(b.facts)+len(b.delFacts)+len(b.dets)+len(b.delDets)+len(b.marks)+len(b.changes) == 0
}

const captureChunk = 256

// capture copies out what has changed since the last checkpoint. The changed rows
// are marshalled in chunks, each under the read lock, so that Apply (which needs the
// write lock) waits for a few hundred small marshals at a time and never for the
// whole checkpoint.
func (s *Store) capture(now time.Time) *batch {
	b := &batch{
		factVer: map[rowKey]uint64{}, detVer: map[detKey]uint64{}, markVer: map[string]uint64{},
		prune: now.Add(-ChangeRetention).UnixMilli(),
	}
	type rv struct {
		k rowKey
		v uint64
	}
	type dv struct {
		k detKey
		v uint64
	}
	s.mu.RLock()
	rowKeys := make([]rv, 0, len(s.dirtyRows))
	for k, v := range s.dirtyRows {
		rowKeys = append(rowKeys, rv{k, v})
	}
	detKeys := make([]dv, 0, len(s.dirtyDets))
	for k, v := range s.dirtyDets {
		detKeys = append(detKeys, dv{k, v})
	}
	for name, v := range s.dirtyMarks {
		m := s.marks[name]
		b.marks = append(b.marks, markRow{name, m.seq, m.atMs})
		b.markVer[name] = v
	}
	b.changes = append(b.changes, s.buf...)
	b.maxSeq = s.flushedSeq
	if n := len(s.buf); n > 0 {
		b.maxSeq = s.buf[n-1].Seq
	}
	s.mu.RUnlock()

	sort.Slice(rowKeys, func(i, j int) bool { return lessRow(rowKeys[i].k, rowKeys[j].k) })
	sort.Slice(detKeys, func(i, j int) bool { return lessDet(detKeys[i].k, detKeys[j].k) })
	sort.Slice(b.marks, func(i, j int) bool { return b.marks[i].name < b.marks[j].name })

	for i := 0; i < len(rowKeys); i += captureChunk {
		end := min(i+captureChunk, len(rowKeys))
		s.mu.RLock()
		for _, e := range rowKeys[i:end] {
			ver := s.dirtyRows[e.k]
			if ver == 0 {
				continue
			}
			if row, ok := s.factRowLocked(e.k); ok {
				b.facts = append(b.facts, row)
			} else {
				sub, key := e.k.subjectKey()
				b.delFacts = append(b.delFacts, [2]string{sub, key})
			}
			b.factVer[e.k] = ver
		}
		s.mu.RUnlock()
	}
	for i := 0; i < len(detKeys); i += captureChunk {
		end := min(i+captureChunk, len(detKeys))
		s.mu.RLock()
		for _, e := range detKeys[i:end] {
			ver := s.dirtyDets[e.k]
			if ver == 0 {
				continue
			}
			if row, ok := s.detRowLocked(e.k); ok {
				b.dets = append(b.dets, row)
			} else {
				b.delDets = append(b.delDets, [2]string{e.k.k.subject(), detNames[e.k.which]})
			}
			b.detVer[e.k] = ver
		}
		s.mu.RUnlock()
	}
	return b
}

func lessRow(a, b rowKey) bool {
	if a.kind != b.kind {
		return a.kind < b.kind
	}
	if a.a != b.a {
		return a.a < b.a
	}
	if a.b != b.b {
		return a.b < b.b
	}
	return a.n < b.n
}

func lessDet(a, b detKey) bool {
	if a.k != b.k {
		if a.k.provider != b.k.provider {
			return a.k.provider < b.k.provider
		}
		return a.k.symbol < b.k.symbol
	}
	return a.which < b.which
}

type priceJSON struct {
	P float64 `json:"p"`
}

// factRowLocked builds the row for a fact identity from the live state; false
// means the state is gone and the row is to be deleted. The caller holds the read
// lock.
func (s *Store) factRowLocked(k rowKey) (factRow, bool) {
	subject, key := k.subjectKey()
	row := factRow{subject: subject, key: key}
	var v any
	switch k.kind {
	case rkPrice:
		sr := s.series[seriesKey{k.a, k.b}]
		if sr == nil {
			return row, false
		}
		v, row.atMs, row.source, row.trust = priceJSON{sr.price}, sr.at.UnixMilli(), sr.src, int(sr.trust)
	case rkPortfolio:
		if s.pf == nil {
			return row, false
		}
		v, row.atMs, row.source, row.trust = s.pf, s.pf.At.UnixMilli(), s.pf.Src, int(s.pf.Trust)
	case rkFutures:
		fa := s.futures[k.a]
		if fa == nil {
			return row, false
		}
		views := make([]FuturesView, 0, len(fa.positions))
		for _, p := range fa.positions {
			views = append(views, p)
		}
		sort.Slice(views, func(i, j int) bool {
			if views[i].Symbol != views[j].Symbol {
				return views[i].Symbol < views[j].Symbol
			}
			return views[i].Side < views[j].Side
		})
		v, row.atMs, row.source, row.trust = views, fa.at.UnixMilli(), fa.src, int(fa.trust)
	case rkPlan:
		ps := s.plans[planKey{k.a, k.n}]
		if ps == nil {
			return row, false
		}
		v, row.atMs, row.source, row.trust = ps.view, ps.view.At.UnixMilli(), ps.src, int(ps.view.Trust)
	case rkFill:
		fs := s.fills[fillKey{k.a, k.b}]
		if fs == nil {
			return row, false
		}
		v, row.atMs, row.source, row.trust = fs.view, fs.view.At.UnixMilli(), fs.src, int(fs.view.Trust)
	default:
		if s.day == (dayHint{}) {
			return row, false
		}
		v = s.day
	}
	b, err := json.Marshal(v)
	if err != nil {
		return row, false
	}
	row.value = string(b)
	return row, true
}

func (s *Store) detRowLocked(k detKey) (detRow, bool) {
	sr := s.series[k.k]
	if sr == nil {
		return detRow{}, false
	}
	var v any
	switch k.which {
	case detMinute:
		v = sr.m1
	case detHour:
		v = sr.h1
	default:
		v = sr.cusum
	}
	b, err := json.Marshal(v)
	if err != nil {
		return detRow{}, false
	}
	return detRow{series: k.k.subject(), detector: detNames[k.which], state: string(b), updatedMs: sr.at.UnixMilli()}, true
}

// Checkpoint writes what changed since the last checkpoint to the database in one
// transaction and then prunes the change log. It holds no lock while it writes. If
// it fails, nothing is lost: the rows stay marked as changed, the buffered log
// entries stay buffered, and the next checkpoint writes them. Checkpoints are
// serialised with each other. It is called every five minutes from Housekeep and
// once from Close, and is a no-op on a memory-only store.
func (s *Store) Checkpoint(ctx context.Context) error {
	if s.db == nil {
		return nil
	}
	s.ckMu.Lock()
	defer s.ckMu.Unlock()
	now := s.clock.Now()
	b := s.capture(now)
	deletedMax, err := s.write(ctx, b)
	if err != nil {
		s.c.checkpointFailures.Add(1)
		s.warn("checkpoint", "World model checkpoint failed; it will be retried", err)
		return fmt.Errorf("worldmodel: checkpoint: %w", err)
	}

	s.mu.Lock()
	for k, ver := range b.factVer {
		if s.dirtyRows[k] == ver {
			delete(s.dirtyRows, k)
		}
	}
	for k, ver := range b.detVer {
		if s.dirtyDets[k] == ver {
			delete(s.dirtyDets, k)
		}
	}
	for name, ver := range b.markVer {
		if s.dirtyMarks[name] == ver {
			delete(s.dirtyMarks, name)
		}
	}
	drop := 0
	for drop < len(s.buf) && s.buf[drop].Seq <= b.maxSeq {
		drop++
	}
	if drop > 0 {
		n := copy(s.buf, s.buf[drop:])
		clear(s.buf[n:])
		s.buf = s.buf[:n]
	}
	s.flushedSeq = max(s.flushedSeq, b.maxSeq)
	if deletedMax > 0 {
		s.lowSeq = max(s.lowSeq, deletedMax+1)
	}
	s.mu.Unlock()

	s.c.checkpoints.Add(1)
	s.lastCk.Store(now.UnixNano())
	return nil
}

// write runs the batch in one transaction and returns the highest sequence number
// the retention prune deleted.
func (s *Store) write(ctx context.Context, b *batch) (deletedMax int64, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	for _, f := range b.facts {
		if _, err = tx.ExecContext(ctx,
			`INSERT INTO wm_facts (subject, key, value_json, at_ms, source, trust) VALUES (?, ?, ?, ?, ?, ?)
			 ON CONFLICT(subject, key) DO UPDATE SET value_json = excluded.value_json, at_ms = excluded.at_ms,
			   source = excluded.source, trust = excluded.trust`,
			f.subject, f.key, f.value, f.atMs, f.source, f.trust); err != nil {
			return 0, err
		}
	}
	for _, f := range b.delFacts {
		if _, err = tx.ExecContext(ctx, `DELETE FROM wm_facts WHERE subject = ? AND key = ?`, f[0], f[1]); err != nil {
			return 0, err
		}
	}
	for _, r := range b.dets {
		if _, err = tx.ExecContext(ctx,
			`INSERT INTO wm_detectors (series, detector, state_json, updated_ms) VALUES (?, ?, ?, ?)
			 ON CONFLICT(series, detector) DO UPDATE SET state_json = excluded.state_json, updated_ms = excluded.updated_ms`,
			r.series, r.detector, r.state, r.updatedMs); err != nil {
			return 0, err
		}
	}
	for _, r := range b.delDets {
		if _, err = tx.ExecContext(ctx, `DELETE FROM wm_detectors WHERE series = ? AND detector = ?`, r[0], r[1]); err != nil {
			return 0, err
		}
	}
	for _, m := range b.marks {
		if _, err = tx.ExecContext(ctx,
			`INSERT INTO wm_marks (name, seq, at_ms) VALUES (?, ?, ?)
			 ON CONFLICT(name) DO UPDATE SET seq = excluded.seq, at_ms = excluded.at_ms`,
			m.name, m.seq, m.atMs); err != nil {
			return 0, err
		}
	}
	for _, e := range b.changes {
		if _, err = tx.ExecContext(ctx,
			`INSERT OR REPLACE INTO wm_changes (seq, at_ms, subject, key, old_json, new_json, trust) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			e.Seq, e.AtMs, e.Subject, e.Key, string(e.Old), string(e.New), int(e.Trust)); err != nil {
			return 0, err
		}
	}
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM wm_changes WHERE at_ms < ?`, b.prune).Scan(&deletedMax); err != nil {
		return 0, err
	}
	if deletedMax > 0 {
		if _, err = tx.ExecContext(ctx, `DELETE FROM wm_changes WHERE at_ms < ?`, b.prune); err != nil {
			return 0, err
		}
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return deletedMax, nil
}
