package tools

import (
	"context"

	"github.com/cryptoquantumwave/khunquant/pkg/journal"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// journalSendOptions are the place options of a journaled order step (approval.order,
// dca.execute, the delta-neutral legs): the write-ahead mark right before the send, and the
// knowledge that an earlier attempt marked. With a mark an empty lookup is never believed,
// because the venue's lookup can lag by any amount; without one no earlier attempt sent
// anything, so an empty lookup is believed as it always was. Outside a journal step (no
// Effect on ctx) there are no options.
func journalSendOptions(ctx context.Context) []broker.PlaceOption {
	eff := journal.EffectFrom(ctx)
	if eff == nil {
		return nil
	}
	return []broker.PlaceOption{broker.WithBeforeSend(eff.Mark), broker.WithPriorSend(eff.Prior)}
}
