// Package sandbox provides developer sandbox mode, which intercepts all
// exchange/broker HTTP traffic and serves mock responses in each venue's real
// wire format. This allows agent tools and skills to be exercised end-to-end
// without touching a live exchange.
//
// # Address Contract
//
// The sandbox server listens on 127.0.0.1:<port> and rewrites all exchange
// requests to a canonical address:
//
//	http://127.0.0.1:<port>/__sbx__/<venue>/<original-host>/<original-path>
//
// Examples:
//   - https://fapi.binance.com/fapi/v1/positionRisk
//     → http://127.0.0.1:P/__sbx__/binance/fapi.binance.com/fapi/v1/positionRisk
//   - https://www.okx.com/api/v5/account/balance
//     → http://127.0.0.1:P/__sbx__/okx/www.okx.com/api/v5/account/balance
//   - https://api.bitkub.com/api/v3/market/balances
//     → http://127.0.0.1:P/__sbx__/bitkub/api.bitkub.com/api/v3/market/balances
//
// Query strings, methods, headers, and request bodies are preserved verbatim.
// The sandbox does NOT carry query strings in the path segment; they are preserved
// in the URL query component (RawQuery).
//
// # Fail-Closed Behavior
//
// Requests that reach the sandbox server but match no fixture return a non-2xx error
// with a body like:
//
//	sandbox: no fixture configured for GET /api/v5/account/balance (venue=okx)
//
// This is deliberate: a developer cannot accidentally send a real request to a live
// exchange and think they are sandboxed. The sandbox never passes through to the
// real API, and it never synthesizes a plausible but fake response.
//
// # Fixtures and File Layout
//
// Fixtures live under <fixturesDir>/<venue>/ as JSON files. Each fixture file
// contains a list of fixture entries. A fixture entry specifies:
//   - Match criteria: HTTP method, path pattern (prefix match), optional query parameters
//   - HTTP response status code
//   - Response body (raw JSON, preserved byte-for-byte)
//   - Optional response headers
//
// # Query Matching
//
// Fixtures can constrain matches on query parameters via an optional "query" field.
// If a fixture has no query field (or it is empty), the fixture matches any request
// to that method and path prefix, regardless of query parameters. If a fixture has
// a query field with key-value pairs, ALL of those keys must be present in the request
// with exact-matching values; unlisted query parameters in the request are ignored.
//
// Example: A fixture with query: {"instId": "BTC-USDT"} matches requests with
// ?instId=BTC-USDT&extra=1 but not ?instId=ETH-USDT or requests without instId.
//
// When multiple fixtures are eligible for a request, the most specific wins.
// Specificity is determined by: (1) number of satisfied query constraints (higher is
// better), then (2) length of path prefix (longer is better). If multiple fixtures
// have the same specificity score, the one appearing first in the fixture file
// (or the first to be added via SetFixtures) is returned. This ensures deterministic,
// reproducible matching.
//
// If a request matches the method and path of a fixture but fails to satisfy its
// query constraints, the sandbox returns an error message explicitly stating that
// fixtures exist for the path but the query did not match, prompting the developer
// to add or update a query-scoped fixture.
//
// # Stateful Simulation
//
// The Server accepts a Responder chain that can intercept requests before the
// static fixture store is consulted. This allows higher-level layers (e.g., T5)
// to implement stateful mutation (place order, close position, etc.) while
// falling through to fixtures for reads. Use Server.SetResponder to register
// a responder, or pass multiple responders to BuildRouter; the first responder
// that returns true short-circuits the fixture store.
//
// StatefulSimulator implements the Responder interface to provide in-memory
// account state management for Binance and OKX futures trading. It simulates:
//
// BINANCE FUTURES (/fapi/v1):
//   - POST /fapi/v1/order — creates orders, updates balances and positions
//   - GET /fapi/v1/order — fetches order status
//   - DELETE /fapi/v1/order — cancels open orders
//   - GET /fapi/v1/openOrders — lists open orders
//   - GET /fapi/v1/positionRisk — lists positions
//   - GET /fapi/v1/account — returns account balances
//   - POST /fapi/v1/leverage — sets leverage
//   - GET /fapi/v1/exchangeInfo — returns market metadata
//
// OKX PERPETUALS (/api/v5):
//   - POST /api/v5/trade/order — creates orders, updates balances and positions
//   - GET /api/v5/trade/order — fetches order status
//   - POST /api/v5/trade/cancel-order — cancels open orders
//   - GET /api/v5/trade/orders-pending — lists open orders
//   - GET /api/v5/account/positions — lists positions
//   - GET /api/v5/account/balance — returns account balances
//   - POST /api/v5/account/set-leverage — sets leverage
//   - GET /api/v5/public/instruments — returns market metadata
//
// SCOPE AND LIMITATIONS:
// The simulator respects the exact semantics documented in CLAUDE.md's
// "Exchange API Pitfalls" section:
//   - Contract-based amounts: Binance and OKX quantity/sz fields are in contracts,
//     not base currency. Market metadata includes contractSize. Order fills and
//     position sizes use contract semantics.
//   - Order side validation: side must be "buy"/"sell" (not "long"/"short").
//     posSide (OKX) or positionSide (Binance) is a separate field for position direction.
//   - Balance ledger: balance decreases on order placement and is restored on cancellation.
//     Actual margin and liquidation are NOT simulated (simplification).
//
// Does NOT simulate:
//   - Exchange fees or maker/taker rebates
//   - Funding payments (perpetual swap interest)
//   - Slippage or market impact
//   - Partial fills (orders assume immediate 100% fill or cancel)
//   - Liquidation, risk events, or negative balance protection
//   - Minimum notional, margin ratios, or other risk constraints
//   - Multi-leg order types or advanced order features
//   - Order cancellation race conditions (assumes synchronous state updates)
//
// For other venues (bitkub, binanceth, settrade, webull), unmodeled endpoints
// fall through to the static fixture store. Requests that match neither the
// simulator nor any fixture return a non-2xx error (fail-closed behavior).
package sandbox
