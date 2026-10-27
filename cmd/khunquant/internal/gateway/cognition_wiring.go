package gateway

import (
	"context"
	"sync"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/agent"
	"github.com/cryptoquantumwave/khunquant/pkg/cognition"
	"github.com/cryptoquantumwave/khunquant/pkg/control"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
)

const (
	// cognitionStatsTTL is how long the stats route reuses a snapshot: the scheduler's
	// Stats reads the database.
	cognitionStatsTTL = 2 * time.Second
	// cognitionPoll is how often stop looks for the queue to have drained.
	cognitionPoll = 25 * time.Millisecond
)

// cognitionDrainShare is the fraction of the stop timeout queued and running jobs
// get to finish before they are cancelled. A variable only for tests.
var cognitionDrainShare = 3

// cognitionRuntime is the cognition scheduler of one generation of services
// (pkg/cognition): every model-driven turn the agent loop runs is a job on it.
// It exists only when proactive.cognition is effectively on; without it nothing
// is built, no goroutine starts, and the agent loop takes the path it always did.
//
// The one goroutine that exists while it runs is Scheduler.Run (and its workers),
// owned by this runtime and stopped by stop.
type cognitionRuntime struct {
	al     *agent.AgentLoop
	sched  *cognition.Scheduler
	cancel context.CancelFunc
	done   chan struct{}

	mu       sync.Mutex
	cached   *cognitionStatsResponse
	cachedAt time.Time
}

// buildCognition builds and starts the scheduler and installs it on the agent
// loop. It also clears a scheduler a previous generation left installed, so a
// reload that turns cognition off really turns it off. The budgets live in the
// shared proactive database, so a reload keeps the day's spend.
func buildCognition(d assemblyDeps) *cognitionRuntime {
	d.AgentLoop.SetScheduler(nil)
	if !d.Cfg.Proactive.Effective().Cognition {
		return nil
	}
	db, err := proactiveDB(d)
	if err != nil {
		logger.WarnCF("cognition", "Cognition scheduler not started: the proactive database is unavailable; "+
			"turns run as they do without it", map[string]any{"error": err.Error()})
		return nil
	}
	sched, err := cognition.New(d.Cfg.Proactive.Cognition, db, d.Shared.clk(), profileZone(d))
	if err != nil {
		logger.WarnCF("cognition", "Cognition scheduler not started; turns run as they do without it",
			map[string]any{"error": err.Error()})
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	rt := &cognitionRuntime{al: d.AgentLoop, sched: sched, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(rt.done)
		if err := sched.Run(ctx); err != nil {
			logger.WarnCF("cognition", "Scheduler stopped with jobs still running", map[string]any{"error": err.Error()})
		}
	}()
	d.AgentLoop.SetScheduler(sched)
	return rt
}

// stop lets the jobs queued or running finish for up to a share of timeout, then
// cancels what is still running; what is still queued is dropped without a
// fallback, and a user message among it is handled by the loop as usual (see
// AgentLoop.dispatchInbound). It waits for the workers.
//
// The stopped scheduler stays installed on the agent loop until the next
// generation replaces it (buildCognition): a producer that submits meanwhile is
// refused with ErrStopped, which a budgeted turn turns into a skip (the composer
// sends its template) and an unbudgeted one into today's inline path. Removing it
// would let the final flush of the outbox run composer turns inline, outside the
// budget.
//
// It runs right after the heartbeat stops and before anything a job uses: a job
// hands its decisions to attention, delivers through notify and the channels and
// reads the workflow engine, so none of them may stop first.
func (rt *cognitionRuntime) stop(timeout time.Duration) {
	if rt == nil {
		return
	}
	//simclock:wall the stop drain: bounded in wall time
	drain := time.Now().Add(timeout / time.Duration(cognitionDrainShare))
	//simclock:wall the stop drain: bounded in wall time
	for rt.sched.Pending() > 0 && time.Now().Before(drain) {
		//simclock:wall the stop drain: bounded in wall time
		time.Sleep(cognitionPoll)
	}
	rt.cancel()
	select {
	case <-rt.done:
	//simclock:wall a shutdown wait, bounded in wall time
	case <-time.After(timeout):
		logger.WarnCF("cognition", "Scheduler did not stop in time", nil)
	}
}

// canceller is what /stop uses to drop the queue; nil, not a typed nil, when
// cognition is off.
func (rt *cognitionRuntime) canceller() control.QueueCanceller {
	if rt == nil {
		return nil
	}
	return rt.sched
}

// cognitionStatsResponse is the scheduler's part of GET /api/proactive/stats:
// counts only.
type cognitionStatsResponse struct {
	Workers       int                           `json:"workers"`
	Running       int                           `json:"running"`
	Queued        map[string]int                `json:"queued"`
	Day           string                        `json:"day"`
	WakesLastHour int                           `json:"wakes_last_hour"`
	WakesPerHour  int                           `json:"wakes_per_hour"`
	TokensPerDay  int                           `json:"tokens_per_day"`
	Budget        map[string]cognitionBudgetUse `json:"budget"`
}

type cognitionBudgetUse struct {
	Wakes        int `json:"wakes"`
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// stats returns the scheduler's counters, at most cognitionStatsTTL old, or nil
// while cognition is off. Safe on a nil runtime.
func (rt *cognitionRuntime) stats() *cognitionStatsResponse {
	if rt == nil {
		return nil
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	//simclock:wall a diagnostic stats cache, not a domain decision
	if rt.cached != nil && time.Since(rt.cachedAt) < cognitionStatsTTL {
		return rt.cached
	}
	st := rt.sched.Stats()
	out := &cognitionStatsResponse{
		Workers: st.Workers, Running: st.Running, Queued: st.Queued, Day: st.Day,
		WakesLastHour: st.WakesLastHour, WakesPerHour: st.WakesPerHour, TokensPerDay: st.TokensPerDay,
		Budget: make(map[string]cognitionBudgetUse, len(st.Budget)),
	}
	for k, u := range st.Budget {
		out.Budget[k] = cognitionBudgetUse{Wakes: u.Wakes, InputTokens: u.InputTokens, OutputTokens: u.OutputTokens}
	}
	//simclock:wall a diagnostic stats cache, not a domain decision
	rt.cached, rt.cachedAt = out, time.Now()
	return out
}

// withCognitionStats adds the scheduler's counters; f may be nil, and may return
// nil while cognition is off.
func withCognitionStats(f func() *cognitionStatsResponse) statsPart {
	return func(r *busStatsResponse) {
		if f != nil {
			r.Cognition = f()
		}
	}
}
