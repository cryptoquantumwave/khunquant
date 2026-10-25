package perception

import (
	"sync"
	"time"
)

// DemandKind says what a Demand wants to be told about.
type DemandKind int

const (
	// DemandTicker wants the last price of a symbol.
	DemandTicker DemandKind = iota
	// DemandOHLCV wants closed candles of a symbol at a timeframe.
	DemandOHLCV
	// DemandFuturesPositions wants an account's futures positions. The market
	// source does not serve it (the futures source does); it is part of the
	// vocabulary so that one registry can name every kind of read.
	DemandFuturesPositions
	// DemandPlanHealth wants a plan checked and its health reported: Provider is the
	// plan family ("dn") and Symbol the plan's numeric ID. MinInterval is not used: the
	// plan's own monitor interval decides how often it is checked, and a source never
	// checks a plan more than once a minute. The plan-health source serves it; the
	// market source does not.
	DemandPlanHealth
)

// String names the kind, for logs.
func (k DemandKind) String() string {
	switch k {
	case DemandTicker:
		return "ticker"
	case DemandOHLCV:
		return "ohlcv"
	case DemandFuturesPositions:
		return "futures_positions"
	case DemandPlanHealth:
		return "plan_health"
	}
	return "unknown"
}

// Demand is one thing somebody wants to know about the market: an alert's
// symbol, a trigger's candle series. Sources read only what is demanded, so no
// demand means no request.
type Demand struct {
	Provider string
	// Account is the configured account the read is made through. Market data is
	// public, but the account's permissions still decide whether it may be read
	// with that account's credentials.
	Account string
	Symbol  string
	Kind    DemandKind
	// Timeframe is the candle size ("1m", "1h", "1d"); OHLCV only.
	Timeframe string
	// MinInterval is how fresh the demander needs the data. Zero means "as often
	// as the source polls"; a source never polls faster than its own cadence.
	MinInterval time.Duration
	// Lookback is how many closed candles the demander wants to see in each
	// candle_closed event (OHLCV only). Zero is one; a source caps it.
	Lookback int
}

// DemandSource is where a source learns what to read. Demands returns the demands
// in force now; the caller must not modify the result. It may be called from any
// goroutine, as often as the source wants to know.
//
// The trigger registry implements it (MR-208). Until then the gateway wires an
// empty one.
type DemandSource interface {
	Demands() []Demand
}

// StaticDemands is a DemandSource whose demands are set by hand. The zero value
// is an empty source. It is safe for concurrent use.
type StaticDemands struct {
	mu sync.RWMutex
	ds []Demand
}

// NewStaticDemands returns a source holding ds.
func NewStaticDemands(ds ...Demand) *StaticDemands {
	s := &StaticDemands{}
	s.Set(ds)
	return s
}

// Set replaces the demands.
func (s *StaticDemands) Set(ds []Demand) {
	cp := append([]Demand(nil), ds...)
	s.mu.Lock()
	s.ds = cp
	s.mu.Unlock()
}

// Add appends d to the demands.
func (s *StaticDemands) Add(d Demand) {
	s.mu.Lock()
	s.ds = append(s.ds[:len(s.ds):len(s.ds)], d)
	s.mu.Unlock()
}

// Demands returns the demands now in force. The slice is never modified after it
// is returned.
func (s *StaticDemands) Demands() []Demand {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ds
}
