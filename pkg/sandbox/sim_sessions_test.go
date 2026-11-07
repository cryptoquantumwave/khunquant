package sandbox

import (
	"strings"
	"testing"
	"time"
)

func bkk(d, h, m int) time.Time { return time.Date(2026, 10, d, h, m, 0, 0, bangkok) }

// The SET day as a function of the clock alone. 2026-10-19 is a Monday, so the 24th and
// 25th are the weekend. A time in another zone is read in Bangkok.
func TestSetDay_AuctionsSessionsAndDayEnd(t *testing.T) {
	d, err := VenueOptions{}.day()
	if err != nil {
		t.Fatal(err)
	}
	next := func(at time.Time) string {
		a, ok := d.nextAuction(at)
		if !ok {
			return "none"
		}
		return a.In(bangkok).Format("Mon 15:04")
	}
	for _, tc := range []struct {
		at          time.Time
		auction     string
		inSession   bool
		dayEnd      string
		nextOpening string
	}{
		{bkk(20, 8, 30), "Tue 10:00", false, "Tue 16:40", "Tue 10:00"},
		{bkk(20, 10, 0), "Tue 14:30", true, "Tue 16:40", "Tue 14:30"}, // at the instant the auction has run
		{bkk(20, 9, 59), "Tue 10:00", false, "Tue 16:40", "Tue 10:00"},
		{bkk(20, 12, 29), "Tue 14:30", true, "Tue 16:40", "Tue 14:30"},
		{bkk(20, 12, 30), "Tue 14:30", false, "Tue 16:40", "Tue 14:30"},
		{bkk(20, 14, 30), "none", true, "Tue 16:40", "Wed 10:00"},
		{bkk(20, 16, 29), "none", true, "Tue 16:40", "Wed 10:00"},
		{bkk(20, 16, 30), "none", false, "Tue 16:40", "Wed 10:00"},
		{bkk(20, 16, 40), "none", false, "Wed 16:40", "Wed 10:00"},
		{bkk(23, 17, 0), "none", false, "Mon 16:40", "Mon 10:00"},      // Friday evening: next trading day is Monday
		{bkk(24, 11, 0), "Mon 10:00", false, "Mon 16:40", "Mon 10:00"}, // Saturday
		{bkk(25, 11, 0), "Mon 10:00", false, "Mon 16:40", "Mon 10:00"}, // Sunday
	} {
		if got := next(tc.at); got != tc.auction {
			t.Errorf("%v: next auction %s, want %s", tc.at, got, tc.auction)
		}
		if got := d.inSession(tc.at); got != tc.inSession {
			t.Errorf("%v: in session %v, want %v", tc.at, got, tc.inSession)
		}
		if got := dayEnd(tc.at).In(bangkok).Format("Mon 15:04"); got != tc.dayEnd {
			t.Errorf("%v: day ends %s, want %s", tc.at, got, tc.dayEnd)
		}
		if got := d.nextSession(tc.at).In(bangkok).Format("Mon 15:04"); got != tc.nextOpening {
			t.Errorf("%v: next session %s, want %s", tc.at, got, tc.nextOpening)
		}
	}
	// A time in another zone is read in Bangkok: 01:00 UTC is 08:00 there, before the open;
	// 03:30 UTC Tuesday is 10:30 Bangkok, inside the morning session.
	if u := time.Date(2026, 10, 20, 1, 0, 0, 0, time.UTC); d.inSession(u) || next(u) != "Tue 10:00" {
		t.Errorf("01:00 UTC (08:00 Bangkok): in session %v, next auction %s", d.inSession(u), next(u))
	}
	if !d.inSession(time.Date(2026, 10, 20, 3, 30, 0, 0, time.UTC)) {
		t.Error("03:30 UTC (10:30 Bangkok) is not in session")
	}
}

// The auction times are options: HH:MM, each inside its session window, and a bad one is
// refused with a message naming it.
func TestVenueOptions_AuctionTimes(t *testing.T) {
	o, err := ParseVenueOptions("settrade", map[string]any{"auction_open": "09:55", "auction_afternoon": "14:00"})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := o.day()
	if a, ok := d.nextAuction(bkk(20, 8, 0)); !ok || a != bkk(20, 9, 55) {
		t.Errorf("next auction = %v, want 09:55", a)
	}
	if !d.inSession(bkk(20, 14, 0)) || d.inSession(bkk(20, 13, 59)) {
		t.Error("the afternoon session does not open at the configured 14:00")
	}
	for name, tc := range map[string]struct {
		opts map[string]any
		want string
	}{
		"unknown key":        {map[string]any{"list_seed": 1}, "list_seed"},
		"number, not string": {map[string]any{"auction_open": 10}, "HH:MM"},
		"not a time":         {map[string]any{"auction_open": "10h"}, "HH:MM"},
		"with seconds":       {map[string]any{"auction_open": "10:00:00"}, "HH:MM"},
		"after the morning":  {map[string]any{"auction_open": "13:00"}, "outside"},
		"before the market":  {map[string]any{"auction_open": "08:00"}, "outside"},
		"afternoon too late": {map[string]any{"auction_afternoon": "16:30"}, "outside"},
	} {
		if _, err := ParseVenueOptions("settrade", tc.opts); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want an error naming %q", name, err, tc.want)
		}
	}
	if _, err := ParseVenueOptions("okx", map[string]any{"auction_open": "10:00"}); err == nil {
		t.Error("okx accepted a settrade option")
	}
	if err := NewStatefulSimulator(NewStateManager()).SetVenueOptions("okx", VenueOptions{}); err == nil {
		t.Error("a venue with no options accepted some")
	}
}
