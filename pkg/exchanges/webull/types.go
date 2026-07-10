package webull

import (
	"strconv"
)

// --- Authentication ---

// TokenResponse maps the /openapi/auth/token/create endpoint response.
type TokenResponse struct {
	Token   string `json:"token"`
	Expires int64  `json:"expires"` // unix milliseconds
	Status  string `json:"status"`  // PENDING, NORMAL, INVALID, EXPIRED
}

// --- Account ---

// AccountListItem represents a single account from /openapi/account/list.
type AccountListItem struct {
	AccountID     string `json:"account_id"`
	AccountType   string `json:"account_type"`
	AccountLabel  string `json:"account_label"`
	AccountClass  string `json:"account_class"`
	AccountNumber string `json:"account_number"`
	UserID        string `json:"user_id"`
}

// --- Balance ---

// BalanceResponse maps the /openapi/assets/balance endpoint.
// All numeric values are JSON strings.
type BalanceResponse struct {
	TotalAssetCurrency        string          `json:"total_asset_currency"`
	TotalNetLiquidationValue  string          `json:"total_net_liquidation_value"`
	TotalMarketValue          string          `json:"total_market_value"`
	TotalCashBalance          string          `json:"total_cash_balance"`
	TotalUnrealizedProfitLoss string          `json:"total_unrealized_profit_loss"`
	TotalDayProfitLoss        string          `json:"total_day_profit_loss"`
	AccountCurrencyAssets     []CurrencyAsset `json:"account_currency_assets"`
}

// CurrencyAsset represents a single currency balance entry.
type CurrencyAsset struct {
	Currency             string `json:"currency"`
	CashBalance          string `json:"cash_balance"`
	SettledCash          string `json:"settled_cash"`
	UnsettledCash        string `json:"unsettled_cash"`
	MarketValue          string `json:"market_value"`
	BuyingPower          string `json:"buying_power"`
	UnrealizedProfitLoss string `json:"unrealized_profit_loss"`
	NetLiquidationValue  string `json:"net_liquidation_value"`
	DayProfitLoss        string `json:"day_profit_loss"`
}

// --- Positions ---

// Position represents a single position from /openapi/assets/positions.
// Positions endpoint returns a JSON array of Position objects.
// All numeric values are JSON strings.
type Position struct {
	Currency                 string `json:"currency"`
	Quantity                 string `json:"quantity"`
	Cost                     string `json:"cost"`
	Proportion               string `json:"proportion"`
	PositionID               string `json:"position_id"`
	Symbol                   string `json:"symbol"`
	InstrumentType           string `json:"instrument_type"`
	CostPrice                string `json:"cost_price"`
	LastPrice                string `json:"last_price"`
	MarketValue              string `json:"market_value"`
	UnrealizedProfitLoss     string `json:"unrealized_profit_loss"`
	UnrealizedProfitLossRate string `json:"unrealized_profit_loss_rate"`
	DayProfitLoss            string `json:"day_profit_loss"`
	DayRealizedProfitLoss    string `json:"day_realized_profit_loss"`
}

// --- Market Data: Snapshot ---

// Snapshot represents a single security snapshot from /openapi/market-data/stock/snapshot.
// Snapshot endpoint returns a JSON array of Snapshot objects.
// All numeric values except last_trade_time are JSON strings.
type Snapshot struct {
	Symbol           string `json:"symbol"`
	InstrumentID     string `json:"instrument_id"`
	Price            string `json:"price"`
	PreClose         string `json:"pre_close"`
	Open             string `json:"open"`
	High             string `json:"high"`
	Low              string `json:"low"`
	Close            string `json:"close"`
	Volume           string `json:"volume"`
	Change           string `json:"change"`
	ChangeRatio      string `json:"change_ratio"`
	Bid              string `json:"bid"`
	BidSize          string `json:"bid_size"`
	Ask              string `json:"ask"`
	AskSize          string `json:"ask_size"`
	Turnover         string `json:"turnover"`
	EPS              string `json:"eps"`
	EPSttm           string `json:"eps_ttm"`
	BPS              string `json:"bps"`
	LastTradeTime    int64  `json:"last_trade_time"` // unix milliseconds
	QuoteTime        int64  `json:"quote_time"`      // unix milliseconds
	ListStatus       string `json:"list_status"`
	ExtendHourPrice  string `json:"extend_hour_price,omitempty"`
	ExtendHourHigh   string `json:"extend_hour_high,omitempty"`
	ExtendHourLow    string `json:"extend_hour_low,omitempty"`
	ExtendHourChange string `json:"extend_hour_change,omitempty"`
	ExtendHourVolume string `json:"extend_hour_volume,omitempty"`
	OvnPrice         string `json:"ovn_price,omitempty"`
	OvnHigh          string `json:"ovn_high,omitempty"`
	OvnLow           string `json:"ovn_low,omitempty"`
	OvnChange        string `json:"ovn_change,omitempty"`
	OvnVolume        string `json:"ovn_volume,omitempty"`
}

// --- Market Data: Bars ---

// Bar represents a single candlestick bar from /openapi/market-data/stock/bars.
// Bars endpoint returns a JSON array of Bar objects (newest-first).
// All numeric values are JSON strings.
type Bar struct {
	TickerID       string `json:"tickerId"`
	Symbol         string `json:"symbol"`
	Time           string `json:"time"` // ISO8601, e.g. "2026-07-09T04:00:00.000+0000"
	Open           string `json:"open"`
	Close          string `json:"close"`
	High           string `json:"high"`
	Low            string `json:"low"`
	Volume         string `json:"volume"`
	TradingSession string `json:"trading_session,omitempty"` // PRE, RTH, ATH, OVN
}

// --- Instruments ---

// Instrument represents a security from /openapi/instrument/stock/list.
// Instruments endpoint returns a JSON array of Instrument objects.
type Instrument struct {
	InstrumentID              string `json:"instrument_id"`
	Symbol                    string `json:"symbol"`
	Name                      string `json:"name"`
	ExchangeCode              string `json:"exchange_code"`
	Category                  string `json:"category"` // US_STOCK, US_ETF
	Status                    string `json:"status"`   // OC, CO, NT
	Fractionable              bool   `json:"fractionable"`
	Shortable                 bool   `json:"shortable"`
	Marginable                bool   `json:"marginable"`
	OvernightTradingSupported bool   `json:"overnight_trading_supported"`
	MarginRequirementLong     string `json:"margin_requirement_long"`
	MarginRequirementShort    string `json:"margin_requirement_short"`
	IntradayMarginLong        string `json:"intraday_margin_long"`
	IntradayMarginShort       string `json:"intraday_margin_short"`
	MaintenanceMarginLong     string `json:"maintenance_margin_long"`
	MaintenanceMarginShort    string `json:"maintenance_margin_short"`
	EasyToBorrow              bool   `json:"easy_to_borrow"`
	LotSize                   string `json:"lot_size"`
	Currency                  string `json:"currency"`
}

// --- Trading Orders ---

// NewOrder represents a single order for placement in /openapi/trade/order/place.
// All string fields are money/quantity values and must be formatted carefully.
type NewOrder struct {
	ClientOrderID         string `json:"client_order_id"`
	ComboType             string `json:"combo_type"`
	EntryType             string `json:"entrust_type"`
	InstrumentType        string `json:"instrument_type"`
	Market                string `json:"market"`
	OrderType             string `json:"order_type"`
	Side                  string `json:"side"`
	Symbol                string `json:"symbol"`
	TimeInForce           string `json:"time_in_force"`
	Quantity              string `json:"quantity,omitempty"`
	LimitPrice            string `json:"limit_price,omitempty"`
	StopPrice             string `json:"stop_price,omitempty"`
	SupportTradingSession string `json:"support_trading_session"`
}

// PlaceOrderRequest is the body for POST /openapi/trade/order/place.
type PlaceOrderRequest struct {
	AccountID string     `json:"account_id"`
	NewOrders []NewOrder `json:"new_orders"`
}

// PlaceOrderResponse is the response from /openapi/trade/order/place.
type PlaceOrderResponse struct {
	ClientOrderID string `json:"client_order_id"`
	OrderID       string `json:"order_id"`
}

// CancelOrderRequest is the body for POST /openapi/trade/order/cancel.
type CancelOrderRequest struct {
	AccountID     string `json:"account_id"`
	ClientOrderID string `json:"client_order_id"`
}

// ComboOrder represents the response from order query endpoints (open/history/detail).
type ComboOrder struct {
	ComboType     string      `json:"combo_type"`
	ComboOrderID  string      `json:"combo_order_id"`
	ClientOrderID string      `json:"client_order_id"` // May also appear at top level in detail response
	Orders        []OrderItem `json:"orders"`
}

// OrderItem represents a single order within a ComboOrder.
type OrderItem struct {
	ClientOrderID  string `json:"client_order_id"`
	OrderID        string `json:"order_id"`
	Symbol         string `json:"symbol"`
	Side           string `json:"side"`
	Status         string `json:"status"`
	OrderType      string `json:"order_type"`
	InstrumentType string `json:"instrument_type"`
	EntryType      string `json:"entrust_type"`
	TimeInForce    string `json:"time_in_force"`
	TotalQuantity  string `json:"total_quantity"`
	FilledQuantity string `json:"filled_quantity"`
	FilledPrice    string `json:"filled_price"`
	LimitPrice     string `json:"limit_price"`
	StopPrice      string `json:"stop_price"`
	PlaceTime      string `json:"place_time"`
	PlaceTimeAt    string `json:"place_time_at"`
	FilledTime     string `json:"filled_time"`
	FilledTimeAt   string `json:"filled_time_at"`
}

// --- Error Response ---

// ErrorResponse maps a Webull API error response.
type ErrorResponse struct {
	Message   string `json:"message"`
	ErrorCode string `json:"error_code"`
}

// Error implements the error interface for ErrorResponse.
func (e ErrorResponse) Error() string {
	if e.ErrorCode != "" {
		return "webull: " + e.Message + " (" + e.ErrorCode + ")"
	}
	return "webull: " + e.Message
}

// --- Helper Functions ---

// parseFloat parses a string as float64, returning 0 for empty strings.
func parseFloat(s string) float64 {
	if s == "" {
		return 0
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return f
}
