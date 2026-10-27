package tools

import "time"

// SetClock makes the DCA path (row timestamps, expiry, the period cap) and the approval
// order backend read now instead of the wall clock. It is process-wide, set once by the
// gateway at start-up before any turn runs (a locked gateway passes its sim clock);
// a nil now restores time.Now.
func SetClock(now func() time.Time) {
	if now == nil {
		now = time.Now
	}
	dcaNow = now
}
