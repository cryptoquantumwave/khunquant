package discord

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/channels"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
)

// Buttons.
//
// A message with bus.Actions carries buttons (an ActionsRow of Button components)
// on its last message. A tap arrives as an INTERACTION_CREATE of type
// message component, which is a control input exactly like a typed /approve: it
// goes to the channel's control router (pkg/control) and to nothing else. It never
// becomes an inbound message, so no model ever sees it.
//
// The authority of a tap is the interaction's own user, as Discord reports it:
// Member.User in a guild, User in a DM. Nothing in the custom_id names a sender,
// a chat or an outcome: it carries an action and an opaque payload (the shared
// kq1|<action>|<payload> codec), and the approval service checks the payload
// against the stored proposal.

const (
	// maxCustomIDBytes is Discord's limit on a component's custom_id.
	maxCustomIDBytes = 100
	// maxButtonsPerRow and maxActionRows are Discord's limits on an ActionsRow and
	// on the rows of a message.
	maxButtonsPerRow = 5
	maxActionRows    = 5
	// maxButtonLabelRunes is Discord's limit on a button label.
	maxButtonLabelRunes = 80

	// interactionAckDeadline is how long the control path gets before the tap is
	// acknowledged anyway. Discord gives 3 seconds to respond to an interaction;
	// the control path normally answers in milliseconds, in which case its words
	// are the response itself, and otherwise they follow as an edit or a followup.
	interactionAckDeadline = 2500 * time.Millisecond
	// followupTimeout bounds the calls that finish a tap after the early ack.
	followupTimeout = 10 * time.Second
	// seenTapTTL is how long an interaction ID is remembered: Discord's interaction
	// token lives 15 minutes, and a redelivery arrives within seconds.
	seenTapTTL = 15 * time.Minute
)

// SupportsActions implements channels.ActionRenderer: bus.Action buttons are
// shown as message components. The text fallback of the actions stays in the
// message body (see SendWithIDs).
func (c *DiscordChannel) SupportsActions() bool { return true }

// buildComponents builds the button rows of a message's actions. It is all or
// nothing: one action that cannot be encoded (or too many) fails the lot, because
// Approve without Reject is worse than neither. The caller sends the message
// without buttons then; the text fallback is in the message body.
func buildComponents(actions []bus.Action) ([]discordgo.MessageComponent, error) {
	if len(actions) == 0 {
		return nil, nil
	}
	if len(actions) > maxButtonsPerRow*maxActionRows {
		return nil, fmt.Errorf("%d actions, the limit is %d", len(actions), maxButtonsPerRow*maxActionRows)
	}
	seen := make(map[string]bool, len(actions))
	buttons := make([]discordgo.MessageComponent, 0, len(actions))
	for _, a := range actions {
		data, err := channels.EncodeCallbackData(a, maxCustomIDBytes)
		if err != nil {
			return nil, err
		}
		if seen[data] {
			// Discord refuses a message whose custom_ids repeat.
			return nil, fmt.Errorf("action %q appears twice", a.ID)
		}
		seen[data] = true
		label := strings.TrimSpace(a.Label)
		if label == "" {
			label = a.ID
		}
		buttons = append(buttons, discordgo.Button{
			Label:    channels.TruncateRunes(label, maxButtonLabelRunes),
			Style:    buttonStyle(a.Style),
			CustomID: data,
		})
	}
	var rows []discordgo.MessageComponent
	for len(buttons) > 0 {
		n := min(len(buttons), maxButtonsPerRow)
		rows = append(rows, discordgo.ActionsRow{Components: buttons[:n]})
		buttons = buttons[n:]
	}
	return rows, nil
}

func buttonStyle(style string) discordgo.ButtonStyle {
	switch style {
	case "primary":
		return discordgo.PrimaryButton
	case "success":
		return discordgo.SuccessButton
	case "danger":
		return discordgo.DangerButton
	}
	return discordgo.SecondaryButton
}

// seenTaps remembers the interactions already handled, so that a delivery the
// gateway repeats (or a second registration of the handler) is not run twice.
type seenTaps struct {
	mu sync.Mutex
	m  map[string]time.Time
}

// first reports whether id has not been seen, and remembers it. An empty id
// cannot be told from another and counts as new.
func (s *seenTaps) first(id string, now time.Time) bool {
	if id == "" {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = make(map[string]time.Time)
	}
	for k, at := range s.m {
		if now.Sub(at) > seenTapTTL {
			delete(s.m, k)
		}
	}
	if _, dup := s.m[id]; dup {
		return false
	}
	s.m[id] = now
	return true
}

// runCtx is the channel's context, or a background one before Start.
func (c *DiscordChannel) runCtx() context.Context {
	if c.ctx == nil {
		return context.Background()
	}
	return c.ctx
}

// interactionUser is the user who pressed the button: Member.User in a guild,
// User in a DM. It is nil when Discord named nobody.
func interactionUser(i *discordgo.Interaction) *discordgo.User {
	if i.Member != nil && i.Member.User != nil {
		return i.Member.User
	}
	return i.User
}

// handleInteraction handles one tap. Every tap is acknowledged, whatever happens
// to it, because a client keeps its spinner until the interaction is answered.
func (c *DiscordChannel) handleInteraction(s *discordgo.Session, ic *discordgo.InteractionCreate) {
	if ic == nil || ic.Interaction == nil || ic.Type != discordgo.InteractionMessageComponent {
		return
	}
	i := ic.Interaction
	if !c.taps.first(i.ID, time.Now()) {
		logger.DebugC("discord", "Dropped a repeated interaction")
		return
	}
	responded := false
	defer func() {
		if rec := recover(); rec != nil {
			logger.ErrorCF("discord", "An interaction handler panicked", map[string]any{"panic": fmt.Sprint(rec)})
			if !responded {
				c.respondEphemeral(s, i, channels.TapFailed)
			}
		}
	}()

	user := interactionUser(i)
	if user == nil {
		responded = true
		c.respondEphemeral(s, i, channels.TapNotAuthorized)
		return
	}
	sender := discordSender(user)

	// The allow-list applies to a tap exactly as to a message, and comes first:
	// somebody who may not talk to the bot gets "Not authorized." and nothing else,
	// not even a verdict on whether their data was well formed.
	if !c.IsAllowedSender(sender) {
		logger.DebugCF("discord", "Interaction rejected by allowlist", map[string]any{"user_id": user.ID})
		responded = true
		c.respondEphemeral(s, i, channels.TapNotAuthorized)
		return
	}

	data, ok := i.Data.(discordgo.MessageComponentInteractionData)
	if !ok {
		responded = true
		c.respondEphemeral(s, i, channels.TapInvalidButton)
		return
	}
	actionID, payload, err := channels.ParseCallbackData(data.CustomID, maxCustomIDBytes)
	if err != nil {
		logger.DebugCF("discord", "Dropped a malformed component", map[string]any{"error": err.Error()})
		responded = true
		c.respondEphemeral(s, i, channels.TapInvalidButton)
		return
	}

	// A guild channel is a group, as for a message; a DM is the direct chat.
	peerKind := "direct"
	if i.GuildID != "" {
		peerKind = "channel"
	}
	messageID := ""
	if i.Message != nil {
		messageID = i.Message.ID
	}
	in := channels.NewTapInput("discord", i.ChannelID, messageID, peerKind, sender, channels.ControlCallback{
		Action: actionID, Payload: payload, CallbackID: i.ID,
	})

	// The control path normally answers in milliseconds and its words are then the
	// interaction response itself. If it is slow, the tap is acknowledged at the
	// deadline and the words follow.
	result := make(chan channels.TapOutcome, 1)
	go func() { result <- channels.RouteTap(c.runCtx(), c.ControlRouter(), in) }()
	deadline := c.ackDeadline
	if deadline <= 0 {
		deadline = interactionAckDeadline
	}
	timer := time.NewTimer(deadline)
	defer timer.Stop()
	select {
	case out := <-result:
		responded = true
		c.respond(s, i, out)
	case <-timer.C:
		responded = true
		if err := s.InteractionRespond(i, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseDeferredMessageUpdate,
		}); err != nil {
			logger.WarnCF("discord", "Could not acknowledge an interaction", map[string]any{"error": err.Error()})
			return
		}
		select {
		case out := <-result:
			c.followUp(s, i, out)
		case <-c.runCtx().Done():
		}
	}
}

// respond answers the tap with the control path's words. A reply that asks for
// the card to be edited replaces the card's text with the outcome and removes the
// buttons; anything else is said to the person who tapped, in a message only they
// see, and leaves the card as it is.
func (c *DiscordChannel) respond(s *discordgo.Session, i *discordgo.Interaction, out channels.TapOutcome) {
	text := channels.TruncateRunes(out.Words(), maxMessageLength)
	resp := &discordgo.InteractionResponse{}
	switch {
	case text == "":
		// Nothing to say: just release the spinner.
		resp.Type = discordgo.InteractionResponseDeferredMessageUpdate
	case out.Edit && strings.TrimSpace(out.Text) != "":
		resp.Type = discordgo.InteractionResponseUpdateMessage
		resp.Data = &discordgo.InteractionResponseData{
			Content: text,
			// An empty (not absent) list is what removes the buttons.
			Components: []discordgo.MessageComponent{},
		}
	default:
		resp.Type = discordgo.InteractionResponseChannelMessageWithSource
		resp.Data = &discordgo.InteractionResponseData{Content: text, Flags: discordgo.MessageFlagsEphemeral}
	}
	if err := s.InteractionRespond(i, resp); err != nil {
		logger.WarnCF("discord", "Could not respond to an interaction", map[string]any{"error": err.Error()})
	}
}

// respondEphemeral answers the tap with words only the person who tapped sees.
func (c *DiscordChannel) respondEphemeral(s *discordgo.Session, i *discordgo.Interaction, text string) {
	if err := s.InteractionRespond(i, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Content: text, Flags: discordgo.MessageFlagsEphemeral},
	}); err != nil {
		logger.WarnCF("discord", "Could not respond to an interaction", map[string]any{"error": err.Error()})
	}
}

// followUp finishes a tap that was acknowledged early: the card is edited, or the
// words are sent as a followup only the person who tapped sees.
func (c *DiscordChannel) followUp(s *discordgo.Session, i *discordgo.Interaction, out channels.TapOutcome) {
	text := channels.TruncateRunes(out.Words(), maxMessageLength)
	if text == "" {
		return
	}
	// The interaction's own deadline is what bounds these calls, not the handler's.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.runCtx()), followupTimeout)
	defer cancel()
	var err error
	if out.Edit && strings.TrimSpace(out.Text) != "" {
		_, err = s.InteractionResponseEdit(i, &discordgo.WebhookEdit{
			Content:    &text,
			Components: &[]discordgo.MessageComponent{},
		}, discordgo.WithContext(ctx))
	} else {
		_, err = s.FollowupMessageCreate(i, false, &discordgo.WebhookParams{
			Content: text,
			Flags:   discordgo.MessageFlagsEphemeral,
		}, discordgo.WithContext(ctx))
	}
	if err != nil {
		logger.WarnCF("discord", "Could not finish an interaction", map[string]any{"error": err.Error()})
	}
}
