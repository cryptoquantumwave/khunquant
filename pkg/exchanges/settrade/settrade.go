// Package settrade provides a broker adapter stub for the Settrade OpenAPI
// (Thailand Stock Exchange — SET), covering equities (EQ) and derivatives (TFEX).
//
// # Status: STUB — not yet implemented
//
// This package defines the structure and interface contracts for a future
// Settrade integration. All methods return ErrNotImplemented until the adapter
// is built (Phase 5).
//
// # API Reference
//
//   - OpenAPI portal: https://developer.settrade.com
//   - Base URL (prod): https://open-api.settrade.com
//   - No sandbox publicly available as of 2026 — use small lot sizes for testing.
//
// # Authentication Flow
//
// Settrade uses a custom ECDSA-signed login (NOT OAuth2):
//
//  1. Generate ECDSA P-256 key pair; store base64-encoded private key as app_secret.
//  2. POST /api/oam/v1/{broker_id}/broker-apps/ALGO/login
//     Body: { apiKey: app_id, params: { grant_type, loginId, ... }, signature: <DER hex>, timestamp: <unix ms> }
//     The signature covers the JSON-serialised params signed with SHA-256/ECDSA.
//  3. Response contains a Bearer JWT + refresh token.
//  4. POST /api/oam/v1/{broker_id}/broker-apps/ALGO/refresh-token to renew.
//  5. All subsequent requests: Authorization: Bearer <token>
//
// # Key Endpoints
//
//	POST /api/oam/v1/{broker_id}/broker-apps/ALGO/login          — ECDSA login
//	POST /api/oam/v1/{broker_id}/broker-apps/ALGO/refresh-token  — refresh JWT
//	GET  /api/um/v1/{broker_id}/user/me                          — current user info
//	GET  /api/um/v1/{broker_id}/user/accounts/{system_id}        — list portfolios
//	POST /api/um/v1/{broker_id}/user/verify-pin                  — verify trading PIN
//	GET  /api/eq/v1/{broker_id}/...                              — equity APIs
//	GET  /api/seosd/v1/{broker_id}/...                           — TFEX derivative APIs
//
// Real-time quotes use MQTT over WebSocket (paho.mqtt.golang).
// OHLCV is REST-polled.
//
// # Go Dependencies (Phase 5)
//
//   - crypto/ecdsa + crypto/elliptic (stdlib) — login signature
//   - github.com/eclipse/paho.mqtt.golang — MQTT WebSocket for live prices
//
// # SET Equity Constraints
//
//   - Board lot: 100 shares (round lot), 1 share (odd lot)
//   - Tick sizes: price tier–dependent (e.g. <2 THB → 0.01, 2–5 → 0.02, ≥1000 → 1.00)
//   - Settlement: T+2 (equities)
//   - Market hours: 10:00–12:30 and 14:00–16:30 ICT (Mon–Fri, excl. holidays)
//   - Order types: limit, ATO (at-the-open), ATC (at-the-close)
//   - OHLCV: intraday candles supported down to 1 minute (1m, 5m, 15m, 1h, 1d)
//
// # TODO (Phase 5)
//
//  1. Register Settrade OpenAPI developer app (requires Thai broker account)
//  2. Implement ECDSA P-256 login signing and token refresh
//  3. Implement MQTT WebSocket connection for real-time price streaming
//  4. Implement PortfolioProvider (cash balance, stock holdings)
//  5. Implement MarketDataProvider (ticker via MQTT, daily OHLCV via REST)
//  6. Implement TradingProvider (with lot-size + tick-size rounding)
//  7. Implement SET holiday calendar for GetMarketStatus
//  8. Add SettradeExchangeConfig to pkg/config/config.go
package settrade

import (
	"context"
	"errors"

	"github.com/khunquant/khunquant/pkg/config"
	"github.com/khunquant/khunquant/pkg/exchanges"
	"github.com/khunquant/khunquant/pkg/providers/broker"
)

// Name is the canonical provider identifier.
const Name = "settrade"

// ErrNotImplemented is returned by all stub methods.
var ErrNotImplemented = errors.New("settrade: adapter not yet implemented (Phase 5)")

// SettradeExchange is a placeholder that satisfies the bare exchanges.Exchange
// interface. Replace with a full implementation in Phase 5.
//
// TODO(phase5): replace stub with real HTTP + MQTT implementation.
type SettradeExchange struct {
	cfg SettradeAccount
}

// NewSettradeExchange creates a stub exchange.
func NewSettradeExchange(cfg SettradeAccount) (*SettradeExchange, error) {
	return &SettradeExchange{cfg: cfg}, nil
}

func (e *SettradeExchange) Name() string { return Name }

func (e *SettradeExchange) GetBalances(_ context.Context) ([]exchanges.Balance, error) {
	return nil, ErrNotImplemented
}

func (e *SettradeExchange) FetchPrice(_ context.Context, _, _ string) (float64, error) {
	return 0, ErrNotImplemented
}

// SettradeAccount holds credentials for a single Settrade OpenAPI account.
//
// TODO(phase5): add to pkg/config/config.go under ExchangesConfig.
type SettradeAccount struct {
	Name string `json:"name,omitempty"`

	// AppID is the Settrade OpenAPI application ID (from developer portal).
	AppID string `json:"app_id"`

	// AppSecret is the base64-encoded ECDSA P-256 private key (DER or PEM).
	// Generated during app registration. Used to sign login requests.
	AppSecret string `json:"app_secret"`

	// BrokerID is the broker code assigned by Settrade (e.g. "DEMO", "ABC").
	BrokerID string `json:"broker_id"`

	// AccountNo is the user's Settrade trading account number.
	AccountNo string `json:"account_no"`

	// PIN is the trading PIN verified before each order placement.
	// Consider prompting at runtime rather than storing in config.
	PIN string `json:"pin,omitempty"`

	// RefreshToken is persisted between runs after first successful login.
	RefreshToken string `json:"refresh_token,omitempty"`
}

// SettradeExchangeConfig is the top-level config block for Settrade.
//
// TODO(phase5): add as a field to pkg/config.ExchangesConfig.
type SettradeExchangeConfig struct {
	Enabled  bool               `json:"enabled"`
	Accounts []SettradeAccount  `json:"accounts,omitempty"`
}

// SettradeMarketDataProvider is a placeholder Provider for the category marker.
type SettradeMarketDataProvider struct {
	*SettradeExchange
}

func (p *SettradeMarketDataProvider) ID() string                        { return Name }
func (p *SettradeMarketDataProvider) Category() broker.AssetCategory    { return broker.CategoryStock }
func (p *SettradeMarketDataProvider) GetMarketStatus(_ context.Context, _ string) (broker.MarketStatus, error) {
	// TODO(phase5): check SET session hours (10:00–12:30 and 14:30–16:30 ICT) and holiday calendar
	return broker.MarketUnknown, ErrNotImplemented
}

// Ensure SettradeExchange satisfies exchanges.Exchange at compile time.
var _ exchanges.Exchange = (*SettradeExchange)(nil)

// Ensure SettradeMarketDataProvider satisfies broker.Provider at compile time.
var _ broker.Provider = (*SettradeMarketDataProvider)(nil)

// suppress unused import
var _ = config.ExchangeAccount{}
