package settrade

import (
	"context"

	"github.com/khunquant/khunquant/pkg/exchanges"
)

// SettradeExchange wraps SettradeFullAdapter and implements exchanges.PricedExchange
// so it works with the portfolio tools (list_portfolios, get_assets_list, etc.).
type SettradeExchange struct {
	adapter *SettradeFullAdapter
}

func (e *SettradeExchange) Name() string { return Name }

func (e *SettradeExchange) SupportedWalletTypes() []string {
	return e.adapter.SupportedWalletTypes()
}

func (e *SettradeExchange) GetBalances(ctx context.Context) ([]exchanges.Balance, error) {
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

func (e *SettradeExchange) GetWalletBalances(ctx context.Context, walletType string) ([]exchanges.WalletBalance, error) {
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

func (e *SettradeExchange) FetchPrice(ctx context.Context, asset, quote string) (float64, error) {
	return e.adapter.FetchPrice(ctx, asset, quote)
}

// SupportedQuotes implements exchanges.QuoteLister.
func (e *SettradeExchange) SupportedQuotes() []string {
	return []string{"THB"}
}
