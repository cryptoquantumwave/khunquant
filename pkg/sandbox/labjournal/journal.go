// Package labjournal is the append-only, sim-time-stamped journal of one sandbox
// instance (a SQLite file in its home). It is a leaf: it imports no provider, channel,
// approval or tool package, so the components that write to it can import it.
package labjournal

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex"
)

const batchMax = 256 // entries per transaction

// Entry is one journal row. Kind: event.<kind>, chat.*, workflow.step, proposal, ...
type Entry struct {
	Seq      int64           `json:"seq"`
	SimTime  time.Time       `json:"sim_time"`
	WallTime time.Time       `json:"wall_time"`
	Kind     string          `json:"kind"`
	Source   string          `json:"source,omitempty"`
	Subject  string          `json:"subject,omitempty"`
	Payload  json.RawMessage `json:"payload,omitempty"`
}

// Options configures Open.
type Options struct {
	Path  string           // <KHUNQUANT_HOME>/lab/journal.db
	Now   func() time.Time // sim time; nil = time.Now
	Queue int              // entries Append can queue before it drops (default 8192)
}

type item struct {
	e       Entry
	barrier chan struct{}
}

// Journal is the SQLite-backed journal. One goroutine writes, in batches.
type Journal struct {
	db  *sql.DB
	now func() time.Time
	q   chan item

	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	drops     atomic.Uint64

	mu      sync.Mutex
	sources []func() uint64
}

var migrations = []sqlitex.Migration{{Component: "labjournal", Version: 1, SQL: `
CREATE TABLE lab_journal (seq INTEGER PRIMARY KEY AUTOINCREMENT, sim_ns INTEGER NOT NULL,
	wall_ns INTEGER NOT NULL, kind TEXT NOT NULL, source TEXT NOT NULL, subject TEXT NOT NULL, payload BLOB NOT NULL);
CREATE INDEX lab_journal_kind ON lab_journal(kind, seq);`}}

// Open opens (creating it, 0600, in a 0700 directory) the journal at o.Path and
// starts its writer. The file survives a restart; Seq continues from the last row.
func Open(o Options) (*Journal, error) {
	if err := os.MkdirAll(filepath.Dir(o.Path), 0o700); err != nil {
		return nil, fmt.Errorf("labjournal: create dir: %w", err)
	}
	db, err := sqlitex.Open(o.Path)
	if err != nil {
		return nil, err
	}
	if err := sqlitex.Migrate(context.Background(), db, migrations...); err != nil {
		_ = db.Close()
		return nil, err
	}
	j := &Journal{
		db: db, now: o.Now, q: make(chan item, cmp.Or(o.Queue, 8192)),
		stop: make(chan struct{}), done: make(chan struct{}),
	}
	if j.now == nil {
		j.now = time.Now
	}
	go j.run()
	return j, nil
}

// Append queues e and returns at once; a full queue drops it (counted in Drops). Zero
// times are stamped, and a payload that is not JSON is stored as a JSON string.
func (j *Journal) Append(e Entry) {
	if e.WallTime.IsZero() {
		e.WallTime = time.Now()
	}
	if e.SimTime.IsZero() {
		e.SimTime = j.now()
	}
	if len(e.Payload) == 0 {
		e.Payload = json.RawMessage(`{}`)
	} else if !json.Valid(e.Payload) {
		e.Payload, _ = json.Marshal(string(e.Payload))
	}
	select {
	case j.q <- item{e: e}:
	default:
		j.drops.Add(1)
	}
}

// Drops counts the entries lost to a full queue or a failed write, plus whatever the
// registered sources report (the observer adds its subscription's dropped events).
func (j *Journal) Drops() uint64 {
	n := j.drops.Load()
	j.mu.Lock()
	srcs := j.sources
	j.mu.Unlock()
	for _, f := range srcs {
		n += f()
	}
	return n
}

// AddDropSource adds a counter to Drops.
func (j *Journal) AddDropSource(f func() uint64) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.sources = append(j.sources, f)
}

// Flush returns once every entry appended before the call has been written.
func (j *Journal) Flush(ctx context.Context) error {
	b := item{barrier: make(chan struct{})}
	select {
	case j.q <- b:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-b.barrier:
		return nil
	case <-j.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close writes what is queued, stops the writer and closes the file. It is idempotent.
func (j *Journal) Close() error {
	var err error
	j.closeOnce.Do(func() {
		close(j.stop)
		<-j.done
		err = j.db.Close()
	})
	return err
}

func (j *Journal) run() {
	defer close(j.done)
	for {
		var batch []item
		select {
		case it := <-j.q:
			batch = []item{it}
		case <-j.stop: // Close: write what is left, then return
			if batch = j.take(nil); len(batch) == 0 {
				return
			}
		}
		j.write(j.take(batch))
	}
}

// take appends up to batchMax queued items to batch without waiting.
func (j *Journal) take(batch []item) []item {
	for len(batch) < batchMax {
		select {
		case it := <-j.q:
			batch = append(batch, it)
		default:
			return batch
		}
	}
	return batch
}

// write commits the entries of batch in one transaction and releases its barriers.
// A failed write loses the entries (counted in Drops).
func (j *Journal) write(batch []item) {
	var es []Entry
	for _, it := range batch {
		if it.barrier == nil {
			es = append(es, it.e)
		}
	}
	if err := j.insert(es); err != nil {
		j.drops.Add(uint64(len(es)))
		logger.WarnCF("labjournal", "Could not write journal entries", map[string]any{"err": err.Error()})
	}
	for _, it := range batch {
		if it.barrier != nil {
			close(it.barrier)
		}
	}
}

func (j *Journal) insert(es []Entry) error {
	if len(es) == 0 {
		return nil
	}
	tx, err := j.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	st, err := tx.Prepare(`INSERT INTO lab_journal (sim_ns, wall_ns, kind, source, subject, payload) VALUES (?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer st.Close()
	for _, e := range es {
		if _, err := st.Exec(e.SimTime.UnixNano(), e.WallTime.UnixNano(), e.Kind, e.Source, e.Subject, []byte(e.Payload)); err != nil {
			return err
		}
	}
	return tx.Commit()
}
