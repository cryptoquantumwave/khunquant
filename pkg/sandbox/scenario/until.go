package scenario

import (
	"context"
	"errors"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labclient"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labjournal"
)

// Defaults and limits of RunUntil.
const (
	DefaultTimeStep  = time.Hour
	DefaultUntilStep = 10 * time.Minute
	DefaultMaxSim    = 24 * time.Hour
	DefaultMaxWall   = 5 * time.Minute
	MaxMaxWall       = 30 * time.Minute
	callWall         = 20 * time.Second // wall budget of one advance call (the instance caps it at 25 s)
)

// UntilOptions say where RunUntil stops. Target is an absolute sim time (zero: only
// Check ends the run); Check is evaluated after every chunk and returns the entry that
// matched. Step, MaxSim and MaxWall default as above.
type UntilOptions struct {
	Target  time.Time
	Check   func(ctx context.Context) (matched bool, entry *labjournal.Entry, err error)
	Step    time.Duration
	MaxSim  time.Duration
	MaxWall time.Duration
}

// UntilResult is how RunUntil ended. Stopped is "" when the target was reached or the
// check matched, else "max_sim" or "max_wall".
type UntilResult struct {
	Reached time.Time         `json:"reached"`
	Matched bool              `json:"matched"`
	Entry   *labjournal.Entry `json:"entry,omitempty"`
	Chunks  int               `json:"chunks"`
	Wall    string            `json:"wall"`
	Stopped string            `json:"stopped,omitempty"`
}

// RunUntil advances the instance's clock in short chunks (each settled, each a loop of
// calls of at most 20 s of wall time) until the target, a match, or a limit. Both the
// scenario step run_until and the sandbox_run_until tool run this one loop.
func RunUntil(ctx context.Context, c *labclient.Client, o UntilOptions) (UntilResult, error) {
	started := time.Now()
	if o.Step <= 0 {
		o.Step = DefaultTimeStep
		if o.Check != nil && o.Target.IsZero() {
			o.Step = DefaultUntilStep
		}
	}
	if o.MaxSim <= 0 {
		o.MaxSim = DefaultMaxSim
	}
	o.MaxWall = min(max(o.MaxWall, 0), MaxMaxWall)
	if o.MaxWall == 0 {
		o.MaxWall = DefaultMaxWall
	}
	first, err := clockNow(ctx, c)
	if err != nil {
		return UntilResult{}, err
	}
	res := UntilResult{Reached: first}
	finish := func() (UntilResult, error) {
		res.Wall = time.Since(started).Round(time.Millisecond).String()
		return res, nil
	}
	for {
		if o.Check != nil {
			matched, entry, err := o.Check(ctx)
			if err != nil {
				return res, err
			}
			if matched {
				res.Matched, res.Entry = true, entry
				return finish()
			}
		}
		left := first.Add(o.MaxSim).Sub(res.Reached)
		switch {
		case !o.Target.IsZero() && !res.Reached.Before(o.Target):
			return finish()
		case left <= 0:
			res.Stopped = "max_sim"
			return finish()
		case time.Since(started) >= o.MaxWall:
			res.Stopped = "max_wall"
			return finish()
		case ctx.Err() != nil:
			return res, ctx.Err()
		}
		chunk := min(o.Step, left)
		if !o.Target.IsZero() {
			chunk = min(chunk, o.Target.Sub(res.Reached))
		}
		switch err := advanceTo(ctx, c, res.Reached.Add(chunk), callWall, started.Add(o.MaxWall)); {
		case errors.Is(err, errMaxWall):
			res.Stopped = "max_wall"
			if res.Reached, err = clockNow(ctx, c); err != nil {
				return res, err
			}
			return finish()
		case err != nil:
			return res, err
		}
		res.Chunks++
		if res.Reached, err = clockNow(ctx, c); err != nil {
			return res, err
		}
	}
}
