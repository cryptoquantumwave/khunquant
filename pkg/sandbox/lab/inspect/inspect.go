// Package inspect is the lab group "inspect": read-only views of a locked instance's
// journal, workflows, proposals and policy verdicts, and the bounded wait_for. It is a
// package of its own so labjournal stays a leaf that simchat and the sim provider can
// import without pulling in approval (which reaches the channels).
package inspect

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/journal"
	"github.com/cryptoquantumwave/khunquant/pkg/policy"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labjournal"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/quiesce"
)

// Deps are the generation-scoped services behind four ops. The gateway fills what the
// current generation runs; a nil field makes its op answer lab.CodeUnavailable.
type Deps struct {
	Workflows func(ctx context.Context, f journal.ListFilter) ([]journal.Workflow, error) // (*journal.Engine).List
	Pending   func(ctx context.Context) ([]approval.PendingView, time.Time, error)       // (*approval.Service).ListPending
	Policy    func(ctx context.Context, n int) ([]policy.Decision, error)                // (*policy.Store).Recent
	Settle    func(ctx context.Context) error                                            // a settle with the default cap, no report
	// SettleWait is Settle with a wall cap and a report (lab.Lab.SettleWait); the op uses
	// it when set. Snapshot lists every quiescer and its state, without waiting (the idle op).
	SettleWait func(ctx context.Context, wallCap time.Duration) (quiesce.Report, error)
	Snapshot   func() []quiesce.Status
}

const (
	defaultWait   = 10 * time.Second
	defaultLimit  = 100
	policyDefault = 20
)

// Argument schemas. A name shared by several ops has one type everywhere: the MCP
// tool merges the ops' properties into one schema.
const (
	argKinds    = `"kinds":{"type":"array","items":{"type":"string"},"description":"exact kinds or prefixes such as chat.*"}`
	argSince    = `"since":{"type":"string","description":"RFC3339 sim time, inclusive"}`
	argUntil    = `"until":{"type":"string","description":"RFC3339 sim time, inclusive"}`
	argSubject  = `"subject":{"type":"string"}`
	argAfterSeq = `"after_seq":{"type":"integer","description":"cursor: only entries with a greater seq, oldest first; without it the newest entries are returned"}`
	argLimit    = `"limit":{"type":"integer","description":"at most 1000"}`
)

func schema(props ...string) json.RawMessage {
	return json.RawMessage("{" + strings.Join(props, ",") + "}")
}

// Group builds the inspect group over j. deps is read on every call, because the
// services behind it are replaced by each config reload.
func Group(j *labjournal.Journal, deps func() Deps) lab.Group {
	d := func() Deps {
		if deps == nil {
			return Deps{}
		}
		return deps()
	}
	ops := []lab.Op{
		{Name: "journal", Description: "journal entries and the drop count",
			Args: schema(argKinds, argSince, argUntil, argSubject, argAfterSeq, argLimit),
			Run:  entriesOp(j, nil, "")},
		{Name: "decisions", Description: "attention decisions",
			Args: schema(argSince, argAfterSeq, argLimit), Run: entriesOp(j, kindsOf(events.KindAttentionDecision), "")},
		{Name: "triggers", Description: "trigger candidates",
			Args: schema(argSince, argAfterSeq, argLimit), Run: entriesOp(j, kindsOf(events.KindTriggerCandidate), "")},
		{Name: "world", Description: "world model diffs (the diff history, not a full view), newest first without after_seq",
			Args: schema(argAfterSeq, argLimit), Run: entriesOp(j, kindsOf(events.KindWorldModelDiff), "")},
		{Name: "outbox", Description: "messages the instance sent to chats (chat.out, chat.edit)",
			Args: schema(`"to":{"type":"string","description":"chat id"}`, argSince, argAfterSeq, argLimit),
			Run:  entriesOp(j, []string{"chat.out", "chat.edit"}, "to")},
		{Name: "workflows", Description: "workflows of the journal engine, newest first",
			Args: schema(`"kind":{"type":"string"}`, `"status":{"type":"array","items":{"type":"string"}}`, argSince, argLimit),
			Run:  workflowsOp(d)},
		{Name: "proposals", Description: "pending order proposals and the proposal journal entries",
			Args: schema(argLimit), Run: proposalsOp(j, d)},
		{Name: "policy", Description: "the newest policy decisions, newest first",
			Args: schema(argLimit), Run: policyOp(d)},
		{Name: "settle", Description: "wait until every component is idle: {settled, wall_ms, busy}",
			Args: schema(`"wall_cap":{"type":"string","description":"Go duration, default 10s, clamped to 25s"}`), Run: settleOp(d)},
		{Name: "idle", Description: "every quiescer and whether it is idle now, without waiting", Run: idleOp(d)},
		{Name: "wait_for", Description: "block until journal entries match; at most 25 s per call, loop on next_seq",
			Args: schema(`"match":{"type":"object","description":"{kind, subject, fields (dotted paths into the payload), count (\"1\", \">= 2\", \"0\" = absence)}"}`,
				argAfterSeq, `"timeout_wall":{"type":"string","description":"Go duration, default 10s, clamped to 25s"}`),
			Required: []string{"match"}, Run: waitForOp(j)},
	}
	return lab.Group{Name: "inspect", Description: "Read the lab journal, workflows, proposals and policy decisions; wait for journal entries.", Ops: ops}
}

func kindsOf(k events.Kind) []string { return []string{"event." + string(k)} }

func decode(args json.RawMessage, v any) error {
	if err := json.Unmarshal(args, v); err != nil {
		return lab.Errorf(lab.CodeBadArgs, "%v", err)
	}
	return nil
}

func unavailable(what string) error {
	return lab.Errorf(lab.CodeUnavailable, "%s is not available in this instance", what)
}

// readArgs are the arguments of the journal-reading ops.
type readArgs struct {
	Kinds    []string  `json:"kinds"`
	Since    time.Time `json:"since"`
	Until    time.Time `json:"until"`
	Subject  string    `json:"subject"`
	To       string    `json:"to"`
	AfterSeq int64     `json:"after_seq"`
	Limit    int       `json:"limit"`
}

type entriesOut struct {
	Entries []labjournal.Entry `json:"entries"`
	NextSeq int64              `json:"next_seq"`
	Drops   uint64             `json:"drops"`
}

// read runs q. With a cursor it returns the oldest matches after it; without one the
// newest matches, in seq order.
func read(ctx context.Context, j *labjournal.Journal, q labjournal.Query) (entriesOut, error) {
	q.Desc = q.AfterSeq == 0
	es, err := j.Query(ctx, q)
	if err != nil {
		return entriesOut{}, err
	}
	if q.Desc {
		slices.Reverse(es)
	}
	out := entriesOut{Entries: es, NextSeq: q.AfterSeq, Drops: j.Drops()}
	if out.Entries == nil {
		out.Entries = []labjournal.Entry{}
	}
	if n := len(es); n > 0 {
		out.NextSeq = max(es[0].Seq, es[n-1].Seq)
	}
	return out, nil
}

// entriesOp is a journal read. A fixed kinds list replaces the caller's; subjectArg
// names the argument that filters the subject in place of "subject".
func entriesOp(j *labjournal.Journal, kinds []string, subjectArg string) lab.OpFunc {
	return func(ctx context.Context, args json.RawMessage) (any, error) {
		var a readArgs
		if err := decode(args, &a); err != nil {
			return nil, err
		}
		if a.Limit < 0 {
			return nil, lab.Errorf(lab.CodeBadArgs, "limit must not be negative")
		}
		if kinds != nil {
			a.Kinds = kinds
		}
		if subjectArg == "to" {
			a.Subject = a.To
		}
		return read(ctx, j, labjournal.Query{Kinds: a.Kinds, Since: a.Since, Until: a.Until,
			Subject: a.Subject, AfterSeq: a.AfterSeq, Limit: cmp.Or(a.Limit, defaultLimit)})
	}
}

func workflowsOp(deps func() Deps) lab.OpFunc {
	return func(ctx context.Context, args json.RawMessage) (any, error) {
		var a struct {
			Kind   string    `json:"kind"`
			Status []string  `json:"status"`
			Since  time.Time `json:"since"`
			Limit  int       `json:"limit"`
		}
		if err := decode(args, &a); err != nil {
			return nil, err
		}
		list := deps().Workflows
		if list == nil {
			return nil, unavailable("the workflow engine")
		}
		f := journal.ListFilter{Since: a.Since, Limit: a.Limit}
		for _, s := range a.Status {
			f.Statuses = append(f.Statuses, journal.Status(s))
		}
		ws, err := list(ctx, f)
		if err != nil {
			return nil, err
		}
		out := []journal.Workflow{} // ListFilter has no kind: it is applied after the limit
		for _, w := range ws {
			if a.Kind == "" || w.Kind == a.Kind {
				out = append(out, w)
			}
		}
		return map[string]any{"workflows": out}, nil
	}
}

func proposalsOp(j *labjournal.Journal, deps func() Deps) lab.OpFunc {
	return func(ctx context.Context, args json.RawMessage) (any, error) {
		var a struct {
			Limit int `json:"limit"`
		}
		if err := decode(args, &a); err != nil {
			return nil, err
		}
		pending := deps().Pending
		if pending == nil {
			return nil, unavailable("order approvals")
		}
		views, now, err := pending(ctx)
		if err != nil {
			return nil, err
		}
		journaled, err := read(ctx, j, labjournal.Query{Kinds: []string{"proposal"}, Limit: cmp.Or(a.Limit, defaultLimit)})
		if err != nil {
			return nil, err
		}
		if views == nil {
			views = []approval.PendingView{}
		}
		return map[string]any{"pending": views, "now": now, "entries": journaled.Entries, "drops": journaled.Drops}, nil
	}
}

func policyOp(deps func() Deps) lab.OpFunc {
	return func(ctx context.Context, args json.RawMessage) (any, error) {
		var a struct {
			Limit int `json:"limit"`
		}
		if err := decode(args, &a); err != nil {
			return nil, err
		}
		recent := deps().Policy
		if recent == nil {
			return nil, unavailable("the policy record")
		}
		ds, err := recent(ctx, min(cmp.Or(a.Limit, policyDefault), labjournal.MaxLimit))
		if err != nil {
			return nil, err
		}
		if ds == nil {
			ds = []policy.Decision{}
		}
		return map[string]any{"decisions": ds}, nil
	}
}

const (
	defaultSettleCap = 10 * time.Second
	maxSettleCap     = 25 * time.Second
)

// settleOp waits for every component to be idle. A settle that does not pass is an
// answer, not an error: {settled:false, busy:[names]}.
func settleOp(deps func() Deps) lab.OpFunc {
	return func(ctx context.Context, args json.RawMessage) (any, error) {
		d := deps()
		var a struct {
			WallCap string `json:"wall_cap"`
		}
		if len(args) > 0 {
			if err := decode(args, &a); err != nil {
				return nil, err
			}
		}
		wallCap := defaultSettleCap
		if a.WallCap != "" {
			var err error
			if wallCap, err = time.ParseDuration(a.WallCap); err != nil || wallCap <= 0 {
				return nil, lab.Errorf(lab.CodeBadArgs, "wall_cap %q is not a positive Go duration", a.WallCap)
			}
		}
		wallCap = min(wallCap, maxSettleCap)
		if d.SettleWait == nil {
			if d.Settle == nil {
				return nil, lab.Errorf(lab.CodeUnavailable, "settle is not available in this instance")
			}
			if err := d.Settle(ctx); err != nil {
				return nil, err
			}
			return map[string]bool{"settled": true}, nil
		}
		rep, err := d.SettleWait(ctx, wallCap)
		var busy *quiesce.BusyError
		if err != nil && !errors.As(err, &busy) {
			return nil, err
		}
		names := []string{}
		if busy != nil {
			names = append(names, busy.Names...)
		}
		return map[string]any{"settled": err == nil, "wall_ms": rep.Wall.Milliseconds(), "busy": names}, nil
	}
}

// idleOp lists every quiescer and its state now.
func idleOp(deps func() Deps) lab.OpFunc {
	return func(context.Context, json.RawMessage) (any, error) {
		snapshot := deps().Snapshot
		if snapshot == nil {
			return nil, unavailable("the quiescence registry")
		}
		st := snapshot()
		if st == nil {
			st = []quiesce.Status{}
		}
		return map[string]any{"quiescers": st}, nil
	}
}

func waitForOp(j *labjournal.Journal) lab.OpFunc {
	return func(ctx context.Context, args json.RawMessage) (any, error) {
		var a struct {
			Match       labjournal.Match `json:"match"`
			AfterSeq    int64            `json:"after_seq"`
			TimeoutWall string           `json:"timeout_wall"`
		}
		if err := decode(args, &a); err != nil {
			return nil, err
		}
		wait := defaultWait
		if a.TimeoutWall != "" {
			var err error
			if wait, err = time.ParseDuration(a.TimeoutWall); err != nil {
				return nil, lab.Errorf(lab.CodeBadArgs, "timeout_wall: %v", err)
			}
		}
		res, err := j.WaitFor(ctx, a.Match, a.AfterSeq, wait)
		if errors.Is(err, labjournal.ErrBadMatch) {
			return nil, lab.Errorf(lab.CodeBadArgs, "%v", err)
		}
		if err != nil {
			return nil, err
		}
		if res.Entries == nil {
			res.Entries = []labjournal.Entry{}
		}
		return res, nil
	}
}
