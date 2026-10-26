package inspect_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/journal"
	"github.com/cryptoquantumwave/khunquant/pkg/policy"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab/inspect"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labjournal"
)

type rig struct {
	t *testing.T
	l *lab.Lab
	j *labjournal.Journal
}

func newRig(t *testing.T, deps func() inspect.Deps) *rig {
	t.Helper()
	j, err := labjournal.Open(labjournal.Options{Path: filepath.Join(t.TempDir(), "lab", "journal.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = j.Close() })
	l := &lab.Lab{}
	if err := l.AddGroup(inspect.Group(j, deps)); err != nil {
		t.Fatal(err)
	}
	return &rig{t: t, l: l, j: j}
}

func (r *rig) add(kind, subject string) {
	r.j.Append(labjournal.Entry{Kind: kind, Subject: subject, Payload: json.RawMessage(`{"event":{"payload":{"n":1}}}`)})
}

// call runs an op through the registry, as both front-ends do, and decodes the result.
func (r *rig) call(op, args string) (map[string]any, error) {
	r.t.Helper()
	res, err := r.l.Call(context.Background(), "inspect", op, json.RawMessage(args))
	if err != nil {
		return nil, err
	}
	b, _ := json.Marshal(res)
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		r.t.Fatalf("%s result %s: %v", op, b, err)
	}
	return out, nil
}

func (r *rig) mustCall(op, args string) map[string]any {
	r.t.Helper()
	out, err := r.call(op, args)
	if err != nil {
		r.t.Fatalf("%s %s: %v", op, args, err)
	}
	return out
}

func seqs(out map[string]any) (s []int) {
	for _, e := range out["entries"].([]any) {
		s = append(s, int(e.(map[string]any)["seq"].(float64)))
	}
	return s
}

func code(err error) string {
	var le *lab.Error
	if errors.As(err, &le) {
		return le.Code
	}
	return fmt.Sprint(err)
}

func TestInspectJournalLimitAndDrops(t *testing.T) {
	r := newRig(t, nil)
	for i := 0; i < 20; i++ {
		r.add("event.agent.error", "s")
	}
	r.add("chat.out", "c1")
	out := r.mustCall("journal", `{"op":"journal","limit":5}`)
	if got := seqs(out); fmt.Sprint(got) != "[17 18 19 20 21]" {
		t.Errorf("limit 5 = %v, want the newest five in seq order", got)
	}
	if _, ok := out["drops"]; !ok || out["next_seq"].(float64) != 21 {
		t.Errorf("result lacks drops or next_seq: %v", out)
	}
	if got := seqs(r.mustCall("journal", `{"op":"journal","after_seq":3,"limit":2}`)); fmt.Sprint(got) != "[4 5]" {
		t.Errorf("a cursor reads the oldest after it: %v", got)
	}
	if got := seqs(r.mustCall("journal", `{"op":"journal","kinds":["chat.*"]}`)); fmt.Sprint(got) != "[21]" {
		t.Errorf("kind prefix = %v", got)
	}
	if got := seqs(r.mustCall("journal", `{"op":"journal","subject":"c1"}`)); fmt.Sprint(got) != "[21]" {
		t.Errorf("subject = %v", got)
	}
	if out := r.mustCall("journal", `{"op":"journal","after_seq":99}`); len(seqs(out)) != 0 || out["next_seq"].(float64) != 99 {
		t.Errorf("past the end: %v", out)
	}
	for _, bad := range []string{`{"limit":-1}`, `{"since":"yesterday"}`, `{"kinds":"chat.out"}`} {
		if _, err := r.call("journal", bad); code(err) != lab.CodeBadArgs {
			t.Errorf("journal %s: %v, want bad_args", bad, err)
		}
	}
}

func TestInspectKindViews(t *testing.T) {
	r := newRig(t, nil)
	r.add("event.attention.decision", "d")
	r.add("event.trigger.candidate", "t")
	r.add("event.worldmodel.diff", "w1")
	r.add("event.worldmodel.diff", "w2")
	r.add("chat.out", "c1")
	r.add("chat.edit", "c1")
	r.add("chat.out", "c2")
	r.add("chat.in", "c1")
	for op, want := range map[string]string{
		"decisions": "[1]", "triggers": "[2]", "world": "[3 4]",
		"outbox": "[5 6 7]",
	} {
		if got := seqs(r.mustCall(op, `{}`)); fmt.Sprint(got) != want {
			t.Errorf("%s = %v, want %s", op, got, want)
		}
	}
	if got := seqs(r.mustCall("world", `{"limit":1}`)); fmt.Sprint(got) != "[4]" {
		t.Errorf("world limit 1 = %v, want the newest diff", got)
	}
	if got := seqs(r.mustCall("outbox", `{"to":"c1"}`)); fmt.Sprint(got) != "[5 6]" {
		t.Errorf("outbox to c1 = %v (chat.in must not appear)", got)
	}
	// The kinds of a fixed view cannot be overridden.
	if got := seqs(r.mustCall("decisions", `{"kinds":["chat.in"]}`)); fmt.Sprint(got) != "[1]" {
		t.Errorf("decisions with kinds = %v", got)
	}
}

func TestInspectServiceOpsUnavailableWithoutDeps(t *testing.T) {
	for name, deps := range map[string]func() inspect.Deps{
		"nil deps func": nil,
		"empty deps":    func() inspect.Deps { return inspect.Deps{} },
	} {
		r := newRig(t, deps)
		for _, op := range []string{"workflows", "proposals", "policy", "settle"} {
			if _, err := r.call(op, `{}`); code(err) != lab.CodeUnavailable {
				t.Errorf("%s: %s = %v, want unavailable", name, op, err)
			}
		}
		r.add("event.x", "s") // the journal ops do not depend on them
		if got := seqs(r.mustCall("journal", `{}`)); len(got) != 1 {
			t.Errorf("%s: journal = %v", name, got)
		}
	}
}

func TestInspectServiceOps(t *testing.T) {
	var gotFilter journal.ListFilter
	var gotN int
	deps := inspect.Deps{
		Workflows: func(_ context.Context, f journal.ListFilter) ([]journal.Workflow, error) {
			gotFilter = f
			return []journal.Workflow{{ID: "1", Kind: "dca.execute"}, {ID: "2", Kind: "approval.order"}}, nil
		},
		Pending: func(context.Context) ([]approval.PendingView, time.Time, error) {
			return []approval.PendingView{{Code: "ABC123", Summary: "buy"}}, time.Unix(100, 0), nil
		},
		Policy: func(_ context.Context, n int) ([]policy.Decision, error) {
			gotN = n
			return []policy.Decision{{Tool: "create_order"}}, nil
		},
		Settle: func(context.Context) error { return nil },
	}
	calls := 0
	r := newRig(t, func() inspect.Deps { calls++; return deps }) // read on every call
	r.j.Append(labjournal.Entry{Kind: "proposal", Subject: "ABC123"})

	out := r.mustCall("workflows", `{"kind":"approval.order","status":["running","waiting"],"limit":7}`)
	ws := out["workflows"].([]any)
	if len(ws) != 1 || ws[0].(map[string]any)["ID"] != "2" {
		t.Errorf("workflows = %v, want only the approval.order one", ws)
	}
	if gotFilter.Limit != 7 || len(gotFilter.Statuses) != 2 || gotFilter.Statuses[0] != journal.StatusRunning {
		t.Errorf("filter passed to the engine = %+v", gotFilter)
	}
	out = r.mustCall("proposals", `{}`)
	if p := out["pending"].([]any); len(p) != 1 || p[0].(map[string]any)["Code"] != "ABC123" || len(out["entries"].([]any)) != 1 {
		t.Errorf("proposals = %v", out)
	}
	r.mustCall("policy", `{}`)
	if gotN != 20 {
		t.Errorf("policy default n = %d, want 20", gotN)
	}
	r.mustCall("policy", `{"limit":3}`)
	if gotN != 3 {
		t.Errorf("policy n = %d, want 3", gotN)
	}
	if out := r.mustCall("settle", `{}`); out["settled"] != true {
		t.Errorf("settle = %v", out)
	}
	if calls < 5 {
		t.Errorf("deps read %d times for 5 calls: they must be read per call", calls)
	}

	deps.Policy = func(context.Context, int) ([]policy.Decision, error) { return nil, errors.New("db closed") }
	if _, err := r.call("policy", `{}`); code(err) != lab.CodeFailed {
		t.Errorf("a failing service = %v, want failed", err)
	}
}

func TestInspectWaitFor(t *testing.T) {
	r := newRig(t, nil)
	r.add("event.trigger.candidate", "price:binance:BTC/USDT")

	out := r.mustCall("wait_for", `{"match":{"kind":"event.trigger.candidate","subject":"price:binance:BTC/USDT","fields":{"event.payload.n":1}},"timeout_wall":"120s"}`)
	if out["matched"] != true || out["count"].(float64) != 1 || out["next_seq"].(float64) != 1 {
		t.Errorf("hit = %v", out)
	}

	// A later entry is found by the next call, from the cursor.
	time.AfterFunc(100*time.Millisecond, func() { r.add("chat.out", "c1") })
	start := time.Now()
	out = r.mustCall("wait_for", `{"match":{"kind":"chat.out"},"after_seq":1,"timeout_wall":"20s"}`)
	if out["matched"] != true || time.Since(start) > 5*time.Second {
		t.Errorf("late entry = %v after %v", out, time.Since(start))
	}

	start = time.Now()
	out = r.mustCall("wait_for", `{"match":{"kind":"chat.in"},"after_seq":2,"timeout_wall":"200ms"}`)
	if out["matched"] != false || out["count"].(float64) != 0 || len(out["entries"].([]any)) != 0 || time.Since(start) > 3*time.Second {
		t.Errorf("timeout = %v after %v", out, time.Since(start))
	}

	for _, bad := range []string{`{"match":{"kind":"x","count":"lots"}}`, `{"match":{"kind":"x"},"timeout_wall":"soon"}`, `{"match":"x"}`} {
		if _, err := r.call("wait_for", bad); code(err) != lab.CodeBadArgs {
			t.Errorf("wait_for %s: %v, want bad_args", bad, err)
		}
	}
	if lab.CallTimeout != labjournal.MaxWait {
		t.Errorf("CallTimeout %v != MaxWait %v", lab.CallTimeout, labjournal.MaxWait)
	}
}

// The MCP tool merges the ops' argument schemas into one: a name two ops share must
// mean the same thing in both.
func TestInspectArgSchemasAgree(t *testing.T) {
	seen := map[string]string{}
	for _, o := range inspect.Group(nil, nil).Ops {
		var props map[string]json.RawMessage
		if len(o.Args) > 0 {
			if err := json.Unmarshal(o.Args, &props); err != nil {
				t.Fatalf("op %s: args are not a JSON object: %v", o.Name, err)
			}
		}
		for k, v := range props {
			if prev, ok := seen[k]; ok && prev != string(v) {
				t.Errorf("op %s: property %q is %s here and %s elsewhere", o.Name, k, v, prev)
			}
			seen[k] = string(v)
		}
	}
	if len(seen) == 0 {
		t.Fatal("no properties found")
	}
}
