package teamswebhook

import (
	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/channels"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

func init() {
	channels.RegisterFactory("teams_webhook", func(cfg *config.Config, b *bus.MessageBus) (channels.Channel, error) {
		return NewTeamsWebhookChannel(cfg.Channels.TeamsWebhook, b)
	})
}
