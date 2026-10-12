package triggers

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// Kinds of the crossing triggers.
const (
	KindPriceCross     = "price_cross"
	KindIndicatorCross = "indicator_cross"
)

// Directions of a crossing.
const (
	DirAbove = "above"
	DirBelow = "below"
)

const (
	// PriceCrossPollInterval is how often a price crossing asks the market source
	// for its symbol: the minute the legacy cron alerts polled at.
	PriceCrossPollInterval = time.Minute
	// priceMaxAge is how old the price a crossing is judged on may be: three polls.
	// A price older than this (a market source that is backing off, a pipeline that
	// was stalled) is not acted on.
	priceMaxAge = 3 * PriceCrossPollInterval
	// candleMaxAge is how old a candle_closed event may be, by its own stamp, when
	// the pipeline gets to it.
	candleMaxAge = 5 * time.Minute

	// MaxAlertMessageRunes bounds the text of an alert's own message, wherever it is
	// stored, carried or rendered.
	MaxAlertMessageRunes = 300

	defaultHysteresisPct  = 0.3
	maxHysteresisPct      = 50
	defaultRSIHysteresis  = 1.0
	defaultBandHysteresis = 0.3 // percent of the threshold, for sma and ema
	defaultCooldownMin    = 60
	maxCooldownMin        = 7 * 24 * 60
	maxAlertTokenLen      = 64
	maxAlertPeriod        = 499
)

// CrossConfig is what the crossing kinds are given by the gateway.
type CrossConfig struct {
	// Language returns the language of the owner's messages: "th" for Thai, anything
	// else for English. It is read when a candidate is built, so it must be cheap
	// and must not do I/O; the gateway reads the owner's profile once and returns
	// what it read. Nil means English.
	Language func() string
	// Stats receives the counters of what the kinds skipped. Nil counts nothing.
	Stats *CrossStats
}

// CrossStats counts what the crossing kinds did not act on, so that a silence has
// a number behind it. Counts only: no symbol, no price.
type CrossStats struct {
	// PriceStale counts ticks of the alert's own symbol that were older than the
	// price may be; PriceUntrusted those whose trust was below the exchange's;
	// PriceInvalid those that were not a positive finite number.
	PriceStale, PriceUntrusted, PriceInvalid atomic.Uint64
	// BarsTooFew counts closed bars on which an indicator could not be computed for
	// want of history; CandleStale and CandleUntrusted those older or less trusted
	// than allowed; IndicatorInvalid those whose result was not finite.
	BarsTooFew, CandleStale, CandleUntrusted, IndicatorInvalid atomic.Uint64
	// Fired counts candidates built, by the kinds together.
	Fired atomic.Uint64
}

// withStats gives a config that has no counters one of its own, so that the kinds
// never test for nil on the pipeline.
func (c CrossConfig) withStats() CrossConfig {
	if c.Stats == nil {
		c.Stats = &CrossStats{}
	}
	return c
}

// crossState is the part of a crossing the registry keeps between events.
//
// The machine, written down (value is a price, or an indicator's value on a closed
// bar; the target side is value >= threshold for "above" and <= for "below"; the
// retreat line is the threshold moved hysteresis away from the target side):
//
//	unknown  --first observation on the far side-------------> far, armed
//	unknown  --first observation on the target side----------> target, FIRE if
//	                                                            fire_if_already, else disarmed
//	any      --value at or past the retreat line while disarmed--> retreated (remembered)
//	disarmed --retreated AND cooldown elapsed since the last fire--> armed
//	armed    --value on the target side-------------------------> FIRE, disarmed
//	FIRE     records last_fired_ms; a one-shot alert becomes done and asks to be
//	         switched off (State.TurnOff), a recurring one waits for the re-arm.
//
// Re-arming needs BOTH the retreat and the cooldown, and the retreat is
// remembered: a price that went back past the line during the cooldown and is on
// the target side again when the cooldown ends fires then, once. A threshold or
// direction edit changes the spec's parameters, so the registry clears the state
// and the next fresh observation is a first one.
type crossState struct {
	Side        string `json:"side,omitempty"` // "above" | "below"; empty is unknown
	Armed       bool   `json:"armed,omitempty"`
	LastFiredMS int64  `json:"last_fired_ms,omitempty"`
	Retreated   bool   `json:"retreated,omitempty"`
	Done        bool   `json:"done,omitempty"`
}

// line is the geometry of one crossing.
type line struct {
	above         bool
	threshold     float64
	retreat       float64
	cooldown      time.Duration
	fireIfAlready bool
	oneShot       bool
}

func newLine(direction string, threshold, retreatDistance float64, cooldownMin int, fireIfAlready, recurring bool) line {
	l := line{
		above: direction == DirAbove, threshold: threshold,
		cooldown: time.Duration(cooldownMin) * time.Minute, fireIfAlready: fireIfAlready, oneShot: !recurring,
	}
	if l.above {
		l.retreat = threshold - retreatDistance
	} else {
		l.retreat = threshold + retreatDistance
	}
	return l
}

func (l line) onTarget(v float64) bool {
	if l.above {
		return v >= l.threshold
	}
	return v <= l.threshold
}

func (l line) inRetreat(v float64) bool {
	if l.above {
		return v <= l.retreat
	}
	return v >= l.retreat
}

// step advances st by one fresh observation and reports whether the alert fires.
// v must be finite. It is a pure function of its arguments.
func (l line) step(st *crossState, v float64, now time.Time) (fire bool) {
	if st.Done {
		return false
	}
	target := l.onTarget(v)
	side := DirBelow
	if (target && l.above) || (!target && !l.above) {
		side = DirAbove
	}
	first := st.Side == ""
	st.Side = side
	switch {
	case first && target:
		fire = l.fireIfAlready
		st.Armed = false
	case first:
		st.Armed = true
	default:
		if !st.Armed {
			if l.inRetreat(v) {
				st.Retreated = true
			}
			if st.Retreated && l.cooledDown(st, now) {
				st.Armed, st.Retreated = true, false
			}
		}
		fire = st.Armed && target
	}
	if fire {
		st.Armed, st.Retreated = false, false
		st.LastFiredMS = now.UnixMilli()
		st.Done = l.oneShot
	}
	return fire
}

func (l line) cooledDown(st *crossState, now time.Time) bool {
	return st.LastFiredMS == 0 || now.Sub(time.UnixMilli(st.LastFiredMS)) >= l.cooldown
}

// load reads a trigger's state; a state that does not decode is a fresh one.
func loadCross(st *State) crossState {
	var cs crossState
	if len(st.Raw) > 0 {
		if err := json.Unmarshal(st.Raw, &cs); err != nil {
			return crossState{}
		}
	}
	return cs
}

// save stores cs into st when it differs from what st holds, and asks to be
// switched off when a one-shot alert is done.
func saveCross(st *State, cs crossState, fired, recurring bool) {
	raw, err := json.Marshal(cs)
	if err != nil {
		return
	}
	if string(raw) != string(st.Raw) {
		st.Raw, st.Dirty = raw, true
	}
	if fired && !recurring {
		st.TurnOff = true
	}
}

// ---- parameters ------------------------------------------------------------

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// validAlertToken is the shape of a provider or account name in an alert: printable
// ASCII without separators that could forge a dedup key.
func validAlertToken(s string, allowEmpty bool) bool {
	if s == "" {
		return allowEmpty
	}
	if len(s) > maxAlertTokenLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-', c == '.', c == '@':
		default:
			return false
		}
	}
	return true
}

// validAlertSymbol is a trading pair as exchanges write it ("BTC/USDT",
// "BTC/USDT:USDT", "BTC-USD"): letters, digits and '/', ':', '_', '-', '.'.
func validAlertSymbol(s string) bool {
	if s == "" || len(s) > maxAlertTokenLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '/', c == ':', c == '_', c == '-', c == '.':
		default:
			return false
		}
	}
	return true
}

// validTimeframe accepts the candle sizes exchanges offer: 1-3 digits and one of
// m, h, d, w ("1m", "15m", "4h", "1d", "1w").
func validTimeframe(tf string) bool {
	if len(tf) < 2 || len(tf) > 4 {
		return false
	}
	n, err := strconv.Atoi(tf[:len(tf)-1])
	if err != nil || n <= 0 || n > 999 {
		return false
	}
	switch tf[len(tf)-1] {
	case 'm', 'h', 'd', 'w':
		return true
	}
	return false
}

// CleanAlertMessage makes an alert's own message safe to carry: it is outside text
// (the model wrote it from what a user said), so it is reduced to one line of
// printable characters, with no double quote to break a quotation, and cut at
// MaxAlertMessageRunes. It is Evidence and the candidate's Quote, never part of a
// title, a subject or a dedup key.
func CleanAlertMessage(s string) string {
	s = strings.ToValidUTF8(s, "")
	var b strings.Builder
	b.Grow(min(len(s), 4*MaxAlertMessageRunes))
	for _, r := range s {
		switch {
		case unicode.IsSpace(r):
			b.WriteRune(' ')
		case r == '"':
			b.WriteRune('\'')
		case unicode.IsControl(r), unicode.Is(unicode.Cf, r), !unicode.IsPrint(r):
			// dropped
		default:
			b.WriteRune(r)
		}
	}
	out := strings.Join(strings.Fields(b.String()), " ")
	if utf8.RuneCountInString(out) > MaxAlertMessageRunes {
		out = strings.TrimRight(string([]rune(out)[:MaxAlertMessageRunes-1]), " ") + "…"
	}
	return out
}

func cooldownOrDefault(m int) int {
	if m <= 0 {
		return defaultCooldownMin
	}
	return m
}

// ---- rendering --------------------------------------------------------------

// isThai says whether the configured language asks for Thai.
func (c CrossConfig) isThai() bool {
	if c.Language == nil {
		return false
	}
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(c.Language())), "th")
}

// fmtNumber renders a price or an indicator value without an exponent and without
// trailing zeros: two decimals from 1,000 up, four from 1, eight below.
func fmtNumber(v float64) string {
	a := math.Abs(v)
	prec := 8
	switch {
	case a >= 1000:
		prec = 2
	case a >= 1:
		prec = 4
	}
	s := strconv.FormatFloat(v, 'f', prec, 64)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	if s == "" || s == "-" || s == "-0" {
		return "0"
	}
	return s
}

// fmtThreshold renders the user's own threshold exactly as given.
func fmtThreshold(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// title text, English and Thai. The only parts that come from outside are the
// symbol and the label, both of which passed validation (letters, digits and a few
// separators); the numbers are formatted here. The user's message is never in it.
func crossTitle(thai bool, subject string, above bool, threshold, now string) string {
	switch {
	case thai && above:
		return subject + " ขึ้นเหนือ " + threshold + " — ตอนนี้ " + now
	case thai:
		return subject + " ลงต่ำกว่า " + threshold + " — ตอนนี้ " + now
	case above:
		return subject + " crossed above " + threshold + " — now " + now
	}
	return subject + " fell below " + threshold + " — now " + now
}

// crossDetail is the evidence line of an alert: where it was seen and the numbers.
// The message the alert was set with is outside text and goes in the Quote, never
// here.
func crossDetail(thai bool, source, threshold, now string) string {
	if thai {
		return source + " · เส้น " + threshold + " · ตอนนี้ " + now
	}
	return source + " · line " + threshold + " · now " + now
}

func dedupWindow(cooldownMin int) time.Duration {
	// Below the cooldown, so that a re-fire the trigger allows is never swallowed by
	// attention as a repeat of the last one.
	w := time.Duration(cooldownOrDefault(cooldownMin)) * time.Minute / 2
	return max(w, time.Minute)
}

func crossCandidate(key, subject, title, detail, quote string, evidence map[string]any, trust proactive.Trust,
	cooldownMin int, at time.Time) proactive.Candidate {
	return proactive.Candidate{
		DedupKey:    key,
		Severity:    proactive.SeverityNotice,
		Salience:    0.5,
		Subject:     subject,
		Title:       title,
		Detail:      detail,
		Quote:       quote,
		Evidence:    evidence,
		Trust:       trust,
		Compose:     proactive.ComposeTemplate,
		DedupWindow: dedupWindow(cooldownMin),
		At:          at,
	}
}
