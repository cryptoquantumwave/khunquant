package whatsapp

import (
	"github.com/khunquant/khunquant/pkg/bus"
	"github.com/khunquant/khunquant/pkg/channels"
	"github.com/khunquant/khunquant/pkg/config"
)

func init() {
	channels.RegisterFactory("whatsapp", func(cfg *config.Config, b *bus.MessageBus) (channels.Channel, error) {
		return NewWhatsAppChannel(cfg.Channels.WhatsApp, b)
	})
}
