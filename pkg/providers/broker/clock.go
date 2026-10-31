package broker

import (
	"sync/atomic"
	"time"
)

var processClock atomic.Pointer[func() time.Time]

// SetClock makes the package's caches (tickers, OHLCV, USD rates, earn history) and
// the paper-mode simulation read now instead of the wall clock. It is process-wide:
// the gateway calls it once at start-up (a locked gateway passes its sim clock).
// A nil now restores time.Now.
func SetClock(now func() time.Time) {
	if now == nil {
		processClock.Store(nil)
		return
	}
	processClock.Store(&now)
}

// processNow is the package's time source: the SetClock hook, else time.Now.
func processNow() time.Time {
	if f := processClock.Load(); f != nil {
		return (*f)()
	}
	//simclock:wall the default when no clock is injected
	return time.Now()
}
