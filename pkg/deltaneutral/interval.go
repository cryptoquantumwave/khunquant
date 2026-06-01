package deltaneutral

import (
	"fmt"
	"time"
)

// DefaultMonitorInterval is the default plan monitoring interval
const DefaultMonitorInterval = "5m"

// supportedIntervals maps interval strings to time.Duration
var supportedIntervals = map[string]time.Duration{
	"30s": 30 * time.Second,
	"1m":  1 * time.Minute,
	"3m":  3 * time.Minute,
	"5m":  5 * time.Minute,
	"10m": 10 * time.Minute,
	"15m": 15 * time.Minute,
	"30m": 30 * time.Minute,
	"1h":  1 * time.Hour,
	"2h":  2 * time.Hour,
	"3h":  3 * time.Hour,
	"4h":  4 * time.Hour,
	"8h":  8 * time.Hour,
	"1d":  24 * time.Hour,
}

// ParseInterval parses an interval string and returns its duration, or an error if unsupported
func ParseInterval(s string) (time.Duration, error) {
	if d, ok := supportedIntervals[s]; ok {
		return d, nil
	}
	return 0, fmt.Errorf("unsupported interval: %q", s)
}

// IntervalToMS converts an interval string to milliseconds, or returns an error if unsupported
func IntervalToMS(s string) (int64, error) {
	d, err := ParseInterval(s)
	if err != nil {
		return 0, err
	}
	return int64(d / time.Millisecond), nil
}

// IsSubMinute returns true if the interval is 30s or 1m (sub-minute intervals)
func IsSubMinute(s string) bool {
	return s == "30s" || s == "1m"
}

// ValidInterval returns true if the interval string is supported
func ValidInterval(s string) bool {
	_, ok := supportedIntervals[s]
	return ok
}

// IntervalFromMS converts milliseconds back to an interval string.
// Returns the string and true if found, or "" and false if no match.
func IntervalFromMS(ms int64) (string, bool) {
	target := time.Duration(ms) * time.Millisecond
	for s, d := range supportedIntervals {
		if d == target {
			return s, true
		}
	}
	return "", false
}
