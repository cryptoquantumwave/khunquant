package telegram

import (
	"github.com/khunquant/khunquant/pkg/bus"
	"github.com/khunquant/khunquant/pkg/channels"
	"github.com/khunquant/khunquant/pkg/config"
)

func init() {
	channels.RegisterFactory("telegram", func(cfg *config.Config, b *bus.MessageBus) (channels.Channel, error) {
		return NewTelegramChannel(cfg, b)
	})
}
