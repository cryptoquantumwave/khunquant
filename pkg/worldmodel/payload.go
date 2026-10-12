package worldmodel

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/perception/orderfeed"
)

// The payload types below mirror, field for field and tag for tag, the payloads
// the perception sources and the gateway put on their events
// (sources.MarketTick, sources.PortfolioSnapshotPayload,
// sources.FuturesPositionsPayload, sources.SourceHealthPayload, sources.MarketHealth,
// sources.ClockDayStartPayload). The model decodes through them instead of importing
// the sources package, which would pull the exchange clients and the broker into
// its import closure. A payload that is already one of these types is used as is;
// any other value (the sources' own struct, or the map a replayed event log
// yields) is decoded through JSON. A contract test in the gateway marshals the real
// source payloads and decodes them here, so a renamed field cannot go unnoticed.
//
// The plan payloads have no producer yet; they are defined here and the source
// that emits them has to conform.

// TickPayload mirrors sources.MarketTick.
type TickPayload struct {
	Provider string    `json:"provider"`
	Symbol   string    `json:"symbol"`
	Last     float64   `json:"last"`
	At       time.Time `json:"at"`
}

// PortfolioPositionPayload mirrors sources.PortfolioPosition.
type PortfolioPositionPayload struct {
	Asset  string  `json:"asset"`
	Source string  `json:"source"`
	Class  string  `json:"class"`
	Value  float64 `json:"value"`
	Qty    float64 `json:"qty"`
}

// PortfolioPayload mirrors sources.PortfolioSnapshotPayload.
type PortfolioPayload struct {
	SnapshotID        int64                      `json:"snapshot_id,omitempty"`
	TakenAt           time.Time                  `json:"taken_at"`
	Quote             string                     `json:"quote"`
	TotalValueUSD     float64                    `json:"total_value_usd"`
	ByClass           map[string]float64         `json:"by_class"`
	Positions         []PortfolioPositionPayload `json:"positions"`
	Truncated         bool                       `json:"truncated,omitempty"`
	Unpriced          []string                   `json:"unpriced,omitempty"`
	UnpricedTruncated bool                       `json:"unpriced_truncated,omitempty"`
}

// FuturesPositionPayload mirrors sources.FuturesPosition.
type FuturesPositionPayload struct {
	Symbol             string  `json:"symbol"`
	Side               string  `json:"side"`
	Contracts          float64 `json:"contracts"`
	ContractSize       float64 `json:"contract_size"`
	ContractSizeKnown  bool    `json:"contract_size_known"`
	MarkPrice          float64 `json:"mark_price"`
	MarkPriceKnown     bool    `json:"mark_price_known"`
	LiqPrice           float64 `json:"liq_price"`
	LiqPriceKnown      bool    `json:"liq_price_known"`
	MarginRatio        float64 `json:"margin_ratio"`
	MarginRatioKnown   bool    `json:"margin_ratio_known"`
	UnrealizedPnl      float64 `json:"unrealized_pnl"`
	UnrealizedPnlKnown bool    `json:"unrealized_pnl_known"`
	NotionalUSD        float64 `json:"notional_usd"`
	NotionalKnown      bool    `json:"notional_known"`
}

// FuturesPayload mirrors sources.FuturesPositionsPayload.
type FuturesPayload struct {
	Provider  string                   `json:"provider"`
	Account   string                   `json:"account"`
	At        time.Time                `json:"at"`
	Positions []FuturesPositionPayload `json:"positions"`
}

// HealthPayload is the union of sources.SourceHealthPayload and
// sources.MarketHealth, the two payloads of a perception.source.health event.
// Healthy is set (non-nil) only by the market source.
type HealthPayload struct {
	Source         string    `json:"source"`
	State          string    `json:"state"`
	Reason         string    `json:"reason"`
	FailedAccounts []string  `json:"failed_accounts,omitempty"`
	Provider       string    `json:"provider"`
	Healthy        *bool     `json:"healthy"`
	Failures       int       `json:"failures"`
	At             time.Time `json:"at"`
}

// DayStartPayload mirrors sources.ClockDayStartPayload.
type DayStartPayload struct {
	Date string    `json:"date"`
	Zone string    `json:"zone"`
	At   time.Time `json:"at"`
}

// PlanPayload is the payload of perception.plan.health and perception.plan.dca_due.
// Kind is the plan family ("dn", "dca"); a field left zero is left unchanged.
type PlanPayload struct {
	Kind    string             `json:"kind"`
	ID      int64              `json:"id"`
	Status  string             `json:"status"`
	Health  map[string]float64 `json:"health,omitempty"`
	NextDue time.Time          `json:"next_due"`
	At      time.Time          `json:"at"`
}

// errNonFinite says a payload held a NaN or an infinity.
var errNonFinite = errors.New("payload holds a non-finite number")

// decode returns the payload as T: directly when it already is one (by value or
// pointer), otherwise through JSON.
func decode[T any](payload any) (T, error) {
	var zero T
	switch v := payload.(type) {
	case T:
		return v, nil
	case *T:
		if v != nil {
			return *v, nil
		}
		return zero, fmt.Errorf("nil %T payload", zero)
	case nil:
		return zero, fmt.Errorf("no payload")
	}
	b, err := json.Marshal(payload)
	if err != nil {
		var unsupported *json.UnsupportedValueError
		if errors.As(err, &unsupported) {
			return zero, errNonFinite // JSON cannot carry NaN or Inf
		}
		return zero, err
	}
	var out T
	if err := json.Unmarshal(b, &out); err != nil {
		return zero, err
	}
	return out, nil
}

// decodeOrder returns the payload as an orderfeed.OrderEvent (the gateway puts one
// on the event as it is).
func decodeOrder(payload any) (orderfeed.OrderEvent, error) {
	return decode[orderfeed.OrderEvent](payload)
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// nonNegative reports whether v is a finite number that is not below zero.
func nonNegative(v float64) bool { return finite(v) && v >= 0 }
