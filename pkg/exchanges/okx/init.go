package okx

import (
	"fmt"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/exchanges"
)

func init() {
	exchanges.RegisterFactory(Name, func(cfg *config.Config) (exchanges.Exchange, error) {
		acc, ok := cfg.Exchanges.OKX.ResolveAccount("")
		if !ok {
			return nil, fmt.Errorf("%s: no accounts configured", Name)
		}
		return NewOKXExchange(acc, cfg.Exchanges.OKX.Testnet)
	})
	exchanges.RegisterAccountFactory(Name, func(cfg *config.Config, accountName string) (exchanges.Exchange, error) {
		acc, ok := cfg.Exchanges.OKX.ResolveAccount(accountName)
		if !ok {
			var names []string
			for i, a := range cfg.Exchanges.OKX.Accounts {
				n := a.Name
				if n == "" {
					n = fmt.Sprintf("%d", i+1)
				}
				names = append(names, n)
			}
			return nil, exchanges.ErrAccountNotFound(Name, accountName, names)
		}
		return NewOKXExchange(acc, cfg.Exchanges.OKX.Testnet)
	})
}
