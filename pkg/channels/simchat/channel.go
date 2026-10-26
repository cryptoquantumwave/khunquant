// Package simchat is the simulated chat channel of a sandbox instance: a real channel
// adapter, so lab chat goes through BaseChannel.HandleMessage, the control path, routing,
// sessions and the manager's delivery pipeline exactly as Telegram does. It has no
// transport, and it exists only in a process under the sandbox lock.
package simchat

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/channels"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

const (
	channelName = "simchat"
	// maxMessageLength is Telegram's, so the manager splits a long message as in production.
	maxMessageLength = 4096
)

// Channel is the adapter. It is built per generation of services and attached to the
// process-lifetime World, which holds the personas, the outbox and the channel state.
type Channel struct {
	*channels.BaseChannel
	world atomic.Pointer[World]
}

var (
	_ channels.IDReportingSender = (*Channel)(nil)
	_ channels.MessageEditor     = (*Channel)(nil)
	_ channels.ActionRenderer    = (*Channel)(nil)
)

// New builds the channel. It refuses unless the process is locked and the config
// enables it (the second gate; the manager checks both before it asks for one).
func New(cfg *config.Config, b *bus.MessageBus) (*Channel, error) {
	if !sandbox.Locked() {
		return nil, errors.New("simchat: only available in a process under the sandbox lock")
	}
	sc := cfg.Channels.SimChat
	if !sc.Enabled {
		return nil, errors.New("simchat: channels.simchat is not enabled")
	}
	return &Channel{BaseChannel: channels.NewBaseChannel(channelName, sc, b, sc.AllowFrom,
		channels.WithMaxMessageLength(maxMessageLength), channels.WithGroupTrigger(sc.GroupTrigger))}, nil
}

// Attach binds the channel to the World; the lab's ops reach the channel through it.
// Start fails closed without it.
func (c *Channel) Attach(w *World) {
	c.world.Store(w)
	w.attach(c)
}

// Start fails without an attached World.
func (c *Channel) Start(context.Context) error {
	if c.world.Load() == nil {
		return errors.New("simchat: no world attached")
	}
	c.SetRunning(true)
	return nil
}

// Stop detaches the channel from the World, but only if it is still the attached one:
// a late Stop of an old generation must not undo a reload's Attach.
func (c *Channel) Stop(context.Context) error {
	c.SetRunning(false)
	if w := c.world.Load(); w != nil {
		w.detach(c)
	}
	return nil
}

// Send delegates to SendWithIDs, the one code path.
func (c *Channel) Send(ctx context.Context, msg bus.OutboundMessage) error {
	_, err := c.SendWithIDs(ctx, msg)
	return err
}

// SendWithIDs captures msg in the outbox under a new message ID, or refuses it as the
// channel's current state says.
func (c *Channel) SendWithIDs(ctx context.Context, msg bus.OutboundMessage) ([]string, error) {
	w := c.world.Load()
	if w == nil || !c.IsRunning() {
		return nil, fmt.Errorf("simchat: %w", channels.ErrNotRunning)
	}
	return w.deliver(ctx, msg)
}

// EditMessage replaces the text of a message the channel sent and removes its buttons.
func (c *Channel) EditMessage(_ context.Context, chatID, messageID, content string) error {
	w := c.world.Load()
	if w == nil || !c.IsRunning() {
		return fmt.Errorf("simchat: %w", channels.ErrNotRunning)
	}
	return w.edit(chatID, messageID, content)
}

// SupportsActions says buttons are rendered natively (the manager adds no text footer).
func (c *Channel) SupportsActions() bool { return true }
