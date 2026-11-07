package sandbox

import (
	"fmt"
	"strings"
	"time"
)

// The SET trading day, in Asia/Bangkok (a fixed +07:00: Thailand has no DST, and a zone
// database may be missing from a minimal image). Monday to Friday; no holidays.
//
//	open auction (default 10:00)      morning session until 12:30
//	afternoon open auction (14:30)    afternoon session until 16:30
//	16:40                             a Day order's end
//
// Everything that depends on the day is in this file, as functions of the sim clock alone.

var bangkok = time.FixedZone("Asia/Bangkok", 7*3600)

const (
	setMorningEnd   = 12*time.Hour + 30*time.Minute
	setAfternoonEnd = 16*time.Hour + 30*time.Minute
	setDayEnd       = 16*time.Hour + 40*time.Minute
)

// setDay holds the opening auctions as offsets from midnight.
type setDay struct{ open, afternoon time.Duration }

// VenueOptions are the per-venue knobs of the equity simulators, set with SetVenueOptions
// (the account group's seed passes them). A zero field is the default.
type VenueOptions struct {
	AuctionOpen      string // settrade: "HH:MM" Asia/Bangkok, default 10:00
	AuctionAfternoon string // settrade: default 14:30
}

// timeOfDay reads "HH:MM".
func timeOfDay(s string) (time.Duration, error) {
	t, err := time.Parse("15:04", s)
	if err != nil || len(s) != 5 {
		return 0, fmt.Errorf("%q is not HH:MM", s)
	}
	return time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute, nil
}

// day resolves the options into offsets. The auctions must open their sessions: the first
// before the morning close and after 09:00, the second between the sessions' ends.
func (o VenueOptions) day() (setDay, error) {
	d := setDay{open: 10 * time.Hour, afternoon: 14*time.Hour + 30*time.Minute}
	for _, f := range []struct {
		name, v string
		dst     *time.Duration
		lo, hi  time.Duration
	}{
		{"auction_open", o.AuctionOpen, &d.open, 9 * time.Hour, setMorningEnd},
		{"auction_afternoon", o.AuctionAfternoon, &d.afternoon, setMorningEnd, setAfternoonEnd},
	} {
		if f.v == "" {
			continue
		}
		t, err := timeOfDay(f.v)
		if err == nil && (t < f.lo || t >= f.hi) {
			err = fmt.Errorf("%s is outside %s to %s", f.v, f.lo, f.hi)
		}
		if err != nil {
			return d, fmt.Errorf("settrade %s: %w", f.name, err)
		}
		*f.dst = t
	}
	return d, nil
}

// SetVenueOptions sets a venue's options; a venue with none is refused.
func (sim *StatefulSimulator) SetVenueOptions(venue string, o VenueOptions) error {
	if venue != "settrade" {
		return fmt.Errorf("venue %q has no options", venue)
	}
	if _, err := o.day(); err != nil {
		return err
	}
	sim.mu.Lock()
	defer sim.mu.Unlock()
	if sim.options == nil {
		sim.options = map[string]VenueOptions{}
	}
	sim.options[venue] = o
	sim.reschedule(sim.clock())
	return nil
}

// ParseVenueOptions reads the options an account seed carries. An unknown key is refused,
// and each value is checked here: the auction times are HH:MM strings.
func ParseVenueOptions(venue string, m map[string]any) (VenueOptions, error) {
	var o VenueOptions
	for k, v := range m {
		s, isStr := v.(string)
		switch {
		case venue == "settrade" && k == "auction_open" && isStr:
			o.AuctionOpen = s
		case venue == "settrade" && k == "auction_afternoon" && isStr:
			o.AuctionAfternoon = s
		case venue == "settrade" && (k == "auction_open" || k == "auction_afternoon"):
			return o, fmt.Errorf("option %s: want a string HH:MM", k)
		default:
			return o, fmt.Errorf("option %q is not an option of venue %q", k, venue)
		}
	}
	_, err := o.day()
	return o, err
}

// setDay is the simulator's SET day. Callers hold sim.mu.
func (sim *StatefulSimulator) setDay() setDay {
	d, _ := sim.options["settrade"].day() // validated when set
	return d
}

func tradingDay(t time.Time) bool {
	wd := t.In(bangkok).Weekday()
	return wd != time.Saturday && wd != time.Sunday
}

// midnight is 00:00 of t's date in Bangkok.
func midnight(t time.Time) time.Time {
	y, m, d := t.In(bangkok).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, bangkok)
}

// nextTradingDay is 00:00 of the first weekday after t's date.
func nextTradingDay(t time.Time) time.Time {
	for d := midnight(t).AddDate(0, 0, 1); ; d = d.AddDate(0, 0, 1) {
		if tradingDay(d) {
			return d
		}
	}
}

// nextAuction is the opening auction an at-the-open order placed at t joins: today's first
// one if it has not run, else today's second; after that there is none that day (ok false:
// the venue refuses it). A weekend order waits for Monday's opening.
func (d setDay) nextAuction(t time.Time) (time.Time, bool) {
	if !tradingDay(t) {
		return nextTradingDay(t).Add(d.open), true
	}
	for _, a := range []time.Duration{d.open, d.afternoon} {
		if at := midnight(t).Add(a); t.Before(at) {
			return at, true
		}
	}
	return time.Time{}, false
}

// inSession says whether continuous trading is on at t.
func (d setDay) inSession(t time.Time) bool {
	if !tradingDay(t) {
		return false
	}
	tod := t.Sub(midnight(t))
	return (tod >= d.open && tod < setMorningEnd) || (tod >= d.afternoon && tod < setAfternoonEnd)
}

// nextSession is the first instant after t that a session opens.
func (d setDay) nextSession(t time.Time) time.Time {
	if tradingDay(t) {
		for _, a := range []time.Duration{d.open, d.afternoon} {
			if at := midnight(t).Add(a); t.Before(at) {
				return at
			}
		}
	}
	return nextTradingDay(t).Add(d.open)
}

// dayEnd is 16:40 of the trade date an order placed at t belongs to: today's while it is
// not over on a weekday, else the next trading day's.
func dayEnd(t time.Time) time.Time {
	if end := midnight(t).Add(setDayEnd); tradingDay(t) && t.Before(end) {
		return end
	}
	return nextTradingDay(t).Add(setDayEnd)
}

// tradable says whether orders of venue can fill at t: always, but on Settrade only inside
// a session. Callers hold sim.mu.
func (sim *StatefulSimulator) tradable(venue string, t time.Time) bool {
	return venue != "settrade" || sim.setDay().inSession(t)
}

// restWake is when the clock should next stop for a resting limit order of the state key:
// a minute on, or on Settrade while it is shut, when the next session opens.
func (sim *StatefulSimulator) restWake(key string, now time.Time) time.Time {
	if venue, _, _ := strings.Cut(key, "."); venue == "settrade" && !sim.setDay().inSession(now) {
		return sim.setDay().nextSession(now)
	}
	return now.Add(restWatch)
}
