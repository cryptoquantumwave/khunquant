package webull

import (
	"fmt"
	"strconv"
)

// --- Authentication ---

// TokenResponse maps the /openapi/auth/token/create and
// /openapi/auth/token/check endpoint responses.
type TokenResponse struct {
	Token   string `json:"token"`
	Expires int64  `json:"expires"` // unix milliseconds
	Status  string `json:"status"`  // TokenStatusNormal, TokenStatusPending, TokenStatusInvalid, TokenStatusExpired
}

// Token status values returned by token/create and token/check. NORMAL is
// the only status usable for authenticated requests; PENDING means the user
// must approve the login in the Webull mobile app (there is no API to submit
// an SMS/OTP code — approval only happens in-app).
const (
	TokenStatusNormal  = "NORMAL"
	TokenStatusPending = "PENDING"
	TokenStatusInvalid = "INVALID"
	TokenStatusExpired = "EXPIRED"
)

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

// OrderLeg represents a single leg in an options order.
type OrderLeg struct {
	Side             string `json:"side"`
	Quantity         string `json:"quantity"`
	Symbol           string `json:"symbol"`
	StrikePrice      string `json:"strike_price,omitempty"`
	OptionExpireDate string `json:"option_expire_date,omitempty"` // yyyy-MM-dd
	InstrumentType   string `json:"instrument_type,omitempty"`
	OptionType       string `json:"option_type,omitempty"` // CALL or PUT
	Market           string `json:"market,omitempty"`
}

// NewOrder represents a single order for placement in /openapi/trade/order/place.
// All string fields are money/quantity values and must be formatted carefully.
type NewOrder struct {
	ClientOrderID         string     `json:"client_order_id"`
	ComboType             string     `json:"combo_type"`
	EntryType             string     `json:"entrust_type"`
	InstrumentType        string     `json:"instrument_type"`
	Market                string     `json:"market"`
	OrderType             string     `json:"order_type"`
	Side                  string     `json:"side"`
	Symbol                string     `json:"symbol"`
	TimeInForce           string     `json:"time_in_force"`
	Quantity              string     `json:"quantity,omitempty"`
	LimitPrice            string     `json:"limit_price,omitempty"`
	StopPrice             string     `json:"stop_price,omitempty"`
	SupportTradingSession string     `json:"support_trading_session,omitempty"`
	OptionStrategy        string     `json:"option_strategy,omitempty"`
	Legs                  []OrderLeg `json:"legs,omitempty"`
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
	ClientOrderID  string             `json:"client_order_id"`
	OrderID        string             `json:"order_id"`
	Symbol         string             `json:"symbol"`
	Side           string             `json:"side"`
	Status         string             `json:"status"`
	OrderType      string             `json:"order_type"`
	InstrumentType string             `json:"instrument_type"`
	EntryType      string             `json:"entrust_type"`
	TimeInForce    string             `json:"time_in_force"`
	TotalQuantity  string             `json:"total_quantity"`
	FilledQuantity string             `json:"filled_quantity"`
	FilledPrice    string             `json:"filled_price"`
	LimitPrice     string             `json:"limit_price"`
	StopPrice      string             `json:"stop_price"`
	PlaceTime      string             `json:"place_time"`
	PlaceTimeAt    string             `json:"place_time_at"`
	FilledTime     string             `json:"filled_time"`
	FilledTimeAt   string             `json:"filled_time_at"`
	Legs           []OrderLegResponse `json:"legs,omitempty"`
}

// OrderLegResponse is a single leg echoed in an order open/detail/history response.
// For OPTION orders it carries the contract fields needed to rebuild the OCC symbol.
type OrderLegResponse struct {
	ID               string `json:"id"`
	Symbol           string `json:"symbol"` // underlying
	Side             string `json:"side"`
	Quantity         string `json:"quantity"`
	OptionType       string `json:"option_type"`     // CALL|PUT
	OptionCategory   string `json:"option_category"` // AMERICAN|EUROPEAN
	StrikePrice      string `json:"strike_price"`
	OptionExpireDate string `json:"option_expire_date"` // yyyy-MM-dd
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

// --- Options Market Data ---

// OptionSnapshotDTO represents a single option snapshot from /openapi/market-data/option/snapshot.
// All numeric values are JSON strings.
type OptionSnapshotDTO struct {
	Symbol        string `json:"symbol"` // Encoded OCC symbol
	InstrumentID  string `json:"instrument_id"`
	Price         string `json:"price"`
	Bid           string `json:"bid"`
	Ask           string `json:"ask"`
	BidSize       string `json:"bid_size"`
	AskSize       string `json:"ask_size"`
	Open          string `json:"open"`
	High          string `json:"high"`
	Low           string `json:"low"`
	Close         string `json:"close"`
	PreClose      string `json:"pre_close"`
	Change        string `json:"change"`
	ChangeRatio   string `json:"change_ratio"`
	Delta         string `json:"delta"`
	Gamma         string `json:"gamma"`
	Theta         string `json:"theta"`
	Vega          string `json:"vega"`
	Rho           string `json:"rho"`
	ImpVol        string `json:"imp_vol"`
	OpenInterest  string `json:"open_interest"`
	Volume        string `json:"volume"`
	StrikePrice   string `json:"strike_price"`
	LastTradeTime int64  `json:"last_trade_time"` // unix milliseconds
	QuoteTime     int64  `json:"quote_time"`      // unix milliseconds
}

// OptionBarDTO is a single candlestick bar from /openapi/market-data/option/bars.
// The option-bars payload is structurally identical to the stock Bar (its Symbol
// is the encoded OCC symbol), so it is an alias — both share barsToOHLCV.
type OptionBarDTO = Bar

// --- Helper Functions ---

// parseFloat parses a string as float64, returning 0 for empty or malformed
// strings. Use this only where a missing/zero value is legitimate (e.g. an
// illiquid contract's bid). For fields where a malformed value must not silently
// become 0 (e.g. a quote price), use parseFloatStrict.
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

// parseFloatStrict parses a string as float64, returning an error for empty or
// unparseable input. A present, well-formed "0" parses to (0, nil). Use this for
// fields where a silent 0-on-error would corrupt downstream valuation/sizing.
func parseFloatStrict(s string) (float64, error) {
	if s == "" {
		return 0, fmt.Errorf("empty value")
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid float %q: %w", s, err)
	}
	return f, nil
}
