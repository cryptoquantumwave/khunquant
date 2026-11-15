package scenario

import (
	"context"
	"strings"
	"testing"
	"time"
)

// An absence window that ends after the sim time the run reached fails, naming both times
// (TRUST-4): "+26h" on a run that stopped at +1h proves nothing. Control: a window the run
// covered passes.
func TestEval_AbsenceWindowMustBeReached(t *testing.T) {
	f := newEvalFixtureReached(t, time.Hour)
	r := f.check(`message: {to: owner, count: 0}
    within: {from: "+0m", to: "+26h"}`, false)
	if !strings.Contains(r.Got, "2026-10-21T11:00:00+07:00") || !strings.Contains(r.Got, "2026-10-20T10:00:00+07:00") {
		t.Errorf("got %q, want it to name the window end and the time reached", r.Got)
	}
	f.check(`message: {to: owner, count: 0}
    within: {from: "+0m", to: "+1h"}`, true)
	f.check(`message: {to: owner, count: 0}
    within: {from: "+2h", to: "+3h"}`, false) // a window that begins after the run is vacuous too
	// A positive check is not an absence check: it only needs its entry.
	f.chatOut("dm:owner", "hi", "info", true, 10*time.Minute)
	f.check(`message: {to: owner, count: 1}
    within: {from: "+0m", to: "+26h"}`, true)
}

// With no profile.timezone, during and next HH:MM are read in the offset of clock.start,
// not UTC (TRUST-4); a profile time zone still wins.
func TestZoneOf_FallsBackToTheClockStartOffset(t *testing.T) {
	_, off := time.Now().In(zoneOf(parseString(t, base))).Zone()
	if off != 7*3600 {
		t.Errorf("no profile zone: offset = %d, want +07:00", off)
	}
	withZone := parseString(t, base+"profile: {timezone: America/New_York}\n")
	if got := zoneOf(withZone).String(); got != "America/New_York" {
		t.Errorf("profile zone = %s", got)
	}
	utc := parseString(t, "version: 1\nscenario: t\nclock: {start: \"2026-10-20T09:00:00Z\"}\n")
	if got := zoneOf(utc); got != time.UTC && got.String() != "+00:00" {
		t.Errorf("a Z start gives %s", got)
	}

	// The effect: a message at 23:30 Bangkok (16:30 UTC) is inside a 22:00-07:00 window.
	f := newEvalFixture(t)
	f.chatOut("dm:owner", "night note", "info", true, 14*time.Hour+30*time.Minute)
	sc := parseString(t, base+"expect:\n  - message: {to: owner, count: 0}\n    during: \"22:00-07:00\"\n")
	run := func(loc *time.Location) bool {
		f.v.loc = loc
		r, _ := f.v.eval(context.Background(), 0, &sc.Expect[0])
		return r.OK
	}
	if run(zoneOf(sc)) {
		t.Error("with the clock.start offset the night message is inside the window, so the absence fails")
	}
	if !run(time.UTC) { // the old fallback: the message is at 16:30, outside 22:00-07:00
		t.Error("control: in UTC the window misses the message")
	}
}

// A file that reads the clock in a zone without naming it is warned; naming it silences the
// warning, and so does using no zone feature.
func TestWarnings_NoProfileZone(t *testing.T) {
	during := base + "expect:\n  - message: {to: owner, count: 0}\n    during: \"22:00-07:00\"\n"
	if w := Warnings(parseString(t, during)); len(w) != 1 || !strings.Contains(w[0], "+07:00") {
		t.Errorf("warnings = %v", w)
	}
	nextHHMM := base + "timeline:\n  - run_until: next 08:00\nexpect:\n  - logs: {}\n"
	if w := Warnings(parseString(t, nextHHMM)); len(w) != 1 {
		t.Errorf("next HH:MM warnings = %v", w)
	}
	if w := Warnings(parseString(t, "profile: {timezone: Asia/Bangkok}\n"+during)); len(w) != 0 {
		t.Errorf("with a zone: %v", w)
	}
	if w := Warnings(parseString(t, base+"expect:\n  - logs: {}\n")); len(w) != 0 {
		t.Errorf("no zone feature: %v", w)
	}
	if issues := Validate(parseString(t, base+"profile: {timezone: Mars/Olympus}\nexpect:\n  - logs: {}\n"), nil); issueAt(issues, "/profile/timezone") == "" {
		t.Errorf("an unknown zone name was accepted: %v", issues)
	}
}
