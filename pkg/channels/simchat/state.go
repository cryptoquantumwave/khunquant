package simchat

import (
	"fmt"
	"math/rand"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/channels"
)

// Channel states. Each one answers a send with an error the manager already knows how
// to retry (or not): see (*channels.Manager).sendWithRetry.
const (
	stateUp          = "up"
	stateDown        = "down"
	stateSlow        = "slow"
	stateRateLimited = "ratelimited"
	stateFlaky       = "flaky"
)

// chanState is the failure mode of the channel. It is guarded by the World's mutex.
type chanState struct {
	kind      string
	delay     time.Duration // slow: wall time a send waits
	perMinute int           // ratelimited: sends allowed per sim minute
	p         float64       // flaky: probability of a temporary failure
	rng       *rand.Rand    // flaky: seeded, so a run repeats
	err       error         // down: channels.ErrTemporary (retried) or channels.ErrSendFailed (not)
	sent      []time.Time   // ratelimited: sim times of the sends in the last minute
}

// gate decides one send at sim time now: how long to wait (wall) before it proceeds,
// or the error it fails with.
func (s *chanState) gate(now time.Time) (wait time.Duration, err error) {
	switch s.kind {
	case stateDown:
		return 0, fmt.Errorf("simchat is down: %w", s.err)
	case stateSlow:
		return s.delay, nil
	case stateRateLimited:
		keep := s.sent[:0]
		for _, t := range s.sent {
			if now.Sub(t) < time.Minute {
				keep = append(keep, t)
			}
		}
		if s.sent = keep; len(s.sent) >= s.perMinute {
			return 0, fmt.Errorf("simchat is rate limited (%d per minute): %w", s.perMinute, channels.ErrRateLimit)
		}
		s.sent = append(s.sent, now)
	case stateFlaky:
		if s.rng.Float64() < s.p {
			return 0, fmt.Errorf("simchat is flaky: %w", channels.ErrTemporary)
		}
	}
	return 0, nil
}
