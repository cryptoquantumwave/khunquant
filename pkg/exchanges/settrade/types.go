package settrade

// --- Portfolio (SEOS v3) ---

// accountInfoResponse maps /api/seos/v3/{broker}/accounts/{acc}/account-info (flat, no data wrapper).
type accountInfoResponse struct {
	CashBalance   float64 `json:"cashBalance"`
	LineAvailable float64 `json:"lineAvailable"` // buying power
	CreditLimit   float64 `json:"creditLimit"`
	CanBuy        bool    `json:"canBuy"`
	CanSell       bool    `json:"canSell"`
}

// portfolioItem maps one entry in portfolioList from /api/seos/v3/{broker}/accounts/{acc}/portfolios.
// Volumes are in shares (not lots).
type portfolioItem struct {
	Symbol        string  `json:"symbol"`
	CurrentVolume float64 `json:"currentVolume"` // total shares held
	ActualVolume  float64 `json:"actualVolume"`  // shares available to sell
	AveragePrice  float64 `json:"averagePrice"`  // average cost price
	MarketPrice   float64 `json:"marketPrice"`
	MarketValue   float64 `json:"marketValue"`
	Profit        float64 `json:"profit"` // unrealized P&L
	PercentProfit float64 `json:"percentProfit"`
}

// portfolioResponse maps the /portfolios response.
// The list is under "portfolioList", not "data".
type portfolioResponse struct {
	PortfolioList []portfolioItem `json:"portfolioList"`
}

// --- Orders (SEOS v3) ---

// createOrderRequest matches the SDK v2 place_order body.
// Volume is in lots (1 lot = 100 shares); converted from shares internally.
type createOrderRequest struct {
	PIN           string  `json:"pin"`
	Side          string  `json:"side"` // "Buy" | "Sell"
	Symbol        string  `json:"symbol"`
	TrusteeIDType string  `json:"trusteeIdType"` // "Local"
	Volume        int     `json:"volume"`        // lots
	QtyOpen       int     `json:"qtyOpen"`
	Price         float64 `json:"price,omitempty"`
	PriceType     string  `json:"priceType"`    // "Limit" | "ATO"
	ValidityType  string  `json:"validityType"` // "Day"
	ClientType    string  `json:"clientType"`   // "Individual"
}

type cancelOrderRequest struct {
	PIN string `json:"pin"`
}

// changeOrderRequest matches equity.change_order() body.
type changeOrderRequest struct {
	PIN           string   `json:"pin"`
	NewPrice      *float64 `json:"newPrice,omitempty"`
	NewVolume     *int     `json:"newVolume,omitempty"` // lots
	TrusteeIDType string   `json:"newTrusteeIdType,omitempty"`
	BypassWarning *bool    `json:"bypassWarning,omitempty"`
}

// cancelOrdersRequest matches equity.cancel_orders() body (bulk cancel).
type cancelOrdersRequest struct {
	PIN    string   `json:"pin"`
	Orders []string `json:"orders"`
}

type settradeOrder struct {
	OrderNo   string  `json:"orderNo"`
	Symbol    string  `json:"symbol"`
	Side      string  `json:"side"`
	PriceType string  `json:"priceType"`
	Volume    float64 `json:"vol"`     // shares (API field is "vol")
	FilledVol float64 `json:"matched"` // shares filled
	Balance   float64 `json:"balance"` // shares remaining (unfilled)
	Price     float64 `json:"price"`
	Status    string  `json:"status"`
	EntryDate string  `json:"entryTime"` // ISO datetime (more precise than date-only "entryDate")
}

type orderResponse struct {
	Data settradeOrder `json:"data"`
}


// --- Market Data (marketapi.settrade.com, flat response) ---

// quoteResponse maps /api/marketdata/v3/{broker}/quote/{symbol}.
type quoteResponse struct {
	Symbol        string  `json:"symbol"`
	Last          float64 `json:"last"`
	High          float64 `json:"high"`
	Low           float64 `json:"low"`
	Change        float64 `json:"change"`
	PercentChange float64 `json:"percentChange"`
	TotalVolume   float64 `json:"totalVolume"`
}

// --- Candlestick / OHLCV (techchart host) ---

// candlestickBar maps one candle from /api/techchart/v3/{broker}/candlesticks.
// The API returns each field as a single-element array (batch format).
// Intervals: '1m','3m','5m','10m','15m','30m','60m','120m','240m','1d','1w','1M'
type candlestickBar struct {
	Time         []int64   `json:"time"` // Unix timestamp (seconds)
	Open         []float64 `json:"open"`
	High         []float64 `json:"high"`
	Low          []float64 `json:"low"`
	Close        []float64 `json:"close"`
	Volume       []float64 `json:"volume"` // shares
	Value        []float64 `json:"value"`  // trading value in THB
	LastSequence int64     `json:"lastSequence"`
}

// OHLCV is a flattened single candlestick bar for use by tools/agent.
type OHLCV struct {
	Time   int64
	Open   float64
	High   float64
	Low    float64
	Close  float64
	Volume float64
	Value  float64
}

// --- Trades (SEOS v4) ---

type tradeRecord struct {
	TradeID   string  `json:"tradeId"`
	OrderNo   string  `json:"orderNo"`
	Symbol    string  `json:"symbol"`
	Side      string  `json:"side"`
	Volume    float64 `json:"volume"` // shares (consistent with order vol and portfolio)
	Price     float64 `json:"price"`
	TradeDate string  `json:"tradeDate"`
}

type tradesResponse struct {
	Data []tradeRecord `json:"data"`
}
