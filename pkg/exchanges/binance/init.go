package binance

import (
	"github.com/khunquant/khunquant/pkg/config"
	"github.com/khunquant/khunquant/pkg/exchanges"
)

func init() {
	exchanges.RegisterFactory("binance", func(cfg *config.Config) (exchanges.Exchange, error) {
		return NewBinanceExchange(cfg)
	})
}
