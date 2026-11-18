package deltaneutral

import "testing"

// The card retries of a round are strictly increasing, start after the first try and all fall
// before the next round's window opens, so a round never reaches into the next one.
func TestRecoverCardRetrySchedule(t *testing.T) {
	var prev int64
	for i, d := range recoverCardRetryAt {
		if d <= 0 || int64(d) <= prev {
			t.Errorf("retry %d at %s is not after the one before", i, d)
		}
		if d >= RecoverOfferEvery {
			t.Errorf("retry %d at %s is outside the %s window", i, d, RecoverOfferEvery)
		}
		prev = int64(d)
	}
}
