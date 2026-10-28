package gateway

import (
	"context"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive/engine"
	"github.com/cryptoquantumwave/khunquant/pkg/worldmodel"
)

// worldModelStopTimeout bounds the final checkpoint that stopping the pipeline
// writes.
const worldModelStopTimeout = 10 * time.Second

// worldModelStage adapts *worldmodel.Store to engine.WorldModel (and
// engine.Housekeeper). pkg/proactive/engine keeps its Diff and View as `any` so
// that it stays independent of pkg/worldmodel; this adapter is the one place the
// concrete types are boxed. Whatever evaluates triggers (the next change) unboxes
// them once, in its own adapter, and hands its triggers a typed worldmodel.View and
// worldmodel.Diff.
//
//	engine.Diff is a worldmodel.Diff (a value);
//	engine.View is a worldmodel.View.
//
// Apply returns the Diff of every event it applied without error, the empty one
// included (the event's envelope and time, no change, no signal): a trigger is
// evaluated for every event of a kind it lists, changed or not, because a fact it
// acts on can move in the View without a Change (the last price inside an open
// minute bucket; the mark and liquidation prices of a futures position whose
// contract count did not change). The engine does not publish an empty Diff on the
// bus (Diff.Empty), so this costs the bus nothing. An event of a kind the model
// does not track (a session boundary, a device, an external trigger, a placed
// order) is passed on the same way, as an envelope-only Diff, so that the
// evaluator is told it happened; the Diff carries its kind, source, time and
// trust, not its payload. Apply returns a nil Diff only together with an error.
type worldModelStage struct{ s *worldmodel.Store }

func (w worldModelStage) Apply(ctx context.Context, ev events.Event) (engine.Diff, error) {
	d, err := w.s.Apply(ctx, ev)
	if err != nil {
		return nil, err
	}
	return d, nil
}

func (w worldModelStage) View() engine.View { return w.s.View() }

// Housekeep forwards the engine's per-second call. The store only decides whether
// a checkpoint is due and starts it on a goroutine of its own.
func (w worldModelStage) Housekeep(ctx context.Context, now time.Time) { w.s.Housekeep(ctx, now) }

var (
	_ engine.WorldModel  = worldModelStage{}
	_ engine.Housekeeper = worldModelStage{}
)

// worldModelRuntime is what buildPerception leaves on the runtime when the world
// model is built.
type worldModelRuntime struct {
	store *worldmodel.Store
	// detach removes the changes contributor from the agent loop.
	detach func()
}

// attachWorldModel builds the world model over the shared proactive database and
// hands it to the engine options, and attaches its "changes" section to the agent
// loop's background prompts. It runs only from buildPerception, so nothing is
// built, no table is created and no goroutine started unless perception is
// effectively on. A world model that cannot be built is logged and left out: the
// pipeline then runs without it, as it did before it existed.
func (rt *perceptionRuntime) attachWorldModel(d assemblyDeps, opts *engine.Options) {
	db, err := proactiveDB(d)
	if err != nil {
		logger.WarnCF("perception", "World model disabled: the proactive database is unavailable",
			map[string]any{"error": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var zoneOpt []worldmodel.Option
	if zone := profileZone(d); zone != nil {
		zoneOpt = append(zoneOpt, worldmodel.WithZone(zone))
	}
	store, err := worldmodel.NewStore(ctx, db, d.Shared.clk(), zoneOpt...)
	if err != nil {
		logger.WarnCF("perception", "World model disabled: it could not be opened",
			map[string]any{"error": err.Error()})
		return
	}
	rt.worldModel = &worldModelRuntime{store: store, detach: func() {}}
	opts.WorldModel = worldModelStage{s: store}

	// The window of the "changes" section starts when the model starts, not at the
	// beginning of time; it moves when a proactive turn completes.
	if err := store.EnsureMark(ctx, worldmodel.MarkProactive); err != nil {
		logger.WarnCF("perception", "World model mark could not be set", map[string]any{"error": err.Error()})
	}
	if d.AgentLoop != nil {
		rt.worldModel.detach = d.AgentLoop.SetBackgroundSection(worldmodel.NewChangesContributor(store))
	}
}

// stopWorldModel detaches the section and writes the final checkpoint. It runs
// after the engine has drained, so every event the pipeline held is in the model.
func (rt *perceptionRuntime) stopWorldModel() {
	wm := rt.worldModel
	if wm == nil {
		return
	}
	wm.detach()
	ctx, cancel := context.WithTimeout(context.Background(), worldModelStopTimeout)
	defer cancel()
	if err := wm.store.Close(ctx); err != nil {
		logger.WarnCF("perception", "World model final checkpoint failed", map[string]any{"error": err.Error()})
	}
}

// worldModelStats are the world model's counters for the stats route: counts only.
type worldModelStats struct {
	Applied           uint64 `json:"applied"`
	Ignored           uint64 `json:"ignored"`
	DroppedInvalid    uint64 `json:"dropped_invalid"`
	DroppedBadName    uint64 `json:"dropped_bad_name"`
	DroppedOutOfOrder uint64 `json:"dropped_out_of_order"`
	DroppedPaper      uint64 `json:"dropped_paper"`
	DecodeErrors      uint64 `json:"decode_errors"`
	Panics            uint64 `json:"panics"`
	FutureClamped     uint64 `json:"future_clamped"`
	Truncated         uint64 `json:"truncated"`

	SeriesEvicted    uint64 `json:"series_evicted"`
	FactsEvicted     uint64 `json:"facts_evicted"`
	ChangesLogged    uint64 `json:"changes_logged"`
	ChangesDropped   uint64 `json:"changes_dropped"`
	SnapshotsRebased uint64 `json:"snapshots_rebased"`

	Checkpoints        uint64 `json:"checkpoints"`
	CheckpointFailures uint64 `json:"checkpoint_failures"`
	RestoredRows       uint64 `json:"restored_rows"`
	DetectorsReset     uint64 `json:"detectors_reset"`

	Series      int `json:"series"`
	BufferedLog int `json:"buffered_log"`
	DirtyRows   int `json:"dirty_rows"`
	// SecondsSinceCheckpoint is how long ago the last checkpoint succeeded (the
	// start of the store counts as one).
	SecondsSinceCheckpoint int64 `json:"seconds_since_checkpoint"`
}

func (rt *perceptionRuntime) worldModelStats() *worldModelStats {
	if rt == nil || rt.worldModel == nil {
		return nil
	}
	s := rt.worldModel.store.Stats()
	out := &worldModelStats{
		Applied: s.Applied, Ignored: s.Ignored, DroppedInvalid: s.DroppedInvalid, DroppedBadName: s.DroppedBadName,
		DroppedOutOfOrder: s.DroppedOutOfOrder, DroppedPaper: s.DroppedPaper, DecodeErrors: s.DecodeErrors,
		Panics: s.Panics, FutureClamped: s.FutureClamped, Truncated: s.Truncated,
		SeriesEvicted: s.SeriesEvicted, FactsEvicted: s.FactsEvicted, ChangesLogged: s.ChangesLogged,
		ChangesDropped: s.ChangesDropped, SnapshotsRebased: s.SnapshotsRebased,
		Checkpoints: s.Checkpoints, CheckpointFailures: s.CheckpointFailures, RestoredRows: s.RestoredRows,
		DetectorsReset: s.DetectorsReset,
		Series:         s.Series, BufferedLog: s.BufferedLog, DirtyRows: s.DirtyRows,
	}
	if !s.LastCheckpoint.IsZero() {
		out.SecondsSinceCheckpoint = max(int64(time.Since(s.LastCheckpoint).Seconds()), 0)
	}
	return out
}
