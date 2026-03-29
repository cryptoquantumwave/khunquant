package settrade

import (
	"fmt"

	"github.com/khunquant/khunquant/pkg/config"
	"github.com/khunquant/khunquant/pkg/exchanges"
)

func init() {
	exchanges.RegisterFactory(Name, func(cfg *config.Config) (exchanges.Exchange, error) {
		acc, ok := cfg.Exchanges.Settrade.ResolveAccount("")
		if !ok {
			return nil, fmt.Errorf("%s: no accounts configured", Name)
		}
		adapter, err := newBrokerAdapter(acc)
		if err != nil {
			return nil, err
		}
		return &SettradeExchange{adapter: adapter}, nil
	})
	exchanges.RegisterAccountFactory(Name, func(cfg *config.Config, accountName string) (exchanges.Exchange, error) {
		acc, ok := cfg.Exchanges.Settrade.ResolveAccount(accountName)
		if !ok {
			var names []string
			for i, a := range cfg.Exchanges.Settrade.Accounts {
				n := a.Name
				if n == "" {
					n = fmt.Sprintf("%d", i+1)
				}
				names = append(names, n)
			}
			return nil, exchanges.ErrAccountNotFound(Name, accountName, names)
		}
		adapter, err := newBrokerAdapter(acc)
		if err != nil {
			return nil, err
		}
		return &SettradeExchange{adapter: adapter}, nil
	})
}
