package gateway

import (
	"context"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/agent"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/calendar"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/sources"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/timerstore"
)

// clockProfileTimeout bounds the profile read the clock source makes each time it
// computes (at most every five minutes).
const clockProfileTimeout = 5 * time.Second

// clockSourcesFor builds the clock source for this generation and (re)loads the
// workspace's holiday files into the process-wide calendar, so a reload picks up an
// edited file. It is only called from perceptionSources, which only runs when
// perception is effectively on: a default install reads no file and starts no timer.
//
// The source reads nothing from an exchange. Its only input besides the calendar is
// the owner's time zone, read from the profile through a store over the shared
// proactive database (the same one the attention governor reads).
func clockSourcesFor(d assemblyDeps) []perception.Source {
	if err := calendar.LoadHolidays(agent.DefaultAgentWorkspace(d.Cfg)); err != nil {
		logger.WarnCF("perception", "Holiday files were not loaded", map[string]any{"error": err.Error()})
	}
	return []perception.Source{sources.NewClockSource(sources.ClockOptions{
		Location: profileZone(d),
		Timers:   clockTimerStore(d),
		Clock:    d.Shared.clk(),
	})}
}

// clockTimerStore opens the table that keeps the clock's timers (reminders) across
// restarts. Without it the timers live in memory only; that is logged, because a
// reminder would then not survive a restart.
func clockTimerStore(d assemblyDeps) perception.TimerStore {
	db, err := proactiveDB(d)
	if err != nil {
		logger.WarnCF("perception", "Clock timers are not persisted: the proactive database is unavailable",
			map[string]any{"error": err.Error()})
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), clockProfileTimeout)
	defer cancel()
	store, err := timerstore.New(ctx, db)
	if err != nil {
		logger.WarnCF("perception", "Clock timers are not persisted: the timer table could not be opened",
			map[string]any{"error": err.Error()})
		return nil
	}
	return store
}

// profileZone returns the function the clock source calls for the owner's zone. It
// returns nil whenever it cannot say (no database, a failed read, a zone that does
// not load), and the source then keeps the last zone it was given.
func profileZone(d assemblyDeps) func() *time.Location {
	db, err := proactiveDB(d)
	if err != nil {
		logger.WarnCF("perception", "Clock source cannot open the profile database; day_start uses the default time zone",
			map[string]any{"error": err.Error()})
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), clockProfileTimeout)
	defer cancel()
	store, err := openIntentStore(ctx, d, db)
	if err != nil {
		logger.WarnCF("perception", "Clock source cannot open the profile store; day_start uses the default time zone",
			map[string]any{"error": err.Error()})
		return nil
	}
	return func() *time.Location {
		ctx, cancel := context.WithTimeout(context.Background(), clockProfileTimeout)
		defer cancel()
		p, err := store.Profile(ctx)
		if err != nil {
			return nil
		}
		loc, ok := p.Location()
		if !ok {
			return nil
		}
		return loc
	}
}

// perceptionClockStats is the clock source's part of the perception stats: counts
// only.
type perceptionClockStats struct {
	SessionEvents     uint64 `json:"session_events"`
	DayStartEvents    uint64 `json:"day_start_events"`
	ReconcileEvents   uint64 `json:"reconcile_events"`
	LateEvents        uint64 `json:"late_events"`
	SkippedBoundaries uint64 `json:"skipped_boundaries"`
	SkippedDays       uint64 `json:"skipped_days"`
	Reanchors         uint64 `json:"reanchors"`
	ZoneChanges       uint64 `json:"zone_changes"`
	Wakes             uint64 `json:"wakes"`
}

// clockStats snapshots the clock source's counters, or nil when there is none.
func (rt *perceptionRuntime) clockStats() *perceptionClockStats {
	if rt == nil || rt.clock == nil {
		return nil
	}
	s := rt.clock.Stats()
	return &perceptionClockStats{
		SessionEvents: s.SessionEvents, DayStartEvents: s.DayStartEvents, ReconcileEvents: s.ReconcileEvents,
		LateEvents: s.LateEvents, SkippedBoundaries: s.SkippedBoundaries, SkippedDays: s.SkippedDays,
		Reanchors: s.Reanchors, ZoneChanges: s.ZoneChanges, Wakes: s.Wakes,
	}
}
