package journal

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/perception/perceptiontest"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex/sqlitextest"
)

type recorder struct {
	mu  sync.Mutex
	evs []Event
}

func (r *recorder) Publish(e Event) {
	r.mu.Lock()
	r.evs = append(r.evs, e)
	r.mu.Unlock()
}

func (r *recorder) all() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Event(nil), r.evs...)
}

// An event carries identifiers, names, numbers and durations only. This pins the
// field list: adding a field to Event must be a conscious change to this test.
func TestEvent_HasNoPayloadFields(t *testing.T) {
	allowed := map[string]reflect.Kind{
		"Scope": reflect.String, "WorkflowID": reflect.String, "WorkflowKind": reflect.String,
		"Protective": reflect.Bool, "Step": reflect.String, "Seq": reflect.Int, "Attempt": reflect.Int,
		"Status": reflect.String, "Duration": reflect.Int64,
	}
	typ := reflect.TypeOf(Event{})
	var got []string
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		got = append(got, f.Name)
		kind, ok := allowed[f.Name]
		if !ok {
			t.Errorf("Event has an unreviewed field %s (%s); an event must never carry input, output or error text", f.Name, f.Type)
			continue
		}
		if f.Type.Kind() != kind {
			t.Errorf("Event.%s is a %s, want %s", f.Name, f.Type.Kind(), kind)
		}
	}
	sort.Strings(got)
	if len(got) != len(allowed) {
		t.Errorf("Event fields = %v", got)
	}
}

func TestEvents_NeverCarryInputOutputOrErrorText(t *testing.T) {
	const (
		inSecret  = "SECRET-INPUT-8f3a"
		outSecret = "SECRET-OUTPUT-1c9d"
		errSecret = "SECRET-ERROR-77be"
	)
	def := Definition{Kind: "secrets", Protective: true, Run: func(c *Ctx, _ json.RawMessage) error {
		if err := c.Step("ok", nil, func(context.Context, int) (any, error) { return outSecret, nil }); err != nil {
			return err
		}
		return c.Step("bad", nil, func(context.Context, int) (any, error) { return nil, errors.New("exchange: " + errSecret) },
			Retries(1))
	}}
	rec := &recorder{}
	n := newNode(t, dbPath(t), t0, []Definition{def}, WithPublisher(rec))
	id := mustStart(t, n.Engine, "secrets", map[string]string{"api": inSecret})
	n.exec(id)
	w := status(t, n, id)
	if w.Status != StatusFailed || !strings.Contains(w.Error, errSecret) {
		t.Fatalf("setup: %+v", w)
	}

	evs := rec.all()
	var steps, workflows int
	var sawFailed, sawRetry, sawStarted, sawFinished bool
	for _, ev := range evs {
		raw, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{inSecret, outSecret, errSecret, "SECRET"} {
			if strings.Contains(string(raw), secret) {
				t.Errorf("event %s carries %q", raw, secret)
			}
		}
		if ev.WorkflowID != id || ev.WorkflowKind != "secrets" || !ev.Protective {
			t.Errorf("event %+v misses ids", ev)
		}
		switch ev.Scope {
		case ScopeStep:
			steps++
			sawFailed = sawFailed || ev.Status == StepFailed
			sawRetry = sawRetry || ev.Status == StepRetry
			if ev.Step == "" || ev.Attempt < 1 {
				t.Errorf("step event without a name or attempt: %+v", ev)
			}
		case ScopeWorkflow:
			workflows++
			sawStarted = sawStarted || ev.Status == WorkflowStarted
			sawFinished = sawFinished || ev.Status == string(StatusFailed)
		}
	}
	if steps < 5 || workflows != 2 || !sawFailed || !sawRetry || !sawStarted || !sawFinished {
		t.Errorf("events: %d step, %d workflow, failed=%v retry=%v started=%v finished=%v\n%+v",
			steps, workflows, sawFailed, sawRetry, sawStarted, sawFinished, evs)
	}
}

func TestPublisherPanicDoesNotFailAWorkflow(t *testing.T) {
	def := Definition{Kind: "pub", Run: func(c *Ctx, _ json.RawMessage) error {
		return c.Step("a", nil, func(context.Context, int) (any, error) { return 1, nil })
	}}
	n := newNode(t, dbPath(t), t0, []Definition{def}, WithPublisher(panicPublisher{}))
	id := mustStart(t, n.Engine, "pub", nil)
	n.exec(id)
	if w := status(t, n, id); w.Status != StatusSucceeded {
		t.Fatalf("workflow = %+v", w)
	}
}

type panicPublisher struct{}

func (panicPublisher) Publish(Event) { panic("subscriber bug") }

// Import boundary: the standard library, pkg/sqlitex and pkg/logger only.
func TestJournal_ImportBoundary(t *testing.T) {
	perceptiontest.AssertImports(t, perceptiontest.Boundary{
		Root:    "pkg/journal",
		Allowed: []string{"pkg/sqlitex", "pkg/logger"},
		Forbidden: []string{
			"pkg/events", "pkg/perception", "pkg/agent", "pkg/tools", "pkg/providers", "pkg/notify",
			"pkg/channels", "pkg/attention", "pkg/bus", "pkg/config", "pkg/intent", "pkg/approval",
		},
	})
}

func TestJournal_NoCrossPackageCallsInTx(t *testing.T) {
	sqlitextest.AssertNoCrossPackageCallsInTx(t, "pkg/journal")
}

// Journal records are committed at synchronous=FULL on a connection that goes back
// to the pool at NORMAL.
func TestStoreTx_DurableWritesRestoreSynchronous(t *testing.T) {
	n := newNode(t, dbPath(t), t0, nil)
	ctx := context.Background()
	var during int
	if err := n.st.tx(ctx, "probe", true, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&during)
	}); err != nil {
		t.Fatal(err)
	}
	if during != 2 { // FULL
		t.Errorf("synchronous inside a durable transaction = %d, want 2 (FULL)", during)
	}
	var conns []*sql.Conn
	for i := 0; i < 4; i++ { // every connection of the pool
		c, err := n.db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
		var mode int
		if err := c.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&mode); err != nil {
			t.Fatal(err)
		}
		if mode != 1 { // NORMAL
			t.Errorf("pooled connection %d has synchronous=%d after a durable write, want 1 (NORMAL)", i, mode)
		}
	}
	for _, c := range conns {
		_ = c.Close()
	}
}
