package triggers

import (
	"context"
	"encoding/json"
	"fmt"
	"math"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/ta"
	"github.com/cryptoquantumwave/khunquant/pkg/worldmodel"
)

// Indicators an indicator_cross can watch.
const (
	IndRSI        = "rsi"
	IndSMA        = "sma"
	IndEMA        = "ema"
	IndMACDLine   = "macd_line"
	IndMACDSignal = "macd_signal"
	IndMACDHist   = "macd_hist"
)

// The MACD every variant uses, as the legacy alert did.
const (
	macdFast, macdSlow, macdSignal = 12, 26, 9
	// macdBars is the history a signal line or a histogram needs: slow + signal - 1.
	macdBars = macdSlow + macdSignal - 1
	// minLookback and maxLookback bound the closes asked of the market source: the
	// legacy alert fetched max(3 x period, 100) bars, at most 500.
	minLookback = 100
	maxLookback = 500
)

// IndicatorCrossParams are the parameters of an indicator_cross spec: "tell me when
// INDICATOR(period) of SYMBOL on TIMEFRAME goes above/below THRESHOLD", judged on
// closed bars only.
//
// Period applies to rsi, sma and ema and is ignored by the macd variants (12/26/9).
// Hysteresis is in the indicator's own units (default 1 for rsi, 0.3% of the
// threshold for sma and ema, 0 for macd: a MACD's scale depends on the asset, so no
// default can fit them all).
type IndicatorCrossParams struct {
	Provider      string   `json:"provider"`
	Account       string   `json:"account,omitempty"`
	Symbol        string   `json:"symbol"`
	Timeframe     string   `json:"timeframe"`
	Indicator     string   `json:"indicator"`
	Period        int      `json:"period,omitempty"`
	Direction     string   `json:"direction"`
	Threshold     float64  `json:"threshold"`
	Hysteresis    *float64 `json:"hysteresis,omitempty"`
	Recurring     bool     `json:"recurring"`
	CooldownMin   int      `json:"cooldown_minutes,omitempty"`
	FireIfAlready *bool    `json:"fire_if_already,omitempty"`
	Message       string   `json:"message,omitempty"`
}

func isMACD(ind string) bool { return ind == IndMACDLine || ind == IndMACDSignal || ind == IndMACDHist }

// Validate checks the parameters.
func (p IndicatorCrossParams) Validate() error {
	switch p.Indicator {
	case IndRSI, IndSMA, IndEMA:
		if p.Period < 2 || p.Period > maxAlertPeriod {
			return fmt.Errorf("period must be from 2 to %d for %s", maxAlertPeriod, p.Indicator)
		}
	case IndMACDLine, IndMACDSignal, IndMACDHist:
		if p.Period != 0 {
			return fmt.Errorf("%s has a fixed 12/26/9 period; leave period out", p.Indicator)
		}
	default:
		return fmt.Errorf("indicator must be rsi, sma, ema, macd_line, macd_signal or macd_hist")
	}
	switch {
	case !validAlertToken(p.Provider, false):
		return fmt.Errorf("provider must be 1-%d letters, digits, '_', '-', '.' or '@'", maxAlertTokenLen)
	case !validAlertToken(p.Account, true):
		return fmt.Errorf("account must be at most %d letters, digits, '_', '-', '.' or '@'", maxAlertTokenLen)
	case !validAlertSymbol(p.Symbol):
		return fmt.Errorf("symbol must be 1-%d letters, digits or '/', ':', '_', '-', '.'", maxAlertTokenLen)
	case !validTimeframe(p.Timeframe):
		return fmt.Errorf("timeframe must look like 1m, 15m, 1h, 4h, 1d or 1w")
	case p.Direction != DirAbove && p.Direction != DirBelow:
		return fmt.Errorf("direction must be %q or %q", DirAbove, DirBelow)
	case !finite(p.Threshold):
		return fmt.Errorf("threshold must be a finite number")
	case p.Hysteresis != nil && (!finite(*p.Hysteresis) || *p.Hysteresis < 0):
		return fmt.Errorf("hysteresis must be a number from 0 up")
	case p.CooldownMin < 0 || p.CooldownMin > maxCooldownMin:
		return fmt.Errorf("cooldown_minutes must be from 1 to %d", maxCooldownMin)
	}
	return nil
}

func (p IndicatorCrossParams) hysteresis() float64 {
	switch {
	case p.Hysteresis != nil:
		return *p.Hysteresis
	case p.Indicator == IndRSI:
		return defaultRSIHysteresis
	case p.Indicator == IndSMA || p.Indicator == IndEMA:
		return math.Abs(p.Threshold) * defaultBandHysteresis / 100
	}
	return 0
}

func (p IndicatorCrossParams) fireIfAlready() bool { return p.FireIfAlready == nil || *p.FireIfAlready }

// JSON encodes the parameters for a Spec.
func (p IndicatorCrossParams) JSON() json.RawMessage {
	b, _ := json.Marshal(p) // plain fields: cannot fail
	return b
}

// minBars is how many closes the indicator needs to be computed at all.
func (p IndicatorCrossParams) minBars() int {
	switch p.Indicator {
	case IndRSI:
		return p.Period + 1
	case IndSMA, IndEMA:
		return p.Period
	}
	// All three MACD variants come from one ta.MACD call, which returns nothing
	// until the signal line exists: slow + signal - 1 closes.
	return macdBars
}

// Lookback is how many closes to ask the market source for: what the legacy alert
// fetched, max(3 x period, 100) up to 500, and never less than a MACD needs.
func (p IndicatorCrossParams) Lookback() int {
	n := minLookback
	if !isMACD(p.Indicator) {
		n = max(n, 3*p.Period)
	}
	return min(max(n, p.minBars()), maxLookback)
}

// label is the indicator as it reads in a message: "RSI(14) 1h", "MACD hist 4h".
func (p IndicatorCrossParams) label() string {
	switch p.Indicator {
	case IndRSI:
		return fmt.Sprintf("RSI(%d) %s", p.Period, p.Timeframe)
	case IndSMA:
		return fmt.Sprintf("SMA(%d) %s", p.Period, p.Timeframe)
	case IndEMA:
		return fmt.Sprintf("EMA(%d) %s", p.Period, p.Timeframe)
	case IndMACDLine:
		return fmt.Sprintf("MACD line %s", p.Timeframe)
	case IndMACDSignal:
		return fmt.Sprintf("MACD signal %s", p.Timeframe)
	}
	return fmt.Sprintf("MACD hist %s", p.Timeframe)
}

// IndicatorValue computes the latest value of the indicator on closes with the
// existing pkg/ta functions (RSI, SMA, EMA, MACD). ok is false when closes are too
// few for it or the result is not finite.
func (p IndicatorCrossParams) IndicatorValue(closes []float64) (v float64, ok bool) {
	if len(closes) < p.minBars() {
		return 0, false
	}
	var series []float64
	switch p.Indicator {
	case IndRSI:
		series = ta.RSI(closes, p.Period)
	case IndSMA:
		series = ta.SMA(closes, p.Period)
	case IndEMA:
		series = ta.EMA(closes, p.Period)
	default:
		m := ta.MACD(closes, macdFast, macdSlow, macdSignal)
		if m == nil {
			return 0, false
		}
		switch p.Indicator {
		case IndMACDLine:
			series = m.MACD
		case IndMACDSignal:
			series = m.Signal
		default:
			series = m.Histogram
		}
	}
	if len(series) == 0 || !finite(series[len(series)-1]) {
		return 0, false
	}
	return series[len(series)-1], true
}

// IndicatorCrossFactory builds indicator_cross triggers.
func IndicatorCrossFactory(cfg CrossConfig) Factory {
	cfg = cfg.withStats()
	return func(spec Spec) (Trigger, error) {
		var p IndicatorCrossParams
		if err := decodeStrict(spec.Params, &p); err != nil {
			return nil, err
		}
		if err := p.Validate(); err != nil {
			return nil, err
		}
		return &indicatorCross{
			id: spec.Kind + ":" + spec.ID, p: p, cfg: cfg, owner: OwnerAlertSpec(spec.Kind, spec.Origin), intent: intentOf(spec),
			line: newLine(p.Direction, p.Threshold, p.hysteresis(), cooldownOrDefault(p.CooldownMin), p.fireIfAlready(), p.Recurring),
		}, nil
	}
}

type indicatorCross struct {
	id   string
	p    IndicatorCrossParams
	cfg  CrossConfig
	line line
	// owner: the spec is an alert the owner set (OwnerAlertSpec); its candidates are
	// marked OwnerRequested.
	owner bool
	// intent: the intent the spec was compiled from, if any (see intentspec.go).
	intent intentRef
}

func (t *indicatorCross) ID() string { return t.id }

func (t *indicatorCross) Kinds() []events.Kind {
	return []events.Kind{events.KindPerceptionMarketCandleClosed}
}

// Demands: the closed candles of the symbol at the timeframe, with the history the
// indicator needs in every event.
func (t *indicatorCross) Demands() []perception.Demand {
	return []perception.Demand{{
		Provider: t.p.Provider, Account: t.p.Account, Symbol: t.p.Symbol,
		Kind: perception.DemandOHLCV, Timeframe: t.p.Timeframe, Lookback: t.p.Lookback(),
	}}
}

// Evaluate judges the alert on a bar that has just closed, from the closes the
// event carried: the world model does not track candles, so the Diff of the event is
// empty and holds the event's payload (worldmodel.Diff.Payload). The forming bar never reaches it: the market source emits closed
// bars only, and a wick inside an open bar changes nothing here.
//
// An event for another series is ignored. For its own, the event must come from an
// exchange (trust), be no older than candleMaxAge by the view's clock, hold enough
// closes for the indicator (fewer are counted and judged on nothing) and yield a
// finite value. Anything else changes no state and fires nothing.
func (t *indicatorCross) Evaluate(_ context.Context, v worldmodel.View, d worldmodel.Diff, st *State) ([]proactive.Candidate, error) {
	cp, ok := d.Payload.(perception.CandlePayload)
	if !ok {
		return nil, nil // not a candle payload: not mine
	}
	c := cp.CandleClose()
	if c.Provider != t.p.Provider || c.Symbol != t.p.Symbol || c.Timeframe != t.p.Timeframe {
		return nil, nil
	}
	trust := d.Event.TrustLevel()
	if trust < proactive.TrustExchange {
		t.cfg.Stats.CandleUntrusted.Add(1)
		return nil, nil
	}
	if !v.Fresh(c.At, candleMaxAge) {
		t.cfg.Stats.CandleStale.Add(1)
		return nil, nil
	}
	if len(c.Closes) < t.p.minBars() {
		t.cfg.Stats.BarsTooFew.Add(1)
		return nil, nil
	}
	value, ok := t.p.IndicatorValue(c.Closes)
	if !ok {
		t.cfg.Stats.IndicatorInvalid.Add(1)
		return nil, nil
	}

	cs := loadCross(st)
	fired := t.line.step(&cs, value, v.Now())
	saveCross(st, cs, fired, t.p.Recurring)
	if !fired {
		return nil, nil
	}
	t.cfg.Stats.Fired.Add(1)
	return []proactive.Candidate{t.candidate(value, trust, v)}, nil
}

func (t *indicatorCross) candidate(value float64, trust proactive.Trust, v worldmodel.View) proactive.Candidate {
	thai := t.cfg.isThai()
	thr, now := fmtThreshold(t.p.Threshold), fmtNumber(value)
	msg := CleanAlertMessage(t.p.Message)
	ev := map[string]any{
		"value": value, "threshold": t.p.Threshold, "direction": t.p.Direction,
		"provider": t.p.Provider, "symbol": t.p.Symbol, "timeframe": t.p.Timeframe,
		"indicator": t.p.Indicator, "message": msg,
	}
	if t.p.Period > 0 {
		ev["period"] = t.p.Period
	}
	key := IndicatorCrossKey(t.p.Provider, t.p.Symbol, t.p.Timeframe, t.p.Indicator, t.p.Period, t.p.Direction, thr)
	c := crossCandidate(key, t.p.Symbol,
		crossTitle(thai, t.p.Symbol+" "+t.p.label(), t.p.Direction == DirAbove, thr, now),
		crossDetail(thai, t.p.Provider, thr, now), msg,
		ev, trust, t.p.CooldownMin, v.Now())
	c.OwnerRequested = t.owner
	c = t.intent.shape(KindIndicatorCross, t.p.Symbol, thai, c)
	return c
}
