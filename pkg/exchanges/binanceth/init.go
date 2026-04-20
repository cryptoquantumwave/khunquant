package binanceth

import (
	"fmt"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/exchanges"
)

func init() {
	exchanges.RegisterFactory(Name, func(cfg *config.Config) (exchanges.Exchange, error) {
		acc, ok := cfg.Exchanges.BinanceTH.ResolveAccount("")
		if !ok {
			return nil, fmt.Errorf("%s: no accounts configured", Name)
		}
		return NewBinanceTHExchange(acc)
	})
	exchanges.RegisterAccountFactory(Name, func(cfg *config.Config, accountName string) (exchanges.Exchange, error) {
		acc, ok := cfg.Exchanges.BinanceTH.ResolveAccount(accountName)
		if !ok {
			var names []string
			for i, a := range cfg.Exchanges.BinanceTH.Accounts {
				n := a.Name
				if n == "" {
					n = fmt.Sprintf("%d", i+1)
				}
				names = append(names, n)
			}
			return nil, exchanges.ErrAccountNotFound(Name, accountName, names)
		}
		return NewBinanceTHExchange(acc)
	})
}
