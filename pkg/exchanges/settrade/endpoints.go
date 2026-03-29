package settrade

const (
	baseURL       = "https://open-api.settrade.com"
	marketBaseURL = "https://marketapi.settrade.com"

	// --- Equity (SEOS v3 / v4) ---
	// fmt.Sprintf(endpoint, brokerID, accountNo[, orderNo])
	endpointEQAccountInfo = "/api/seos/v3/%s/accounts/%s/account-info"
	endpointEQPortfolio   = "/api/seos/v3/%s/accounts/%s/portfolios"
	endpointEQOrders      = "/api/seos/v3/%s/accounts/%s/orders"
	endpointEQOrder       = "/api/seos/v3/%s/accounts/%s/orders/%s"
	endpointEQOrderCancel  = "/api/seos/v3/%s/accounts/%s/orders/%s/cancel"
	endpointEQOrderChange  = "/api/seos/v3/%s/accounts/%s/orders/%s/change"
	endpointEQCancelOrders = "/api/seos/v3/%s/accounts/%s/cancel"
	endpointEQTrades       = "/api/seos/v4/%s/accounts/%s/trades"

	// --- Market Data (separate host: marketapi.settrade.com) ---
	// fmt.Sprintf(endpoint, brokerID, symbol)
	endpointMarketQuote     = "/api/marketdata/v3/%s/quote/%s"
	endpointMarketCandlestick = "/api/techchart/v3/%s/candlesticks"
)
