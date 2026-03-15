package feishu

import (
	"github.com/khunquant/khunquant/pkg/bus"
	"github.com/khunquant/khunquant/pkg/channels"
	"github.com/khunquant/khunquant/pkg/config"
)

func init() {
	channels.RegisterFactory("feishu", func(cfg *config.Config, b *bus.MessageBus) (channels.Channel, error) {
		return NewFeishuChannel(cfg.Channels.Feishu, b)
	})
}
