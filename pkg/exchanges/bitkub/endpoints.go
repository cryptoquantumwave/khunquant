package bitkub

const (
	baseURL = "https://api.bitkub.com"

	// Public market data
	endpointTicker  = "/api/v3/market/ticker"
	endpointDepth   = "/api/v3/market/depth"
	endpointSymbols = "/api/v3/market/symbols"
	endpointTrades  = "/api/v3/market/trades"

	// Private market data (authenticated GET)
	endpointMyOpenOrders = "/api/v3/market/my-open-orders"
	endpointOrderHistory = "/api/v3/market/my-order-history"
	endpointOrderInfo    = "/api/v3/market/order-info"

	// Private trading (authenticated POST)
	endpointBalances    = "/api/v3/market/balances"
	endpointPlaceBid    = "/api/v3/market/place-bid"
	endpointPlaceAsk    = "/api/v3/market/place-ask"
	endpointCancelOrder = "/api/v3/market/cancel-order"
)
