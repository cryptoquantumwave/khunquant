package line

import (
	"github.com/khunquant/khunquant/pkg/bus"
	"github.com/khunquant/khunquant/pkg/channels"
	"github.com/khunquant/khunquant/pkg/config"
)

func init() {
	channels.RegisterFactory("line", func(cfg *config.Config, b *bus.MessageBus) (channels.Channel, error) {
		return NewLINEChannel(cfg.Channels.LINE, b)
	})
}
