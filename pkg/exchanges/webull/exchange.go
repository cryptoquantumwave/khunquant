package webull

import (
	"context"

	"github.com/cryptoquantumwave/khunquant/pkg/exchanges"
)

// WebullExchange wraps webullAdapter and implements exchanges.PricedExchange
// for compatibility with portfolio tools.
type WebullExchange struct {
	adapter *webullAdapter
}

func (e *WebullExchange) Name() string { return Name }

func (e *WebullExchange) SupportedWalletTypes() []string {
	return e.adapter.SupportedWalletTypes()
}

func (e *WebullExchange) GetBalances(ctx context.Context) ([]exchanges.Balance, error) {
	bs, err := e.adapter.GetBalances(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]exchanges.Balance, len(bs))
	for i, b := range bs {
		out[i] = exchanges.Balance{Asset: b.Asset, Free: b.Free, Locked: b.Locked}
	}
	return out, nil
}

func (e *WebullExchange) GetWalletBalances(ctx context.Context, walletType string) ([]exchanges.WalletBalance, error) {
	bs, err := e.adapter.GetWalletBalances(ctx, walletType)
	if err != nil {
		return nil, err
	}
	out := make([]exchanges.WalletBalance, len(bs))
	for i, b := range bs {
		out[i] = exchanges.WalletBalance{
			Balance:    exchanges.Balance{Asset: b.Asset, Free: b.Free, Locked: b.Locked},
			WalletType: b.WalletType,
			Extra:      b.Extra,
		}
	}
	return out, nil
}

func (e *WebullExchange) FetchPrice(ctx context.Context, asset, quote string) (float64, error) {
	return e.adapter.FetchPrice(ctx, asset, quote)
}

// SupportedQuotes implements exchanges.QuoteLister.
func (e *WebullExchange) SupportedQuotes() []string {
	return []string{"USD"}
}
