package lab

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/simclock"
)

// ClockGroup is the group "clock": get, advance and set_mode on the instance clock.
func (l *Lab) ClockGroup() Group {
	return Group{
		Name:        "clock",
		Description: "The instance's sim clock. In manual mode time moves only on advance; the components follow it.",
		Ops: []Op{
			{Name: "get", Description: "sim now, mode, scale, resolution and the armed timers",
				Args: json.RawMessage(`{}`), Run: l.clockGet},
			{Name: "advance", Description: "move manual time by `by` (a duration) or to `to` (RFC 3339), firing timers on the way. " +
				"done:false means the wall budget ran out: call again",
				Args: json.RawMessage(`{"by":{"type":"string","description":"duration, e.g. 45m"},"to":{"type":"string","description":"RFC 3339"},` +
					`"wall_budget":{"type":"string","description":"wall time to spend, default 20s, max 25s"}}`),
				Run: l.clockAdvance},
			{Name: "set_mode", Description: "switch between manual, scaled (with scale) and realtime; resolution is the manual-mode quantum (0 = exact)",
				Args: json.RawMessage(`{"mode":{"type":"string","enum":["manual","scaled","realtime"]},` +
					`"scale":{"type":"number","description":"sim seconds per wall second (scaled only)"},` +
					`"resolution":{"type":"string","description":"duration, e.g. 1m; 0 = exact"}}`),
				Required: []string{"mode"}, Run: l.clockSetMode},
		},
	}
}

type clockState struct {
	Now         time.Time  `json:"now"`
	Mode        string     `json:"mode"`
	Scale       float64    `json:"scale,omitempty"`
	Resolution  string     `json:"resolution"`
	Pending     int        `json:"pending_timers"`
	NextTimerAt *time.Time `json:"next_timer_at,omitempty"`
}

func (l *Lab) clockState() clockState {
	st := l.Clock.State()
	cs := clockState{Now: st.Now, Mode: string(st.Mode), Scale: st.Scale, Resolution: st.Resolution.String(), Pending: l.Clock.Pending()}
	if at, ok := l.Clock.NextAt(); ok {
		cs.NextTimerAt = &at
	}
	return cs
}

func (l *Lab) clockReady() error {
	if l.Clock == nil {
		return Errorf(CodeUnavailable, "this lab has no sim clock")
	}
	return nil
}

func (l *Lab) clockGet(context.Context, json.RawMessage) (any, error) {
	if err := l.clockReady(); err != nil {
		return nil, err
	}
	return l.clockState(), nil
}

func strictArgs(args json.RawMessage, into any) error {
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return Errorf(CodeBadArgs, "%v", err)
	}
	return nil
}

func parseDur(field, v string) (time.Duration, error) {
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, Errorf(CodeBadArgs, "%s %q is not a duration", field, v)
	}
	return d, nil
}

func (l *Lab) clockAdvance(ctx context.Context, args json.RawMessage) (any, error) {
	if err := l.clockReady(); err != nil {
		return nil, err
	}
	var a struct {
		Op         string `json:"op"`
		By         string `json:"by"`
		To         string `json:"to"`
		WallBudget string `json:"wall_budget"`
	}
	if err := strictArgs(args, &a); err != nil {
		return nil, err
	}
	if (a.By == "") == (a.To == "") {
		return nil, Errorf(CodeBadArgs, "give exactly one of by and to")
	}
	budget := DefaultWallBudget
	if a.WallBudget != "" {
		d, err := parseDur("wall_budget", a.WallBudget)
		if err != nil {
			return nil, err
		}
		if d <= 0 {
			return nil, Errorf(CodeBadArgs, "wall_budget must be > 0")
		}
		budget = min(d, MaxWallBudget)
	}
	var target time.Time
	if a.By != "" {
		d, err := parseDur("by", a.By)
		if err != nil {
			return nil, err
		}
		target = l.Clock.Now().Add(d)
	} else {
		t, err := time.Parse(time.RFC3339, a.To)
		if err != nil {
			return nil, Errorf(CodeBadArgs, "to %q is not RFC 3339", a.To)
		}
		target = t
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	sum, err := l.Clock.Advance(ctx, target, l.Settle)
	if err != nil {
		return nil, fmt.Errorf("advance: %w", err)
	}
	return struct {
		simclock.AdvanceSummary
		Wall   string `json:"wall"`
		Settle string `json:"settle"`
	}{sum, sum.Wall.Round(time.Millisecond).String(), "engine-only"}, nil
}

func (l *Lab) clockSetMode(_ context.Context, args json.RawMessage) (any, error) {
	if err := l.clockReady(); err != nil {
		return nil, err
	}
	var a struct {
		Op         string   `json:"op"`
		Mode       string   `json:"mode"`
		Scale      *float64 `json:"scale"`
		Resolution *string  `json:"resolution"`
	}
	if err := strictArgs(args, &a); err != nil {
		return nil, err
	}
	m := simclock.Mode(a.Mode)
	if m != simclock.ModeManual && m != simclock.ModeScaled && m != simclock.ModeRealtime {
		return nil, Errorf(CodeBadArgs, "mode %q is not manual, scaled or realtime", a.Mode)
	}
	if a.Scale != nil && m != simclock.ModeScaled {
		return nil, Errorf(CodeBadArgs, "scale applies to scaled mode only")
	}
	if a.Resolution != nil {
		d, err := parseDur("resolution", *a.Resolution)
		if err != nil {
			return nil, err
		}
		if err := l.Clock.SetResolution(d); err != nil {
			return nil, Errorf(CodeBadArgs, "%v", err)
		}
	}
	if a.Scale != nil {
		if err := l.Clock.SetScale(*a.Scale); err != nil {
			return nil, Errorf(CodeBadArgs, "%v", err)
		}
	}
	if err := l.Clock.SetMode(m); err != nil {
		return nil, fmt.Errorf("set_mode: %w", err)
	}
	saveClock(l.ClockPath(), l.Clock)
	return l.clockState(), nil
}
