package worldmodel

import (
	"errors"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// ErrNoMark is returned by SinceMark for a mark that was never set.
var ErrNoMark = errors.New("worldmodel: no such mark")

// View is the world model as a trigger reads it. Every method is safe to call
// from any goroutine and returns copies. A restored fact keeps the timestamp it
// had when it was saved, so every answer that is about a moment carries it, and
// Fresh is the test for "recent enough to act on".
type View interface {
	// Now is the store's clock.
	Now() time.Time
	// Fresh reports whether a fact stamped at is no older than maxAge. A zero
	// stamp, and a stamp more than a minute ahead of the clock, are not fresh.
	Fresh(at time.Time, maxAge time.Duration) bool

	// Price is the last price of a symbol and when it was observed.
	Price(provider, symbol string) (p float64, at time.Time, ok bool)
	// PriceTrust is the trust of the event that set the price.
	PriceTrust(provider, symbol string) (proactive.Trust, bool)

	// Portfolio is the last complete portfolio snapshot with its day-start
	// baseline and 30-day peak.
	Portfolio() (PortfolioView, bool)

	// FuturesPosition is one open position. account is "provider/account".
	FuturesPosition(account, symbol string) (FuturesView, bool)
	// FuturesPositions are all open positions of one account, by symbol. The
	// boolean says the account has been seen (an empty slice is a flat account).
	FuturesPositions(account string) ([]FuturesView, bool)

	// Plan is the last reported state of a plan ("dn", "dca") by id.
	Plan(kind string, id int64) (PlanView, bool)

	// Series names are "price:<provider>:<symbol>" (the 1-minute return series) and
	// the same with "@1h" appended.
	Series(name string) (SeriesView, bool)

	// Source is the health of a perception source as the model last heard it.
	Source(name string) (perception.SourceHealth, bool)
	// SourceState adds which accounts or providers a source cannot read.
	SourceState(name string) (SourceView, bool)

	// LastFill is the last fill of a symbol on an account ("provider/account").
	LastFill(account, symbol string) (FillView, bool)
}

// Change is one fact that moved. Subject and Key name the fact, Old and New are
// its values (nil for "absent"). Values are scalars: numbers, strings, booleans.
//
//	price:<provider>:<symbol>          close_1m, close_1h
//	portfolio                          total_usd, class:<name>, unpriced_assets
//	position:<venue>:<asset>           qty (appeared: from nil, gone: to nil)
//	futures:<provider>/<account>:<sym> long, short or position: the key is the side
//	                                   and the values are contract counts
//	order:<provider>/<account>:<sym>   fill
//	plan:<kind>:<id>                   status, next_due
//	source:<name>                      state
type Change struct {
	Subject string `json:"subject"`
	Key     string `json:"key"`
	Old     any    `json:"old,omitempty"`
	New     any    `json:"new,omitempty"`
	// Trust is the lowest trust among the inputs of the change.
	Trust proactive.Trust `json:"trust"`
	// Minor changes (a one-minute price close) are reported to the evaluators in the
	// Diff and are not kept in the change log that "since you last looked" reads:
	// their volume would drown the log and they say nothing a one-hour close does
	// not say better.
	Minor bool `json:"minor,omitempty"`
}

// Signal is a detector's verdict on a series. Series is "price:<provider>:<symbol>"
// with "@1m" or "@1h"; Detector is "ewma_z" (Value is the z-score of the latest
// bucket return) or "cusum" (Value is the sum that crossed its threshold, signed
// by direction). Signals are reported when they fire.
type Signal struct {
	Series   string             `json:"series"`
	Detector string             `json:"detector"`
	Value    float64            `json:"value"`
	Fired    bool               `json:"fired"`
	Detail   map[string]float64 `json:"detail,omitempty"`
	Trust    proactive.Trust    `json:"trust"`
}

// Diff is what an event, or a stretch of time since a mark, changed.
//
// Event is the envelope of the event that caused it: id, kind, time, source and
// trust, with the payload and attributes removed (a portfolio payload holds up to
// 200 positions, and a Diff goes onto the event bus). The data a consumer needs is
// in Changes and Signals, or in the View.
type Diff struct {
	Event   events.Event `json:"event"`
	At      time.Time    `json:"at"`
	Changes []Change     `json:"changes,omitempty"`
	Signals []Signal     `json:"signals,omitempty"`
	// Truncated is set on a SinceMark result when the mark is older than the change
	// log: what is returned is what is still there, and earlier changes are gone
	// (pruned after seven days, or dropped when the log's memory bound was hit).
	Truncated bool `json:"truncated,omitempty"`
	// Payload is the untracked event's own payload, for the trigger that handles
	// that kind; never serialised, never logged, never on the bus. Apply sets it only
	// for an event of a kind the model does not track (see Tracks), so a tracked
	// payload such as a portfolio of 200 positions is never carried; such a Diff has
	// no change and no signal, so it is Empty and the pipeline does not publish it.
	// The text in it comes from outside: a trigger type-asserts it and treats
	// anything it does not recognise as not its own.
	Payload any `json:"-"`
}

// Empty reports whether the diff carries nothing: no change and no signal.
func (d Diff) Empty() bool { return len(d.Changes) == 0 && len(d.Signals) == 0 }

// PositionView is one portfolio holding.
type PositionView struct {
	Asset string
	Venue string
	Class string
	Value float64 // USD
	Qty   float64
}

// PortfolioView is the last complete portfolio snapshot. Its values exclude the
// assets that could not be priced (UnpricedCount of them), and DayStartUSD and
// Peak30dUSD are always on the same footing as TotalUSD: when the set of unpriced
// assets changes, both are re-based on the newer snapshot, and DayStartRebased
// says the day-start baseline was moved during the day for that reason.
type PortfolioView struct {
	At         time.Time
	SnapshotID int64
	TotalUSD   float64
	ByClass    map[string]float64
	Positions  []PositionView
	// PositionsTruncated is set when the snapshot had more holdings than it carried.
	PositionsTruncated bool

	UnpricedCount     int
	UnpricedTruncated bool

	// DayStartUSD is the total of the first complete snapshot of the owner's local
	// day DayDate (YYYY-MM-DD), taken at DayStartAt.
	DayStartUSD     float64
	DayStartAt      time.Time
	DayDate         string
	DayStartRebased bool
	// Peak30dUSD is the highest total of the last 30 days on the same footing.
	Peak30dUSD float64

	Trust proactive.Trust
}

// FuturesView is one open futures position. A number whose Known flag is false is
// absent, not zero. Contracts is a CONTRACT COUNT.
type FuturesView struct {
	Account string // "provider/account"
	Symbol  string
	Side    string // "long", "short" or ""

	Contracts float64

	ContractSize      float64
	ContractSizeKnown bool
	MarkPrice         float64
	MarkPriceKnown    bool
	LiqPrice          float64
	LiqPriceKnown     bool
	MarginRatio       float64
	MarginRatioKnown  bool
	UnrealizedPnl     float64
	UnrealizedKnown   bool
	NotionalUSD       float64
	NotionalKnown     bool

	// At is when the account was read. AccountDegraded says the futures source
	// reported that it could not read this account since: the position is shown as
	// it was last seen, and At says how long ago.
	At              time.Time
	AccountDegraded bool
	Trust           proactive.Trust
}

// PlanView is the last reported state of a plan.
type PlanView struct {
	Kind    string
	ID      int64
	Status  string
	Health  map[string]float64
	NextDue time.Time
	At      time.Time
	Trust   proactive.Trust
}

// SeriesView describes one return series. Samples is the number of returns the
// EWMA has seen; LastZ is meaningful only when LastZValid.
type SeriesView struct {
	Name       string
	Bucket     time.Duration
	Mean       float64
	Sigma      float64
	Samples    int
	LastZ      float64
	LastZValid bool
	// CUSUMPos and CUSUMNeg are the running sums of the 1-minute series (zero for
	// the 1-hour series, which has no CUSUM).
	CUSUMPos float64
	CUSUMNeg float64
	At       time.Time
	Trust    proactive.Trust
}

// SourceView is what the model knows about a source beyond perception.SourceHealth.
type SourceView struct {
	Name        string
	Degraded    bool
	Reason      string
	Failed      []string // accounts or providers the source cannot read, sorted
	LastEventAt time.Time
	LastErrorAt time.Time
	// ConsecutiveDegraded counts degraded reports since the source last recovered.
	ConsecutiveDegraded int
}

// FillView is the last fill of a symbol on an account.
type FillView struct {
	Side          string
	Filled        float64
	FilledKnown   bool
	Unit          string // "base", "contracts", "option_contracts"
	AvgPrice      float64
	AvgPriceKnown bool
	At            time.Time
	Trust         proactive.Trust
}

// Stats are the store's counters. Counts and gauges only: no name, no value.
type Stats struct {
	Applied           uint64 // events handled
	Ignored           uint64 // events of a kind the model does not track
	DroppedInvalid    uint64 // non-finite, non-positive or negative numbers, bad enumerations
	DroppedBadName    uint64 // names that are empty, too long or have characters outside the allowed set
	DroppedOutOfOrder uint64 // older than the fact they would replace
	DroppedPaper      uint64 // simulated orders, which are never fills
	DecodeErrors      uint64 // payloads that could not be decoded
	Panics            uint64 // Apply panics, recovered
	FutureClamped     uint64 // events stamped in the future, treated as now
	Truncated         uint64 // lists cut to their bound (positions, classes, health entries)

	SeriesEvicted    uint64
	FactsEvicted     uint64 // plans, fills, futures accounts, sources evicted at their bound
	ChangesLogged    uint64
	ChangesDropped   uint64 // oldest log entries dropped at the memory bound
	SnapshotsRebased uint64 // portfolio snapshots not compared because the unpriced set changed

	Checkpoints        uint64
	CheckpointFailures uint64
	RestoredRows       uint64
	DetectorsReset     uint64 // restored detector states discarded as stale

	Series         int // gauge
	BufferedLog    int // gauge: log entries not yet written
	DirtyRows      int // gauge: rows changed since the last checkpoint
	LastCheckpoint time.Time
}
