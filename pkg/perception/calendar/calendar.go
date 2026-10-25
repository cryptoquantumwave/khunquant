// Package calendar answers "is this market open?" and "when does it next open or
// close?" for the markets the proactive agent watches. It exists so that rules
// such as "only evaluate a market's moves while it is open" run in code instead
// of being prose the model has to follow.
//
// Sessions, [Open, Close) in the market's own time zone:
//
//   - SET (Stock Exchange of Thailand): Monday to Friday, 10:00-12:30 and
//     14:30-16:30 Asia/Bangkok. The lunch break is a closed period, so it has a
//     close boundary at 12:30 and an open boundary at 14:30.
//   - US equities (NYSE): Monday to Friday, 09:30-16:00 America/New_York. The zone
//     database applies daylight saving time; each session is built from the local
//     wall-clock date, so the UTC instant moves by an hour across a DST change.
//   - Crypto: always open. It has no boundaries.
//
// Out of scope, on purpose: half days (NYSE early closes, the SET's closing
// auction and pre-open periods) and special sessions. They are treated as
// ordinary full sessions. A market is "open" here when its regular continuous
// session is running.
//
// Holidays come from user-editable files, <workspace>/calendar/set_holidays.json
// and us_holidays.json (see LoadHolidays for the format). A wrong holiday would
// silently switch off evaluation on a trading day, so the rules are:
//
//   - a list says which years it covers; a day in a year it does not cover is a
//     normal day and logs one WARN per market and year per process;
//   - a missing, oversized or malformed file means "no holidays for that market"
//     with a WARN, never a panic and never a half-applied list.
//
// The files are state that governs what the agent evaluates, so
// pkg/tools lists workspace/calendar/ among the paths an autonomous turn may not
// write.
//
// Dependency rule: this package imports pkg/logger and the standard library.
package calendar

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/logger"
)

// Market is a market whose sessions the calendar knows.
type Market string

// The markets.
const (
	Crypto Market = "crypto"
	SET    Market = "set"
	US     Market = "us"
)

// Valid reports whether m is one of the known markets.
func (m Market) Valid() bool { return m == Crypto || m == SET || m == US }

// Phase names of a boundary, as NextBoundary returns them.
const (
	PhaseOpen  = "open"
	PhaseClose = "close"
)

// lookaheadDays bounds how far NextBoundary searches. The longest run of closed
// days on these calendars is under ten, so two weeks always reaches a session; a
// calendar with nothing in two weeks is treated as having no next boundary.
const lookaheadDays = 14

// Session is one continuous trading session: open from Open up to, but not
// including, Close.
type Session struct {
	Market Market
	Open   time.Time
	Close  time.Time
}

// span is a session as wall-clock minutes in the market's zone.
type span struct{ open, close int } // minutes after local midnight

var (
	setSpans = []span{{10*60 + 0, 12*60 + 30}, {14*60 + 30, 16*60 + 30}}
	usSpans  = []span{{9*60 + 30, 16*60 + 0}}
)

var (
	locOnce [3]sync.Once
	locVal  [3]*time.Location
)

// location returns the zone a market's sessions are defined in. ok is false when
// it cannot be loaded (a host without a zone database and without an embedded
// one): the caller treats the market as having no known sessions.
//
// Thailand has had no daylight saving time for decades, so the SET falls back to
// a fixed UTC+7 rather than to "unknown". The US has no safe fixed offset.
func location(m Market) (*time.Location, bool) {
	var i int
	var name string
	switch m {
	case SET:
		i, name = 1, "Asia/Bangkok"
	case US:
		i, name = 2, "America/New_York"
	default:
		return time.UTC, true
	}
	locOnce[i].Do(func() {
		loc, err := time.LoadLocation(name)
		switch {
		case err == nil:
			locVal[i] = loc
		case m == SET:
			locVal[i] = time.FixedZone("ICT", 7*3600)
		default:
			logger.WarnCF("perception", "Market time zone is unavailable; its sessions are unknown and it is treated as open",
				map[string]any{"market": string(m), "zone": name, "error": err.Error()})
		}
	})
	return locVal[i], locVal[i] != nil
}

// Calendar holds the holiday lists. The zero value has none. It is safe for
// concurrent use, and LoadHolidays may be called on a live calendar to reload.
type Calendar struct {
	tables atomic.Pointer[tableSet]
	warned sync.Map // "market/year" -> struct{}: the year-not-covered WARN, once
}

// New returns a calendar with no holidays.
func New() *Calendar { return &Calendar{} }

// Default is the calendar the package-level functions use, and the one the
// gateway loads the workspace's holiday files into.
var Default = New()

// Sessions is Default.Sessions.
func Sessions(m Market, day time.Time) []Session { return Default.Sessions(m, day) }

// IsOpen is Default.IsOpen.
func IsOpen(m Market, t time.Time) bool { return Default.IsOpen(m, t) }

// NextBoundary is Default.NextBoundary.
func NextBoundary(m Market, t time.Time) (time.Time, string) { return Default.NextBoundary(m, t) }

// LoadHolidays is Default.LoadHolidays.
func LoadHolidays(workspace string) error { return Default.LoadHolidays(workspace) }

// Sessions returns the sessions of market m on the calendar day that contains day,
// as that day is read in the market's own zone (a UTC instant late on Sunday is
// already Monday in Bangkok). It returns none on a weekend or a holiday.
//
// Crypto has a single session spanning the UTC day. A market whose zone cannot
// be loaded returns none.
func (c *Calendar) Sessions(m Market, day time.Time) []Session {
	if m == Crypto {
		y, mo, d := day.UTC().Date()
		start := time.Date(y, mo, d, 0, 0, 0, 0, time.UTC)
		return []Session{{Market: m, Open: start, Close: start.AddDate(0, 0, 1)}}
	}
	loc, ok := location(m)
	if !ok {
		return nil
	}
	spans, ok := spansFor(m)
	if !ok {
		return nil
	}
	y, mo, d := day.In(loc).Date()
	return c.sessionsOn(m, loc, spans, y, mo, d)
}

// spansFor returns the regular sessions of a market with a timetable.
func spansFor(m Market) ([]span, bool) {
	switch m {
	case SET:
		return setSpans, true
	case US:
		return usSpans, true
	}
	return nil, false
}

func (c *Calendar) sessionsOn(m Market, loc *time.Location, spans []span, y int, mo time.Month, d int) []Session {
	date := time.Date(y, mo, d, 12, 0, 0, 0, loc) // noon: d may run past the month's end
	y, mo, d = date.Date()
	if wd := date.Weekday(); wd == time.Saturday || wd == time.Sunday {
		return nil
	}
	if c.isHoliday(m, y, mo, d) {
		return nil
	}
	out := make([]Session, 0, len(spans))
	for _, sp := range spans {
		out = append(out, Session{
			Market: m,
			Open:   time.Date(y, mo, d, sp.open/60, sp.open%60, 0, 0, loc),
			Close:  time.Date(y, mo, d, sp.close/60, sp.close%60, 0, 0, loc),
		})
	}
	return out
}

// IsOpen reports whether market m is in a session at t. A session is open at its
// Open instant and closed at its Close instant.
//
// A market whose zone cannot be loaded is reported open: a calendar that cannot
// tell must not silence the evaluation it gates.
func (c *Calendar) IsOpen(m Market, t time.Time) bool {
	if m == Crypto {
		return true
	}
	if _, ok := location(m); !ok {
		return true
	}
	for _, s := range c.Sessions(m, t) {
		if !t.Before(s.Open) && t.Before(s.Close) {
			return true
		}
	}
	return false
}

// NextBoundary returns the first session boundary strictly after t, and whether it
// is an "open" or a "close". It returns the zero time and "" when the market has no
// boundaries (crypto, an unloadable zone) or none within two weeks.
func (c *Calendar) NextBoundary(m Market, t time.Time) (time.Time, string) {
	spans, ok := spansFor(m)
	if !ok {
		return time.Time{}, ""
	}
	loc, ok := location(m)
	if !ok {
		return time.Time{}, ""
	}
	y, mo, d := t.In(loc).Date()
	for i := 0; i <= lookaheadDays; i++ {
		for _, s := range c.sessionsOn(m, loc, spans, y, mo, d+i) {
			if s.Open.After(t) {
				return s.Open, PhaseOpen
			}
			if s.Close.After(t) {
				return s.Close, PhaseClose
			}
		}
	}
	return time.Time{}, ""
}
