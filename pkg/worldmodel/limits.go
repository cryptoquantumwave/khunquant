package worldmodel

import (
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/worldmodel/detect"
)

// Bounds on what outside data can grow. A name or list over its bound is dropped
// (and counted), never truncated: cutting a symbol short could make two symbols
// one key.
const (
	// MaxSeries is the number of price series held. Above it the least recently
	// updated are evicted, a tenth at a time, and a WARN is logged.
	MaxSeries = 2000
	// MaxPlans, MaxFills, MaxFuturesAccounts and MaxSources bound the other fact
	// families the same way.
	MaxPlans           = 500
	MaxFills           = 500
	MaxFuturesAccounts = 64
	MaxSources         = 64
	// MaxFuturesPositions is the positions kept per account; MaxPortfolioPositions
	// the holdings kept of a snapshot (the source already caps it at 200); MaxClasses
	// the asset classes; MaxHealthEntries the numbers of a plan's health;
	// MaxFailedNames the failed accounts or providers kept per source.
	MaxFuturesPositions   = 200
	MaxPortfolioPositions = 200
	MaxClasses            = 32
	MaxHealthEntries      = 16
	MaxFailedNames        = 64
	// MaxMarks bounds the named marks.
	MaxMarks = 32

	// MaxTokenLen bounds a provider, account, venue, source, kind or status name;
	// MaxSymbolLen a symbol or asset.
	MaxTokenLen  = 48
	MaxSymbolLen = 64

	// MaxLogBuffer is the change-log entries kept in memory before a checkpoint
	// writes them. At the bound the OLDEST is dropped and counted (and a mark older
	// than the drop sees Truncated), because a log whose newest entries are refused
	// would make "what changed since I looked" wrong in the direction that hides the
	// latest news.
	MaxLogBuffer = 2048
	// maxScan bounds the rows SinceMark reads from the database.
	maxScan = 20000
	// maxPositionChanges bounds the appeared/gone position changes of one snapshot.
	maxPositionChanges = 20

	// ChangeRetention is how long the change log is kept.
	ChangeRetention = 7 * 24 * time.Hour
	// CheckpointEvery is the checkpoint period; a failed one is retried after
	// checkpointRetry. A checkpoint is abandoned after checkpointTimeout.
	CheckpointEvery   = 5 * time.Minute
	checkpointRetry   = time.Minute
	checkpointTimeout = 30 * time.Second

	// futureSkew is how far ahead of the clock an event may be stamped before it is
	// treated as stamped now.
	futureSkew = 5 * time.Minute
	// freshSkew is how far ahead of the clock a fact stamp may be to still count as
	// fresh in View.Fresh.
	freshSkew = time.Minute

	// warnEvery rate-limits each kind of log line.
	warnEvery = time.Minute
)

// The two return series every price series runs. Half-lives are in buckets:
// the one-minute EWMA remembers about an hour, the one-hour one about a day.
// MinSigma floors the scale of a return so that a flat stretch cannot turn the next
// tick into an enormous z (2 and 3 basis points). MaxGap is how many empty buckets a
// return may span before the series re-bases instead (see detect.ReturnSeries).
//
// A restored detector is trusted only if its series was updated within
// StaleBuckets of its own bucket before the restart: 30 minutes for the minute
// series (half a half-life), 6 hours for the hour series. Older state describes a
// market that is no longer this one, and is reset.
const (
	minuteBucket       = time.Minute
	hourBucket         = time.Hour
	minuteHalfLife     = 60.0
	hourHalfLife       = 24.0
	minuteMinSamples   = 20
	hourMinSamples     = 12
	minuteMinSigma     = 2e-4
	hourMinSigma       = 3e-4
	minuteMaxGap       = 5
	hourMaxGap         = 3
	minuteStaleBuckets = 30
	hourStaleBuckets   = 6

	// zFire is the |z| at which an EWMA signal fires; cusumK and cusumH are the
	// allowance and threshold of the one-minute CUSUM, in standard deviations.
	zFire  = 4.0
	cusumK = 0.5
	cusumH = 5.0
)

func newMinuteSeries() detect.ReturnSeries {
	return detect.ReturnSeries{
		Bucket: minuteBucket, MaxGap: minuteMaxGap,
		E: detect.EWMA{HalfLife: minuteHalfLife, MinSamples: minuteMinSamples, MinSigma: minuteMinSigma},
	}
}

func newHourSeries() detect.ReturnSeries {
	return detect.ReturnSeries{
		Bucket: hourBucket, MaxGap: hourMaxGap,
		E: detect.EWMA{HalfLife: hourHalfLife, MinSamples: hourMinSamples, MinSigma: hourMinSigma},
	}
}

func newCUSUM() detect.CUSUM { return detect.CUSUM{K: cusumK, H: cusumH} }

// validToken reports whether s is a usable provider, account, venue, source, kind
// or status name: 1..max bytes of letters, digits, '.', '_' and '-'.
func validToken(s string, max int) bool {
	if len(s) == 0 || len(s) > max {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// validSymbol is validToken that also allows '/', ':', '@' and '+', which
// symbols ("BTC/USDT:USDT", "SET:PTT") and account paths use.
func validSymbol(s string, max int) bool {
	if len(s) == 0 || len(s) > max {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '.' || c == '_' || c == '-' || c == '/' || c == ':' || c == '@' || c == '+') {
			return false
		}
	}
	return true
}
