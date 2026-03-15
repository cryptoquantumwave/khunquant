package exchanges

import "context"

// Balance represents the balance of a single asset on an exchange.
type Balance struct {
	Asset  string
	Free   float64
	Locked float64
}

// WalletBalance extends Balance with wallet-type metadata and optional extra fields.
type WalletBalance struct {
	Balance
	WalletType string
	Extra      map[string]string // additional fields (e.g. "unrealized_pnl", "borrowed")
}

// Exchange is the interface that all exchange adapters must implement.
type Exchange interface {
	Name() string
	GetBalances(ctx context.Context) ([]Balance, error)
}

// WalletExchange is an optional extension for exchanges that support multiple wallet types.
type WalletExchange interface {
	Exchange
	SupportedWalletTypes() []string
	GetWalletBalances(ctx context.Context, walletType string) ([]WalletBalance, error)
}

// PricedExchange extends WalletExchange with asset price lookup.
// FetchPrice returns the last traded price of asset in terms of quote currency (e.g. "USDT").
// Returns (0, nil) if asset IS the quote currency or a recognized USD-equivalent stablecoin.
// Returns (0, error) if price cannot be determined.
type PricedExchange interface {
	WalletExchange
	FetchPrice(ctx context.Context, asset, quote string) (float64, error)
}
