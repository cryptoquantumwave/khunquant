package perception

import (
	"context"
	"time"
)

// DefaultMisfireGrace is how late a timer may be seen and still fire. The clock
// source applies it; the intent compiler reads it to tell a reminder whose moment is
// gone for good.
const DefaultMisfireGrace = 6 * time.Hour

// Timer is a moment the clock source wakes the pipeline for, kept across restarts.
// Its ID is chosen by whoever schedules it (the intent compiler uses the id of the
// trigger spec the timer is for).
//
// A one-shot timer fires once at At and is then gone. A recurring one has a cron
// expression (and the zone to read it in; empty is the owner's zone): when it fires
// the clock source computes the next occurrence after the one it just fired, with
// the cron service's own schedule function, and keeps the timer.
type Timer struct {
	ID string
	// At is the next time the timer fires.
	At time.Time
	// Cron and TZ make the timer recurring. Both empty means one-shot.
	Cron string
	TZ   string
	// Until, when set, ends a recurring timer: an occurrence at or after it is not
	// scheduled (the intent's expiry). Budget, when positive, is how many times a
	// recurring timer may fire in all, and Fired how many times it already has: the
	// clock source stops the timer when the budget is spent. Both are enforced where
	// the next occurrence is computed, not only in the ledger.
	Until  time.Time
	Budget int
	Fired  int
}

// Recurring reports whether the timer repeats.
func (t Timer) Recurring() bool { return t.Cron != "" }

// TimerStore keeps the timers across restarts. The clock source reads the list at
// start and writes through it whenever the list changes. Implementations are safe
// for concurrent use and do local work only.
type TimerStore interface {
	Load(ctx context.Context) ([]Timer, error)
	Save(ctx context.Context, t Timer) error
	Delete(ctx context.Context, id string) error
}

// TimerDue is one perception.clock.timer event as a consumer that cannot import the
// clock source reads it (the time_at trigger).
type TimerDue struct {
	// ID is the timer's id.
	ID string
	// Due is the moment the timer was set for.
	Due time.Time
	// Late is set when the timer was seen more than a minute after Due (a suspend,
	// a restart): the reminder is still worth sending, and is sent once.
	Late bool
	// Missed is set when the timer came due longer ago than the misfire grace
	// allows: the moment is gone and the consumer says so instead of acting on it.
	Missed bool
}

// TimerPayload is implemented by the payload of a perception.clock.timer event. The
// world model does not track timers, so such an event reaches the triggers as an
// empty Diff whose Payload is this; a trigger type-asserts it.
type TimerPayload interface {
	TimerDue() TimerDue
}
