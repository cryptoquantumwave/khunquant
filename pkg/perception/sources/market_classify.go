package sources

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	ccxt "github.com/ccxt/ccxt/go/v4"
)

// ErrRateLimited marks an error as an exchange saying "slow down" (or "you are
// banned"). A reader may wrap it around anything its own layer recognises, such as
// the broker package's rate-limit error; the source also recognises the shapes
// below without it.
var ErrRateLimited = errors.New("market: rate limited by the exchange")

var (
	// statusRE finds an HTTP 418 or 429 named as a status.
	statusRE = regexp.MustCompile(`(?i)(?:\bhttp\b|\bstatus\b|\bcode\b|\bresponse\b)[^0-9]{0,12}\b(?:418|429)\b|\b(?:418|429)\b[ :-]*(?:too many|i'?m a teapot|rate|banned)`)
	// notSupportedRE finds the "this exchange cannot do that" family.
	notSupportedRE = regexp.MustCompile(`(?i)not (?:supported|implemented|available)|unsupported|does not support`)
)

// rateLimitWords are the phrases exchanges use when they push back. They are
// matched in lower case.
var rateLimitWords = []string{
	"too many requests", "rate limit", "ratelimit", "rate-limit", "request frequency",
	"banned", "ip has been auto-banned", "ddos", "too frequent", "-1003", "\"50011\"", "code=50011",
}

// statusCoder is the shape of an HTTP error that knows its status.
type statusCoder interface{ StatusCode() int }

// isRateLimitErr reports whether err is the exchange pushing back: HTTP 418 or
// 429, a ccxt RateLimitExceeded or DDoSProtection, the package's own sentinel, or
// text that says so. A false positive costs a few minutes of back-off; a false
// negative retries into a ban, so the match is generous.
func isRateLimitErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrRateLimited) {
		return true
	}
	var sc statusCoder
	if errors.As(err, &sc) {
		if c := sc.StatusCode(); c == 418 || c == 429 {
			return true
		}
	}
	var ce *ccxt.Error
	if errors.As(err, &ce) {
		switch ce.Type {
		case ccxt.RateLimitExceededErrType, ccxt.DDoSProtectionErrType:
			return true
		}
	}
	msg := strings.ToLower(err.Error())
	for _, w := range rateLimitWords {
		if strings.Contains(msg, w) {
			return true
		}
	}
	return statusRE.MatchString(msg)
}

// isNotSupportedErr reports whether err says the exchange cannot do the call (as
// opposed to failing at it): the batch ticker call on an adapter without one.
func isNotSupportedErr(err error) bool {
	if err == nil {
		return false
	}
	var ce *ccxt.Error
	if errors.As(err, &ce) && ce.Type == ccxt.NotSupportedErrType {
		return true
	}
	return notSupportedRE.MatchString(err.Error())
}

// errLabel is a short, bounded, printable form of err for the health report and
// the logs: an exchange's error text can be long and can carry whatever the
// exchange chose to put in it.
func errLabel(err error) string {
	if err == nil {
		return ""
	}
	s := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(err.Error(), ""))
	if r := []rune(s); len(r) > 200 {
		s = string(r[:200])
	}
	return s
}

// timeframeDuration parses a ccxt timeframe such as "1m", "15m", "4h", "1d", "1w"
// ("1M" is a month, taken as 30 days). It reports false for anything else.
func timeframeDuration(tf string) (time.Duration, bool) {
	tf = strings.TrimSpace(tf)
	if len(tf) < 2 {
		return 0, false
	}
	n, err := strconv.Atoi(tf[:len(tf)-1])
	if err != nil || n <= 0 || n > 1000 {
		return 0, false
	}
	var unit time.Duration
	switch tf[len(tf)-1] {
	case 's':
		unit = time.Second
	case 'm':
		unit = time.Minute
	case 'h':
		unit = time.Hour
	case 'd':
		unit = 24 * time.Hour
	case 'w':
		unit = 7 * 24 * time.Hour
	case 'M':
		unit = 30 * 24 * time.Hour
	default:
		return 0, false
	}
	return time.Duration(n) * unit, true
}
