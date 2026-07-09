package webull

const (
	Name        = "webull"
	defaultHost = "https://api.webull.com"

	// TODO: verify exact US OpenAPI host/paths against developer.webull.com
	// The following paths are best-guesses and must be verified against live documentation.

	// Account and positions endpoints
	endpointAccount   = "/v2/trading/accounts/%s"
	endpointBalances  = "/v2/trading/accounts/%s/balances"
	endpointPositions = "/v2/trading/accounts/%s/positions"

	// Market data endpoints
	endpointQuote = "/v2/market/quotes/%s"
	endpointBars  = "/v2/market/bars"

	// Order book endpoint (not exposed via OpenAPI, but included for completeness)
	endpointOrderBook = "/v2/market/orderbook/%s"
)
