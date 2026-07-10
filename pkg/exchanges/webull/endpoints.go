package webull

const (
	Name = "webull"

	// Host routing by environment
	// Prod: api.webull.com
	// Sandbox/UAT: us-openapi-alb.uat.webullbroker.com (not api.sandbox.webull.com — that doesn't work with shared creds)
	prodHost = "api.webull.com"
	uatHost  = "us-openapi-alb.uat.webullbroker.com"

	// Authentication endpoints
	endpointTokenCreate = "/openapi/auth/token/create"
	endpointTokenCheck  = "/openapi/auth/token/check"

	// Account and portfolio endpoints
	endpointAccountList = "/openapi/account/list"
	endpointBalance     = "/openapi/assets/balance"
	endpointPositions   = "/openapi/assets/positions"

	// Instrument endpoints
	endpointInstrumentStockList = "/openapi/instrument/stock/list"

	// Market data endpoints (equity/ETF)
	endpointSnapshot = "/openapi/market-data/stock/snapshot"
	endpointBars     = "/openapi/market-data/stock/bars"

	// Market data endpoints (options)
	endpointOptionSnapshot = "/openapi/market-data/option/snapshot"
	endpointOptionBars     = "/openapi/market-data/option/bars"

	// Trading endpoints (add as constants for future implementation)
	endpointOrderPlace   = "/openapi/trade/order/place"
	endpointOrderPreview = "/openapi/trade/order/preview"
	endpointOrderCancel  = "/openapi/trade/order/cancel"
	endpointOrderReplace = "/openapi/trade/order/replace"
	endpointOrderOpen    = "/openapi/trade/order/open"
	endpointOrderHistory = "/openapi/trade/order/history"
	endpointOrderDetail  = "/openapi/trade/order/detail"
)

// HostForEnvironment returns the API host for the given environment.
func HostForEnvironment(environment string) string {
	switch environment {
	case "uat", "sandbox":
		return uatHost
	default:
		return prodHost
	}
}

// BaseURLForEnvironment returns the full base URL (https://) for the given environment.
func BaseURLForEnvironment(environment string) string {
	host := HostForEnvironment(environment)
	return "https://" + host
}
