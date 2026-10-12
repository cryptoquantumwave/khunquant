package calendar

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/logger"
)

// repoWorkspace returns the repository's workspace/ directory by walking up to
// go.mod, so the tests run on any checkout.
func repoWorkspace(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return filepath.Join(dir, "workspace")
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test's working directory")
		}
		dir = parent
	}
}

// shipped returns a calendar loaded from the holiday files this repository ships.
func shipped(t *testing.T) *Calendar {
	t.Helper()
	c := New()
	if err := c.LoadHolidays(repoWorkspace(t)); err != nil {
		t.Fatal(err)
	}
	tables := c.tables.Load()
	if tables == nil || (*tables)[SET] == nil || (*tables)[US] == nil {
		t.Fatal("the shipped holiday files did not load: fail loudly rather than test an empty calendar")
	}
	return c
}

type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// captureLogs tees the logger into a buffer for the rest of the test.
func captureLogs(t *testing.T) *syncBuf {
	t.Helper()
	buf := &syncBuf{}
	logger.SetAdditionalWriter(buf)
	t.Cleanup(func() { logger.SetAdditionalWriter(nil) })
	return buf
}

func utc(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// The expected values below are written out by hand from the published session
// times, as UTC instants, and not derived with the code under test:
//
//	SET  10:00-12:30, 14:30-16:30 Asia/Bangkok (UTC+7, no DST)
//	     = 03:00-05:30Z and 07:30-09:30Z
//	NYSE 09:30-16:00 America/New_York
//	     = 14:30-21:00Z in EST (UTC-5) and 13:30-20:00Z in EDT (UTC-4)
//	US DST 2026: starts Sun Mar 8, ends Sun Nov 1.
//
// 2026-10-12 is a Monday; 2026-10-13 (Tue) and 2026-10-23 (Fri) are SET holidays;
// 2026-07-03 (Fri), 2026-11-26 (Thu) and 2026-04-03 (Fri) are NYSE holidays.
func TestIsOpen_HandChecked(t *testing.T) {
	c := shipped(t)
	cases := []struct {
		name   string
		m      Market
		at     string
		wantIn bool
	}{
		{"SET one second before the morning open", SET, "2026-10-12T02:59:59Z", false},
		{"SET morning open is inclusive", SET, "2026-10-12T03:00:00Z", true},
		{"SET last second before the lunch break", SET, "2026-10-12T05:29:59Z", true},
		{"SET lunch break starts at 12:30", SET, "2026-10-12T05:30:00Z", false},
		{"SET middle of the lunch break (13:30)", SET, "2026-10-12T06:30:00Z", false},
		{"SET afternoon open at 14:30", SET, "2026-10-12T07:30:00Z", true},
		{"SET last second before the close", SET, "2026-10-12T09:29:59Z", true},
		{"SET close at 16:30 is exclusive", SET, "2026-10-12T09:30:00Z", false},
		{"SET Saturday", SET, "2026-10-10T04:00:00Z", false},
		{"SET holiday Tue 13 Oct, 11:00 Bangkok", SET, "2026-10-13T04:00:00Z", false},
		{"SET day after the holiday, 11:00 Bangkok", SET, "2026-10-14T04:00:00Z", true},
		{"US Fri 6 Mar (EST) 09:29:59", US, "2026-03-06T14:29:59Z", false},
		{"US Fri 6 Mar (EST) open 09:30", US, "2026-03-06T14:30:00Z", true},
		{"US Mon 9 Mar (EDT) 09:29:59", US, "2026-03-09T13:29:59Z", false},
		{"US Mon 9 Mar (EDT) open 09:30 is 13:30Z", US, "2026-03-09T13:30:00Z", true},
		{"US Fri 30 Oct (EDT) open", US, "2026-10-30T13:30:00Z", true},
		{"US Fri 30 Oct (EDT) last second", US, "2026-10-30T19:59:59Z", true},
		{"US Fri 30 Oct (EDT) close 16:00 is 20:00Z", US, "2026-10-30T20:00:00Z", false},
		{"US Mon 2 Nov (EST) 13:30Z is only 08:30", US, "2026-11-02T13:30:00Z", false},
		{"US Mon 2 Nov (EST) open is 14:30Z", US, "2026-11-02T14:30:00Z", true},
		{"US Mon 2 Nov (EST) close 16:00 is 21:00Z", US, "2026-11-02T21:00:00Z", false},
		{"US Sunday of the spring DST change", US, "2026-03-08T15:00:00Z", false},
		{"US Thu 2 Jul trading day", US, "2026-07-02T15:00:00Z", true},
		{"US Fri 3 Jul Independence Day observed", US, "2026-07-03T15:00:00Z", false},
		{"US Good Friday", US, "2026-04-03T15:00:00Z", false},
		{"US Thanksgiving", US, "2026-11-26T15:00:00Z", false},
		{"US day after Thanksgiving (an early close elsewhere; a full day here)", US, "2026-11-27T15:00:00Z", true},
		{"US 2027 Good Friday", US, "2027-03-26T15:00:00Z", false},
		{"crypto on a Sunday at 03:00Z", Crypto, "2026-10-11T03:00:00Z", true},
	}
	for _, tc := range cases {
		if got := c.IsOpen(tc.m, utc(t, tc.at)); got != tc.wantIn {
			t.Errorf("%s: IsOpen(%s, %s) = %v, want %v", tc.name, tc.m, tc.at, got, tc.wantIn)
		}
	}
}

func TestSET_LunchBreakClosed(t *testing.T) {
	c := shipped(t)
	day := utc(t, "2026-10-12T00:00:00Z")
	ss := c.Sessions(SET, day)
	if len(ss) != 2 {
		t.Fatalf("SET has two sessions on a trading day, got %d", len(ss))
	}
	if want := utc(t, "2026-10-12T05:30:00Z"); !ss[0].Close.Equal(want) {
		t.Errorf("morning close = %v, want %v", ss[0].Close, want)
	}
	if want := utc(t, "2026-10-12T07:30:00Z"); !ss[1].Open.Equal(want) {
		t.Errorf("afternoon open = %v, want %v", ss[1].Open, want)
	}
	for _, at := range []string{"2026-10-12T05:30:00Z", "2026-10-12T06:00:00Z", "2026-10-12T07:29:59Z"} {
		if c.IsOpen(SET, utc(t, at)) {
			t.Errorf("SET is open during the lunch break at %s", at)
		}
	}
	// The break is a pair of boundaries: a close at 12:30 and an open at 14:30.
	at, phase := c.NextBoundary(SET, utc(t, "2026-10-12T04:00:00Z"))
	if phase != "close" || !at.Equal(utc(t, "2026-10-12T05:30:00Z")) {
		t.Errorf("next boundary from 11:00 = %v %q, want 05:30Z close", at, phase)
	}
	at, phase = c.NextBoundary(SET, utc(t, "2026-10-12T05:30:00Z"))
	if phase != "open" || !at.Equal(utc(t, "2026-10-12T07:30:00Z")) {
		t.Errorf("next boundary from 12:30 = %v %q, want 07:30Z open", at, phase)
	}
}

func TestUS_DSTShift(t *testing.T) {
	c := shipped(t)
	opens := []struct{ day, wantOpenUTC, wantCloseUTC string }{
		{"2026-03-06T12:00:00Z", "2026-03-06T14:30:00Z", "2026-03-06T21:00:00Z"}, // Fri, EST
		{"2026-03-09T12:00:00Z", "2026-03-09T13:30:00Z", "2026-03-09T20:00:00Z"}, // Mon, EDT
		{"2026-10-30T12:00:00Z", "2026-10-30T13:30:00Z", "2026-10-30T20:00:00Z"}, // Fri, EDT
		{"2026-11-02T12:00:00Z", "2026-11-02T14:30:00Z", "2026-11-02T21:00:00Z"}, // Mon, EST
	}
	for _, o := range opens {
		ss := c.Sessions(US, utc(t, o.day))
		if len(ss) != 1 {
			t.Fatalf("%s: %d sessions, want 1", o.day, len(ss))
		}
		if want := utc(t, o.wantOpenUTC); !ss[0].Open.Equal(want) {
			t.Errorf("%s: open = %v, want %v", o.day, ss[0].Open.UTC(), want)
		}
		if want := utc(t, o.wantCloseUTC); !ss[0].Close.Equal(want) {
			t.Errorf("%s: close = %v, want %v", o.day, ss[0].Close.UTC(), want)
		}
	}
	// The boundary search crosses the change: from Friday's close the next open
	// is Monday's, an hour earlier in UTC than the Friday one.
	at, phase := c.NextBoundary(US, utc(t, "2026-03-06T21:00:00Z"))
	if phase != "open" || !at.Equal(utc(t, "2026-03-09T13:30:00Z")) {
		t.Errorf("next boundary after the Friday close = %v %q, want Mon 13:30Z open", at, phase)
	}
	at, phase = c.NextBoundary(US, utc(t, "2026-10-30T20:00:00Z"))
	if phase != "open" || !at.Equal(utc(t, "2026-11-02T14:30:00Z")) {
		t.Errorf("next boundary after the Friday close (autumn) = %v %q, want Mon 14:30Z open", at, phase)
	}
}

func TestHolidayClosed(t *testing.T) {
	c := shipped(t)
	// SET: Tue 13 Oct is a holiday, so the close on Monday is followed by an open
	// on Wednesday.
	at, phase := c.NextBoundary(SET, utc(t, "2026-10-12T09:30:00Z"))
	if phase != "open" || !at.Equal(utc(t, "2026-10-14T03:00:00Z")) {
		t.Errorf("SET next boundary over the 13 Oct holiday = %v %q, want Wed 03:00Z open", at, phase)
	}
	if got := c.Sessions(SET, utc(t, "2026-10-13T00:00:00Z")); len(got) != 0 {
		t.Errorf("SET sessions on a holiday = %v, want none", got)
	}
	if got := c.Sessions(SET, utc(t, "2026-10-14T00:00:00Z")); len(got) != 2 {
		t.Errorf("SET sessions the day after a holiday = %d, want 2", len(got))
	}
	// The Songkran run (Mon 13 to Wed 15 April 2026) is closed end to end.
	at, phase = c.NextBoundary(SET, utc(t, "2026-04-10T09:30:00Z"))
	if phase != "open" || !at.Equal(utc(t, "2026-04-16T03:00:00Z")) {
		t.Errorf("SET next open after Fri 10 Apr = %v %q, want Thu 16 Apr 03:00Z", at, phase)
	}
	// US: Thanksgiving closed, the Friday after open.
	if c.IsOpen(US, utc(t, "2026-11-26T16:00:00Z")) {
		t.Error("US is open on Thanksgiving")
	}
	if !c.IsOpen(US, utc(t, "2026-11-27T16:00:00Z")) {
		t.Error("US is closed the day after Thanksgiving")
	}
	// The holiday is read in the market's zone: Mon 12 Oct 23:00 UTC is already
	// Tue 13 Oct 06:00 in Bangkok, a holiday - though the session has not begun.
	if got := c.Sessions(SET, utc(t, "2026-10-12T23:00:00Z")); len(got) != 0 {
		t.Errorf("SET sessions for an instant that is 13 Oct in Bangkok = %v, want none", got)
	}
}

func TestNextBoundary_CryptoHasNone(t *testing.T) {
	c := shipped(t)
	if at, phase := c.NextBoundary(Crypto, utc(t, "2026-10-12T00:00:00Z")); !at.IsZero() || phase != "" {
		t.Errorf("crypto boundary = %v %q, want none", at, phase)
	}
	if at, phase := c.NextBoundary("bogus", utc(t, "2026-10-12T00:00:00Z")); !at.IsZero() || phase != "" {
		t.Errorf("unknown market boundary = %v %q, want none", at, phase)
	}
}

func TestSessions_MonthAndYearEnd(t *testing.T) {
	// d+i past the end of the month must normalise, not index a bogus day.
	c := New()
	at, phase := c.NextBoundary(US, utc(t, "2026-12-31T22:00:00Z"))
	if phase != "open" || !at.Equal(utc(t, "2027-01-01T14:30:00Z")) {
		t.Errorf("next boundary over the new year (no holiday list) = %v %q, want 2027-01-01 14:30Z open", at, phase)
	}
}

func TestYearNotCovered_WarnsOnceAndIsOpen(t *testing.T) {
	buf := captureLogs(t)
	c := shipped(t)
	// The shipped SET list covers 2026 only. Fri 1 Jan 2027 is not listed, so it is
	// an ordinary day, however the real exchange treats it.
	for i := 0; i < 5; i++ {
		if !c.IsOpen(SET, utc(t, "2027-01-04T04:00:00Z")) { // Mon 4 Jan 2027, 11:00 Bangkok
			t.Fatal("a day in a year the list does not cover must count as a normal day")
		}
		c.Sessions(SET, utc(t, "2027-01-01T04:00:00Z"))
	}
	logs := buf.String()
	if n := strings.Count(logs, "holiday list does not cover 2027"); n != 1 {
		t.Fatalf("the uncovered-year WARN was logged %d times, want exactly 1\n%s", n, logs)
	}
	if !strings.Contains(logs, "WRN") && !strings.Contains(strings.ToLower(logs), "warn") {
		t.Errorf("the message is not at WARN level:\n%s", logs)
	}
	// A different market has its own once.
	if c.IsOpen(US, utc(t, "2028-01-04T16:00:00Z")) != true {
		t.Error("US 2028 is not covered and must count as a normal day")
	}
	if n := strings.Count(buf.String(), "holiday list does not cover 2028"); n != 1 {
		t.Errorf("US 2028 WARN logged %d times, want 1", n)
	}
	// A market with no list at all says nothing here (the load already did).
	buf2 := captureLogs(t)
	empty := New()
	empty.IsOpen(SET, utc(t, "2030-01-04T04:00:00Z"))
	if strings.Contains(buf2.String(), "does not cover") {
		t.Errorf("a calendar with no holiday list warned about coverage:\n%s", buf2.String())
	}
}

func TestShippedFiles_AreConsistent(t *testing.T) {
	c := shipped(t)
	tables := *c.tables.Load()
	for _, m := range []Market{SET, US} {
		list := tables[m]
		for k := range list.days {
			y, mo, d := k/10000, time.Month(k/100%100), k%100
			wd := time.Date(y, mo, d, 12, 0, 0, 0, time.UTC).Weekday()
			if wd == time.Saturday || wd == time.Sunday {
				t.Errorf("%s lists %04d-%02d-%02d, a %s: a holiday on a weekend is a typo or a missing observance", m, y, mo, d, wd)
			}
		}
	}
	if n := len(tables[SET].days); n != 19 {
		t.Errorf("SET 2026 list has %d days, want the 19 the SET calendar shows", n)
	}
	if n := len(tables[US].days); n != 20 {
		t.Errorf("US 2026-2027 list has %d days, want 10 per year", n)
	}
	if _, ok := tables[SET].years[2027]; ok {
		t.Error("SET claims to cover 2027, whose calendar was not published when the file was written")
	}
}
