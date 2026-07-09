package webull

// --- Account and Portfolio ---

// AccountResponse maps the /v2/trading/accounts/{accountId} endpoint response.
type AccountResponse struct {
	ID       string `json:"id"`
	Status   string `json:"status"` // e.g., "NORMAL", "SUSPENDED"
	Currency string `json:"currency"`
	// Additional fields as needed
}

// BalanceItem represents a single balance entry in the balances response.
type BalanceItem struct {
	Asset       string  `json:"asset"`
	Free        float64 `json:"free"`
	Locked      float64 `json:"locked"`
	Total       float64 `json:"total"`
	MarketValue float64 `json:"market_value,omitempty"`
}

// BalancesResponse maps the /v2/trading/accounts/{accountId}/balances endpoint.
type BalancesResponse struct {
	Balances []BalanceItem `json:"balances"`
	Total    float64       `json:"total,omitempty"`
}

// PositionItem represents a single position.
type PositionItem struct {
	Symbol        string  `json:"symbol"`
	Quantity      float64 `json:"quantity"`
	AvgPrice      float64 `json:"avg_price"`
	CurrentPrice  float64 `json:"current_price"`
	MarketValue   float64 `json:"market_value"`
	UnrealizedPnL float64 `json:"unrealized_pnl,omitempty"`
	PercentPnL    float64 `json:"percent_pnl,omitempty"`
}

// PositionsResponse maps the /v2/trading/accounts/{accountId}/positions endpoint.
type PositionsResponse struct {
	Positions []PositionItem `json:"positions"`
}

// --- Market Data ---

// QuoteResponse maps a single quote from the /v2/market/quotes/{symbol} endpoint.
type QuoteResponse struct {
	Symbol        string  `json:"symbol"`
	Last          float64 `json:"last"`
	Bid           float64 `json:"bid,omitempty"`
	Ask           float64 `json:"ask,omitempty"`
	High          float64 `json:"high,omitempty"`
	Low           float64 `json:"low,omitempty"`
	Open          float64 `json:"open,omitempty"`
	Close         float64 `json:"close,omitempty"`
	Volume        float64 `json:"volume,omitempty"`
	Change        float64 `json:"change,omitempty"`
	ChangePercent float64 `json:"change_percent,omitempty"`
	Timestamp     int64   `json:"timestamp,omitempty"` // milliseconds
	PreviousClose float64 `json:"previous_close,omitempty"`
}

// BarItem represents a single candlestick bar.
type BarItem struct {
	Timestamp int64   `json:"timestamp"` // milliseconds
	Open      float64 `json:"open"`
	High      float64 `json:"high"`
	Low       float64 `json:"low"`
	Close     float64 `json:"close"`
	Volume    float64 `json:"volume"`
}

// BarsResponse maps the /v2/market/bars endpoint response.
type BarsResponse struct {
	Symbol string    `json:"symbol"`
	Bars   []BarItem `json:"bars"`
}

// --- Error Response ---

// ErrorResponse maps a Webull API error response.
type ErrorResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Details string `json:"details,omitempty"`
}

// Error implements the error interface for ErrorResponse.
func (e ErrorResponse) Error() string {
	if e.Details != "" {
		return "webull: " + e.Message + " (" + e.Details + ")"
	}
	return "webull: " + e.Message
}
