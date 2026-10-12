package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/agent"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

func TestHeartbeatDeadline(t *testing.T) {
	for name, tc := range map[string]struct{ interval, want time.Duration }{
		"30 minutes -> the 5 minute cap":    {30 * time.Minute, 5 * time.Minute},
		"2 hours -> the 5 minute cap":       {2 * time.Hour, 5 * time.Minute},
		"10 minutes -> half the interval":   {10 * time.Minute, 5 * time.Minute},
		"5 minutes -> half the interval":    {5 * time.Minute, 150 * time.Second},
		"tiny interval -> the 60s floor":    {10 * time.Second, time.Minute},
		"zero interval -> the 60s floor":    {0, time.Minute},
		"exactly two minutes -> the floor":  {2 * time.Minute, time.Minute},
		"three minutes -> 90s, above floor": {3 * time.Minute, 90 * time.Second},
	} {
		if got := heartbeatDeadline(tc.interval); got != tc.want {
			t.Errorf("%s: heartbeatDeadline(%v) = %v, want %v", name, tc.interval, got, tc.want)
		}
	}
}

// The handler passes the deadline of the interval it was built with to the turn.
func TestHeartbeatGoverned_TurnGetsTheIntervalDeadline(t *testing.T) {
	for interval, want := range map[time.Duration]time.Duration{
		30 * time.Minute: 5 * time.Minute,
		5 * time.Minute:  150 * time.Second,
		time.Second:      time.Minute,
	} {
		r := &fakeRunner{res: agent.ProactiveTurnResult{
			Decisions: []proactive.Decision{{Kind: proactive.DecideSilent}},
		}}
		governedHeartbeatHandler(r, &fakeSubmitter{}, interval, proactive.ModelTierPrimary)(context.Background(), "p", "", "")
		reqs := r.requests()
		if len(reqs) != 1 || reqs[0].Deadline != want {
			t.Errorf("interval %v: turn deadline = %+v, want %v", interval, reqs, want)
		}
	}
}
