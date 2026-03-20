package snapshot

import "time"

// Position represents a single asset holding within a snapshot.
// It is market-agnostic: Source can be an exchange, broker, or any other provider.
type Position struct {
	ID         int64
	SnapshotID int64
	Source     string            // "binance", "tdameritrade", "interactive-brokers"
	Account    string            // "HighRiskPort", "Roth-IRA", ""
	Category   string            // "spot", "futures_usdt", "stock", "forex", "bond"
	Asset      string            // "BTC", "AAPL", "EUR/USD"
	Quantity   float64
	Price      float64           // unit price in snapshot's quote currency
	Value      float64           // quantity * price
	Meta       map[string]string // "locked", "unrealized_pnl", "sector", etc.
}

// Snapshot captures the state of a portfolio at a point in time.
type Snapshot struct {
	ID         int64
	TakenAt    time.Time
	Quote      string // "USDT", "USD", "THB", "EUR"
	TotalValue float64
	Label      string // "daily", "heartbeat", "pre-rebalance"
	Note       string
	Positions  []Position
}

// QueryFilter controls which snapshots are returned by QuerySnapshots.
type QueryFilter struct {
	Since, Until         *time.Time
	Label, Source, Asset string
	Limit, Offset        int // default limit=20, max=100
}

// SummaryFilter controls which snapshots are included in SnapshotSummary.
type SummaryFilter struct {
	Since, Until  *time.Time
	Label, Source string
	GroupBy       string // "day", "week", "month", "source"
}

// DeleteFilter controls which snapshots are removed by DeleteSnapshots.
type DeleteFilter struct {
	ID       *int64
	Before   *time.Time
	Label    string
	KeepLast int // safety: always keep N most recent (default 5)
}

// Summary is the result of SnapshotSummary.
type Summary struct {
	Count          int
	Earliest       time.Time
	Latest         time.Time
	MinValue       float64
	MaxValue       float64
	AvgValue       float64
	LatestValue    float64
	Change         float64 // latest - earliest (change from start)
	ChangePct      float64
	PrevValue      float64 // second-to-last snapshot value
	PrevChange     float64 // latest - previous (change from immediate previous)
	PrevChangePct  float64
	Quote          string
	Groups         []GroupSummary
}

// GroupSummary is a single group in a Summary result.
type GroupSummary struct {
	Key                          string
	Count                        int
	AvgValue, MinValue, MaxValue float64
}
