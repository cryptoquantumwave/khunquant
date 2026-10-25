package journal

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/perception/perceptiontest"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex"
)

var t0 = time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)

const (
	testLease = 30 * time.Second
	testGrace = 10 * time.Second
)

// dbPath is a database file shared by the engines of one test. Each engine opens
// its own handle on it, as two processes would.
func dbPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "proactive.db")
}

func openAt(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sqlitex.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// node is one engine on its own database handle and its own fake clock: a process.
type node struct {
	*Engine
	clock *perceptiontest.Clock
	db    *sql.DB
	kill  *killSwitch
}

func newNode(t *testing.T, path string, start time.Time, defs []Definition, opts ...Option) *node {
	t.Helper()
	db := openAt(t, path)
	clock := perceptiontest.NewClock(start)
	all := append([]Option{WithWaitPoll(2 * time.Millisecond), WithShutdownWait(300 * time.Millisecond)}, opts...)
	e, err := New(db, defs, clock, all...)
	if err != nil {
		t.Fatal(err)
	}
	return &node{Engine: e, clock: clock, db: db}
}

var errKilled = errors.New("test: process is dead")

// killSwitch makes an engine behave like a process that died: from the moment it
// is thrown, every database operation of that engine fails, and the goroutine that
// hit the crash point ends.
type killSwitch struct{ dead atomic.Bool }

func (k *killSwitch) fault(string) error {
	if k.dead.Load() {
		return errKilled
	}
	return nil
}

// armKill makes n die at the first crash point for which at returns true.
func (n *node) armKill(at func(point, step string, attempt int) bool) *killSwitch {
	k := &killSwitch{}
	n.kill = k
	n.st.fault = k.fault
	n.hook = func(point, _, step string, attempt int) {
		if k.dead.Load() {
			runtime.Goexit()
		}
		if at(point, step, attempt) {
			k.dead.Store(true)
			runtime.Goexit()
		}
	}
	return k
}

// exec runs the workflow on n in a goroutine of its own (a crash point ends the
// goroutine with Goexit) and waits for it.
func (n *node) exec(id string) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		n.execute(context.Background(), id)
	}()
	<-done
}

// claimable reports whether n could claim id now.
func (n *node) claimable(t *testing.T, id string) bool {
	t.Helper()
	ids, err := n.st.due(context.Background(), n.kinds, n.clock.Now(), n.opt.grace, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

func status(t *testing.T, n *node, id string) Workflow {
	t.Helper()
	// Read with a store of its own: a killed engine's store fails every call.
	w, err := (&store{db: n.db}).getWorkflow(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// ledger counts how often each side effect really happened and which attempts
// each step function saw.
type ledger struct {
	mu       sync.Mutex
	effects  map[string]int
	attempts map[string][]int
}

func newLedger() *ledger {
	return &ledger{effects: map[string]int{}, attempts: map[string][]int{}}
}

func (l *ledger) saw(step string, attempt int) {
	l.mu.Lock()
	l.attempts[step] = append(l.attempts[step], attempt)
	l.mu.Unlock()
}

func (l *ledger) did(effect string) {
	l.mu.Lock()
	l.effects[effect]++
	l.mu.Unlock()
}

func (l *ledger) effect(effect string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.effects[effect]
}

func (l *ledger) seen(step string) []int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]int(nil), l.attempts[step]...)
}

// exchange is a fake venue that dedups on a client id, the way a real one does.
type exchange struct {
	mu     sync.Mutex
	orders map[string]int
	calls  int
}

func newExchange() *exchange { return &exchange{orders: map[string]int{}} }

func (x *exchange) place(key string) int {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.calls++
	x.orders[key]++
	return x.orders[key]
}

func (x *exchange) has(key string) bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.orders[key] > 0
}

func (x *exchange) snapshot() (calls int, orders map[string]int) {
	x.mu.Lock()
	defer x.mu.Unlock()
	cp := map[string]int{}
	for k, v := range x.orders {
		cp[k] = v
	}
	return x.calls, cp
}

type orderOut struct {
	Key string `json:"key"`
	N   int    `json:"n"`
}

// orderStep places an order the way MR-304 will: on attempt > 1 it looks for the
// order before placing it again.
func orderStep(c *Ctx, l *ledger, x *exchange, name string) error {
	key := c.IdempotencyKey(name)
	var out orderOut
	return c.Step(name, &out, func(_ context.Context, attempt int) (any, error) {
		l.saw(name, attempt)
		if got := c.IdempotencyKey(name); got != key {
			return nil, fmt.Errorf("key changed inside the step: %q != %q", got, key)
		}
		if attempt > 1 && x.has(key) {
			return orderOut{Key: key}, nil
		}
		l.did(name)
		return orderOut{Key: key, N: x.place(key)}, nil
	})
}

// fiveSteps is the acceptance workflow: five order steps.
func fiveSteps(l *ledger, x *exchange) Definition {
	return Definition{Kind: "five", Run: func(c *Ctx, _ json.RawMessage) error {
		for i := 1; i <= 5; i++ {
			if err := orderStep(c, l, x, fmt.Sprintf("s%d", i)); err != nil {
				return err
			}
		}
		return nil
	}}
}

func mustStart(t *testing.T, e *Engine, kind string, input any) string {
	t.Helper()
	id, err := e.Start(context.Background(), kind, input)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// rawSteps reads workflow_steps directly, for assertions that must not go through
// the engine under test.
func rawSteps(t *testing.T, db *sql.DB, id string) []StepRecord {
	t.Helper()
	st := &store{db: db}
	rows, err := st.loadSteps(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]StepRecord, len(rows))
	for i, r := range rows {
		out[i] = r.record(id)
	}
	return out
}

func exec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatal(err)
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func waitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}
