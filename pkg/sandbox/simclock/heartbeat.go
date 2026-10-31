package simclock

import "time"

// HeartbeatClock is the clock shape pkg/heartbeat takes. Its NewTimer returns the same
// unnamed interface as heartbeat.Timer (an alias of it), so a HeartbeatClock satisfies
// heartbeat.Clock without simclock importing heartbeat.
type HeartbeatClock interface {
	Now() time.Time
	NewTimer(d time.Duration) interface {
		C() <-chan time.Time
		Stop() bool
	}
}

// ForHeartbeat adapts c to the heartbeat service's clock.
func ForHeartbeat(c Clock) HeartbeatClock { return heartbeatClock{c} }

type heartbeatClock struct{ c Clock }

func (h heartbeatClock) Now() time.Time { return h.c.Now() }

func (h heartbeatClock) NewTimer(d time.Duration) interface {
	C() <-chan time.Time
	Stop() bool
} {
	return h.c.NewTimer(d)
}
