package simclock

import "time"

// Clock is the superset of the clock interfaces the gateway's components take
// (perception, journal, cognition, reflection, feedback): Real() and *Sim satisfy
// each of them with no adapter.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
	NewTimer(d time.Duration) Timer
}

// Timer is the time.Timer API behind an interface so a Sim can implement it.
type Timer interface {
	C() <-chan time.Time
	Stop() bool
	Reset(d time.Duration) bool
}

// Real is the wall clock: time.Now, time.After and time.NewTimer.
func Real() Clock { return realClock{} }

// NowFunc returns c.Now as a plain function, for the hooks shaped func() time.Time.
func NowFunc(c Clock) func() time.Time { return c.Now }

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }
func (realClock) NewTimer(d time.Duration) Timer         { return realTimer{time.NewTimer(d)} }

type realTimer struct{ t *time.Timer }

func (r realTimer) C() <-chan time.Time        { return r.t.C }
func (r realTimer) Stop() bool                 { return r.t.Stop() }
func (r realTimer) Reset(d time.Duration) bool { return r.t.Reset(d) }
