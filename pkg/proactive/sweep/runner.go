package sweep

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/worldmodel"
)

// Mark is the name of the world-model mark the sweep reads and moves: where the
// last completed sweep stopped looking.
const Mark = "heartbeat"

const (
	// DefaultReconcileBudget is how long the sources have, all together, to
	// re-read their state.
	DefaultReconcileBudget = 2 * time.Minute
	// DefaultSettleBudget is how long the sweep waits for the pipeline to work off
	// what the reconcile queued.
	DefaultSettleBudget = 15 * time.Second
	// DefaultMaxFailures is how many sweeps may fail over the same range of changes
	// before the sweep moves past it.
	DefaultMaxFailures = 3
)

// Reconciler asks the sources to re-read their state, within ctx's deadline.
// *perception.Manager satisfies it.
type Reconciler interface {
	ReconcileReport(ctx context.Context) perception.ReconcileReport
}

// Settler waits until the pipeline has processed what is queued. *engine.Engine
// satisfies it.
type Settler interface {
	Settle(ctx context.Context) error
}

// World is the part of the world model the sweep uses. *worldmodel.Store satisfies
// it.
type World interface {
	Cursor() int64
	SinceMark(ctx context.Context, name string) (worldmodel.Diff, error)
	MarkInfo(name string) (seq int64, at time.Time, ok bool)
	EnsureMark(ctx context.Context, name string) error
	SetMarkAtTime(ctx context.Context, name string, seq int64, at time.Time) error
}

// TopicSource tells which topics the triggers have decided since a time.
type TopicSource interface {
	TriggerTopics(ctx context.Context, since time.Time) ([]Topic, error)
}

// Outcome is what a sweep turn decided. Meta is the caller's own record of the turn
// (token counts, the model), handed back to Deliver unread.
type Outcome struct {
	Decisions []proactive.Decision
	Meta      any
}

// Config is everything a Runner needs. Reconciler, Settler and Deliver may be nil
// (nothing is reconciled, settled or delivered); World, Topics and Turn may not.
type Config struct {
	Reconciler Reconciler
	Settler    Settler
	World      World
	Topics     TopicSource
	// Turn runs the model on the prompt. It must be read-only and publish nothing;
	// the gateway's is a proactive heartbeat turn.
	Turn func(ctx context.Context, prompt string) (Outcome, error)
	// Deliver submits the decisions that remain after the filter, and returns a
	// one-line summary for the log.
	Deliver func(ctx context.Context, kept []proactive.Decision, o Outcome) string
	// Now is the clock; time.Now when nil.
	Now func() time.Time

	ReconcileBudget time.Duration
	SettleBudget    time.Duration
	MaxFailures     int

	// State is shared by every Runner of the process, so that the overlap guard and
	// the failure count survive a config reload.
	State *State
}

// Stats are the sweep's counters since the process started.
type Stats struct {
	Sweeps              uint64 // sweeps that ran (not skipped)
	SkippedOverlap      uint64 // ticks skipped because the previous sweep was still running
	NoChanges           uint64 // sweeps with nothing in the diff: no model call
	NoiseOnly           uint64 // sweeps whose changes were all below the noise floor: no model call
	ModelCalls          uint64
	TurnFailures        uint64
	RangesAbandoned     uint64 // ranges moved past after MaxFailures failed sweeps
	DecisionsDropped    uint64 // decisions dropped because a trigger had decided the topic
	ReconcileIncomplete uint64 // sweeps in which some source failed, did not finish or was busy
	Panics              uint64
}

// State is the part of the sweep that outlives a Runner. The zero value is ready.
type State struct {
	running atomic.Bool

	mu        sync.Mutex
	failFrom  int64 // the mark position the failures count against
	failCount int

	sweeps, skipped, noChanges, noiseOnly, modelCalls, turnFailures atomic.Uint64
	abandoned, dropped, incomplete, panics                          atomic.Uint64
}

// Stats returns the counters.
func (s *State) Stats() Stats {
	return Stats{
		Sweeps: s.sweeps.Load(), SkippedOverlap: s.skipped.Load(), NoChanges: s.noChanges.Load(),
		NoiseOnly: s.noiseOnly.Load(), ModelCalls: s.modelCalls.Load(), TurnFailures: s.turnFailures.Load(),
		RangesAbandoned: s.abandoned.Load(), DecisionsDropped: s.dropped.Load(),
		ReconcileIncomplete: s.incomplete.Load(), Panics: s.panics.Load(),
	}
}

// lostMarker is the phrase the changes section carries when the change log no longer
// reaches back to the mark (worldmodel.RenderChanges writes the same words).
const lostMarker = "older changes were lost"

// Outcome names for Result.
const (
	ResultSkipped     = "skipped"      // the previous sweep was still running
	ResultNoChanges   = "no_changes"   // nothing in the diff; no model call
	ResultNoise       = "noise_only"   // only changes below the noise floor; no model call
	ResultCompleted   = "completed"    // the model ran, its decisions were handled, the mark moved
	ResultFailed      = "failed"       // the turn (or what it needed) failed; the mark stayed
	ResultInterrupted = "interrupted"  // the service was stopping; the mark stayed
	ResultAbandoned   = "abandoned"    // failed too often over the same range; the mark moved past it
	ResultNoMark      = "mark_created" // there was no mark yet: it was created now
)

// Result is how a sweep ended.
type Result struct {
	Outcome string
	Summary string
	Err     error
}

// Runner runs sweeps. Build one per generation of services.
type Runner struct {
	cfg Config
}

// New returns a Runner over cfg.
func New(cfg Config) *Runner {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.ReconcileBudget <= 0 {
		cfg.ReconcileBudget = DefaultReconcileBudget
	}
	if cfg.SettleBudget <= 0 {
		cfg.SettleBudget = DefaultSettleBudget
	}
	if cfg.MaxFailures <= 0 {
		cfg.MaxFailures = DefaultMaxFailures
	}
	if cfg.State == nil {
		cfg.State = &State{}
	}
	return &Runner{cfg: cfg}
}

// Stats returns the shared counters.
func (r *Runner) Stats() Stats { return r.cfg.State.Stats() }

// Run is one sweep. heartbeatPrompt is the heartbeat's own prompt (the time and
// HEARTBEAT.md), which goes into the sweep's prompt after the changes.
//
//  1. The sources reconcile, together, under one budget; a source that fails or
//     does not finish is reported and the sweep goes on with what it has.
//  2. The sweep waits, briefly, for the pipeline to work off what that queued: the
//     world model has applied it, and the triggers it fired are decided.
//  3. It reads what changed since its mark. With nothing material in it there is no
//     model call: one INFO line says so, with the counts.
//  4. Otherwise the model is asked, with the changes, the topics the triggers have
//     already decided, and HEARTBEAT.md. Its decisions that repeat one of those
//     topics are dropped here, in code; the rest are submitted.
//  5. Only then does the mark move. A turn that fails or is cut off leaves it where
//     it was, so the next sweep sees the same changes; after MaxFailures failures
//     over the same range the sweep moves past it with a WARN naming the range.
//
// One sweep runs at a time; a tick that finds the previous one still running is
// skipped and counted. Run never panics.
func (r *Runner) Run(ctx context.Context, heartbeatPrompt string) (res Result) {
	st := r.cfg.State
	if !st.running.CompareAndSwap(false, true) {
		n := st.skipped.Add(1)
		logger.InfoCF("heartbeat", "heartbeat sweep skipped: the previous sweep is still running",
			map[string]any{"skipped_total": n})
		return Result{Outcome: ResultSkipped, Summary: "skipped: the previous sweep is still running"}
	}
	defer st.running.Store(false)
	defer func() {
		if p := recover(); p != nil {
			st.panics.Add(1)
			logger.ErrorCF("heartbeat", "heartbeat sweep panicked; nothing delivered",
				map[string]any{"panic": fmt.Sprint(p), "stack": string(debug.Stack())})
			res = Result{Outcome: ResultFailed, Err: fmt.Errorf("sweep panicked: %v", p)}
		}
	}()
	st.sweeps.Add(1)

	rep := r.reconcile(ctx)
	r.settle(ctx)
	if ctx.Err() != nil {
		return Result{Outcome: ResultInterrupted, Summary: "interrupted before the changes were read"}
	}

	// The position and the time are taken before the read: what arrives after is in
	// the next sweep's window, never lost.
	readAt := r.cfg.Now()
	cursor := r.cfg.World.Cursor()
	diff, err := r.cfg.World.SinceMark(ctx, Mark)
	if errors.Is(err, worldmodel.ErrNoMark) {
		if err := r.cfg.World.EnsureMark(ctx, Mark); err != nil {
			return Result{Outcome: ResultFailed, Err: fmt.Errorf("sweep mark: %w", err)}
		}
		logger.InfoCF("heartbeat", "heartbeat: no sweep mark yet; created now, nothing to compare with",
			map[string]any{"mark": Mark})
		return Result{Outcome: ResultNoMark, Summary: "created the sweep mark"}
	}
	if err != nil {
		return Result{Outcome: ResultFailed, Err: fmt.Errorf("reading changes: %w", err)}
	}
	seq, markAt, _ := r.cfg.World.MarkInfo(Mark)

	material, noise := Material(diff)
	counts := map[string]any{
		"changes": len(diff.Changes), "signals": len(diff.Signals), "material": len(material.Changes) + len(material.Signals),
		"below_noise_floor": noise, "truncated": diff.Truncated,
		"sources_ok": len(rep.OK), "sources_failed": len(rep.Failed),
		"sources_unfinished": len(rep.Unfinished), "sources_busy": len(rep.Busy),
	}
	// A mark older than what the change log still holds returns what is left and says
	// so (Truncated). The sweep cannot know what it missed, so it never reads that as
	// "nothing" or "noise": the model is asked once, told that older changes were
	// lost, and the mark moves past the gap.
	if diff.Truncated {
		material = diff
	}
	if diff.Empty() && !diff.Truncated {
		st.noChanges.Add(1)
		logger.InfoCF("heartbeat", "heartbeat: no changes", counts)
		r.advance(ctx, cursor, readAt)
		return Result{Outcome: ResultNoChanges, Summary: "no changes"}
	}
	if material.Empty() && !diff.Truncated {
		st.noiseOnly.Add(1)
		logger.InfoCF("heartbeat", "heartbeat: no material changes", counts)
		// The mark stays: a drift that is small at every look adds up in the diff
		// until it is not.
		return Result{Outcome: ResultNoise, Summary: "no material changes"}
	}

	topics, err := r.cfg.Topics.TriggerTopics(ctx, markAt)
	if err != nil {
		// Without the list the sweep cannot keep its promise not to repeat a trigger.
		logger.WarnCF("heartbeat", "heartbeat sweep skipped: the topics the triggers decided cannot be read",
			map[string]any{"error": err.Error()})
		return Result{Outcome: ResultFailed, Err: fmt.Errorf("reading the triggers' decisions: %w", err)}
	}

	changes := worldmodel.RenderChanges(material, MaxChangeItems, MaxChangeChars)
	if diff.Truncated && !strings.Contains(changes, lostMarker) {
		changes += "## Changes since your last look\n(" + lostMarker + "; none of the ones that remain is worth listing)\n"
	}
	prompt := BuildPrompt(changes, topics, heartbeatPrompt)
	st.modelCalls.Add(1)
	counts["handled_topics"] = len(topics)
	logger.InfoCF("heartbeat", "heartbeat: asking the model about the changes", counts)

	out, err := r.cfg.Turn(ctx, prompt)
	if err == nil {
		// A trigger can fire during a turn that takes minutes: read the list again,
		// right before anything is submitted.
		var later []Topic
		if later, err = r.cfg.Topics.TriggerTopics(ctx, markAt); err == nil {
			topics = union(topics, later)
		} else {
			err = fmt.Errorf("reading the triggers' decisions: %w", err)
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			return Result{Outcome: ResultInterrupted, Summary: "interrupted during the turn"}
		}
		st.turnFailures.Add(1)
		return r.failed(ctx, err, seq, cursor, readAt, markAt)
	}

	kept, dropped := Filter(out.Decisions, topics)
	for _, d := range dropped {
		st.dropped.Add(1)
		logger.InfoCF("heartbeat", "heartbeat sweep decision dropped: a trigger already decided this topic",
			map[string]any{"topic_key": d.Topic.Key, "trigger_kind": d.Topic.Kind, "asset": d.Topic.Asset,
				"decision_key": d.Decision.DedupKey})
	}
	summary := fmt.Sprintf("%d decision(s), %d dropped as repeats of trigger topics", len(out.Decisions), len(dropped))
	if r.cfg.Deliver != nil {
		summary = r.cfg.Deliver(ctx, kept, out) + "; " + fmt.Sprintf("%d dropped as repeats of trigger topics", len(dropped))
	}
	if ctx.Err() != nil {
		// Stopping: what was not handed over must be seen again by the next sweep.
		return Result{Outcome: ResultInterrupted, Summary: "interrupted before the decisions were handed over"}
	}
	r.advance(ctx, cursor, readAt)
	r.resetFailures()
	return Result{Outcome: ResultCompleted, Summary: summary}
}

// reconcile asks the sources to re-read their state, within the budget.
func (r *Runner) reconcile(ctx context.Context) perception.ReconcileReport {
	if r.cfg.Reconciler == nil {
		return perception.ReconcileReport{}
	}
	rctx, cancel := context.WithTimeout(ctx, r.cfg.ReconcileBudget)
	defer cancel()
	rep := r.cfg.Reconciler.ReconcileReport(rctx)
	if !rep.Complete() {
		r.cfg.State.incomplete.Add(1)
		failed := make([]string, 0, len(rep.Failed))
		for _, f := range rep.Failed {
			failed = append(failed, f.Source+": "+f.Err)
		}
		logger.WarnCF("heartbeat", "heartbeat sweep: some sources did not reconcile; going on with what they have", map[string]any{
			"failed": failed, "unfinished": rep.Unfinished, "busy": rep.Busy, "ok": rep.OK,
			"budget": r.cfg.ReconcileBudget.String(),
		})
	}
	return rep
}

// settle waits for the pipeline to work off what the reconcile queued.
func (r *Runner) settle(ctx context.Context) {
	if r.cfg.Settler == nil {
		return
	}
	sctx, cancel := context.WithTimeout(ctx, r.cfg.SettleBudget)
	defer cancel()
	if err := r.cfg.Settler.Settle(sctx); err != nil && ctx.Err() == nil {
		// The changes still queued are read by the next sweep: the mark only moves to
		// the position read now.
		logger.WarnCF("heartbeat", "heartbeat sweep: the pipeline did not settle; reading what it has",
			map[string]any{"error": err.Error()})
	}
}

// advance moves the mark to the position read at readAt.
func (r *Runner) advance(ctx context.Context, cursor int64, readAt time.Time) {
	if err := r.cfg.World.SetMarkAtTime(context.WithoutCancel(ctx), Mark, cursor, readAt); err != nil {
		logger.WarnCF("heartbeat", "heartbeat sweep: the mark could not be moved; the next sweep sees these changes again",
			map[string]any{"error": err.Error()})
	}
}

func (r *Runner) resetFailures() {
	st := r.cfg.State
	st.mu.Lock()
	st.failFrom, st.failCount = 0, 0
	st.mu.Unlock()
}

// failed records a failed sweep against the range it was looking at. After
// MaxFailures over the same range (the same mark position) the sweep moves past
// it, so that one diff that a model cannot handle does not cost a model call at
// every tick for ever.
func (r *Runner) failed(ctx context.Context, err error, seq, cursor int64, readAt, markAt time.Time) Result {
	st := r.cfg.State
	st.mu.Lock()
	if st.failFrom != seq || st.failCount == 0 {
		st.failFrom, st.failCount = seq, 0
	}
	st.failCount++
	n := st.failCount
	st.mu.Unlock()

	logger.WarnCF("heartbeat", "Heartbeat sweep turn failed; nothing delivered",
		map[string]any{"error": err.Error(), "failures_over_range": n, "max_failures": r.cfg.MaxFailures})
	if n < r.cfg.MaxFailures {
		return Result{Outcome: ResultFailed, Err: fmt.Errorf("sweep turn failed: %w", err)}
	}

	st.abandoned.Add(1)
	logger.WarnCF("heartbeat", "Heartbeat sweep giving up on a range of changes after repeated failures; "+
		"the mark moves past it and the model never looks at it", map[string]any{
		"from_position": seq, "to_position": cursor, "from": markAt.UTC().Format(time.RFC3339),
		"to": readAt.UTC().Format(time.RFC3339), "failures": n,
	})
	r.advance(ctx, cursor, readAt)
	r.resetFailures()
	return Result{Outcome: ResultAbandoned, Err: fmt.Errorf("sweep turn failed %d times over the same changes; moved past them: %w", n, err)}
}

// union merges two topic lists, one entry per key (the later wins), in key order.
func union(a, b []Topic) []Topic {
	by := map[string]Topic{}
	for _, t := range a {
		by[t.Key] = t
	}
	for _, t := range b {
		by[t.Key] = t
	}
	out := make([]Topic, 0, len(by))
	for _, t := range by {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}
