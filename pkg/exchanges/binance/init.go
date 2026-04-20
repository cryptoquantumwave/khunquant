package binance

import (
	"fmt"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/exchanges"
)

func init() {
	exchanges.RegisterFactory(Name, func(cfg *config.Config) (exchanges.Exchange, error) {
		acc, ok := cfg.Exchanges.Binance.ResolveAccount("")
		if !ok {
			return nil, fmt.Errorf("%s: no accounts configured", Name)
		}
		return NewBinanceExchange(acc, cfg.Exchanges.Binance.Testnet)
	})
	exchanges.RegisterAccountFactory(Name, func(cfg *config.Config, accountName string) (exchanges.Exchange, error) {
		acc, ok := cfg.Exchanges.Binance.ResolveAccount(accountName)
		if !ok {
			var names []string
			for i, a := range cfg.Exchanges.Binance.Accounts {
				n := a.Name
				if n == "" {
					n = fmt.Sprintf("%d", i+1)
				}
				names = append(names, n)
			}
			return nil, exchanges.ErrAccountNotFound(Name, accountName, names)
		}
		return NewBinanceExchange(acc, cfg.Exchanges.Binance.Testnet)
	})
}
