package simchat

import (
	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/channels"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

func init() {
	channels.RegisterFactory(channelName, func(cfg *config.Config, b *bus.MessageBus) (channels.Channel, error) {
		return New(cfg, b)
	})
}
