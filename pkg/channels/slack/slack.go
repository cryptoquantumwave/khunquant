package slack

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/channels"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/identity"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/media"
	"github.com/cryptoquantumwave/khunquant/pkg/utils"
)

// maxMessageLength is the length Slack allows a message's text (the Manager
// splits to it).
const maxMessageLength = 40000

type SlackChannel struct {
	*channels.BaseChannel
	config       config.SlackConfig
	api          *slack.Client
	socketClient *socketmode.Client
	botUserID    string
	teamID       string
	ctx          context.Context
	cancel       context.CancelFunc
	pendingAcks  sync.Map
	// ackFn acknowledges a socket-mode request (socketClient.Ack; a test replaces it).
	ackFn func(socketmode.Request, ...any) error
}

type slackMessageRef struct {
	ChannelID string
	Timestamp string
}

func NewSlackChannel(cfg config.SlackConfig, messageBus *bus.MessageBus) (*SlackChannel, error) {
	if cfg.BotToken.String() == "" || cfg.AppToken.String() == "" {
		return nil, fmt.Errorf("slack bot_token and app_token are required")
	}

	api := slack.New(
		cfg.BotToken.String(),
		slack.OptionAppLevelToken(cfg.AppToken.String()),
	)

	socketClient := socketmode.New(api)

	base := channels.NewBaseChannel("slack", cfg, messageBus, cfg.AllowFrom,
		channels.WithMaxMessageLength(maxMessageLength),
		channels.WithGroupTrigger(cfg.GroupTrigger),
		channels.WithReasoningChannelID(cfg.ReasoningChannelID),
	)

	return &SlackChannel{
		BaseChannel:  base,
		config:       cfg,
		api:          api,
		socketClient: socketClient,
		ackFn:        socketClient.Ack,
	}, nil
}

func (c *SlackChannel) Start(ctx context.Context) error {
	logger.InfoC("slack", "Starting Slack channel (Socket Mode)")

	c.ctx, c.cancel = context.WithCancel(ctx)

	authResp, err := c.api.AuthTest()
	if err != nil {
		return fmt.Errorf("slack auth test failed: %w", err)
	}
	c.botUserID = authResp.UserID
	c.teamID = authResp.TeamID

	logger.InfoCF("slack", "Slack bot connected", map[string]any{
		"bot_user_id": c.botUserID,
		"team":        authResp.Team,
	})

	go c.eventLoop()

	go func() {
		if err := c.socketClient.RunContext(c.ctx); err != nil {
			if c.ctx.Err() == nil {
				logger.ErrorCF("slack", "Socket Mode connection error", map[string]any{
					"error": err.Error(),
				})
			}
		}
	}()

	c.SetRunning(true)
	logger.InfoC("slack", "Slack channel started (Socket Mode)")
	return nil
}

func (c *SlackChannel) Stop(ctx context.Context) error {
	logger.InfoC("slack", "Stopping Slack channel")

	if c.cancel != nil {
		c.cancel()
	}

	c.SetRunning(false)
	logger.InfoC("slack", "Slack channel stopped")
	return nil
}

func (c *SlackChannel) Send(ctx context.Context, msg bus.OutboundMessage) error {
	_, err := c.SendWithIDs(ctx, msg)
	return err
}

// SendWithIDs implements channels.IDReportingSender. It returns the message
// timestamp ("ts") Slack assigns to the posted message, which is Slack's
// message ID.
func (c *SlackChannel) SendWithIDs(ctx context.Context, msg bus.OutboundMessage) ([]string, error) {
	if !c.IsRunning() {
		return nil, channels.ErrNotRunning
	}

	channelID, threadTS := parseSlackChatID(msg.ChatID)
	if channelID == "" {
		return nil, fmt.Errorf("invalid slack chat ID: %s", msg.ChatID)
	}

	// Buttons: the text fallback of the actions stays in the body (the Manager
	// leaves it out for a channel that renders actions), and the buttons go on the
	// last message only. If any action cannot be encoded the message goes without
	// buttons: all of them or none.
	chunks := []string{msg.Content}
	var actionsBlock *slack.ActionBlock
	if len(msg.Actions) > 0 {
		chunks = channels.AppendActionFooter(chunks, msg.Actions, maxMessageLength)
		var bErr error
		if actionsBlock, bErr = buildActionsBlock(msg.Actions); bErr != nil {
			logger.WarnCF("slack", "Invalid message actions, sending the message without buttons", map[string]any{
				"intervention_id": msg.InterventionID,
				"actions":         len(msg.Actions),
				"error":           bErr.Error(),
			})
			actionsBlock = nil
		}
	}

	var ids []string
	for i, chunk := range chunks {
		opts := []slack.MsgOption{
			slack.MsgOptionText(chunk, false),
		}

		if msg.ReplyToMessageID != "" && threadTS == "" {
			// Answer to the message by creating a Thread under it
			opts = append(opts, slack.MsgOptionTS(msg.ReplyToMessageID))
		} else if threadTS != "" {
			// If we are already in a thread, continue in the thread
			opts = append(opts, slack.MsgOptionTS(threadTS))
		}
		if actionsBlock != nil && i == len(chunks)-1 {
			// With blocks, the text above is only the notification; the body is the
			// section blocks.
			opts = append(opts, slack.MsgOptionBlocks(append(sectionBlocks(chunk), actionsBlock)...))
		}

		_, ts, err := c.api.PostMessageContext(ctx, channelID, opts...)
		if err != nil {
			return ids, fmt.Errorf("slack send: %w", channels.ErrTemporary)
		}
		if ts != "" {
			ids = append(ids, ts)
		}
	}

	if ref, ok := c.pendingAcks.LoadAndDelete(msg.ChatID); ok {
		msgRef := ref.(slackMessageRef)
		c.api.AddReaction("white_check_mark", slack.ItemRef{
			Channel:   msgRef.ChannelID,
			Timestamp: msgRef.Timestamp,
		})
	}

	logger.DebugCF("slack", "Message sent", map[string]any{
		"channel_id": channelID,
		"thread_ts":  threadTS,
	})

	return ids, nil
}

// SendMedia implements the channels.MediaSender interface.
func (c *SlackChannel) SendMedia(ctx context.Context, msg bus.OutboundMediaMessage) error {
	if !c.IsRunning() {
		return channels.ErrNotRunning
	}

	channelID, _ := parseSlackChatID(msg.ChatID)
	if channelID == "" {
		return fmt.Errorf("invalid slack chat ID: %s", msg.ChatID)
	}

	store := c.GetMediaStore()
	if store == nil {
		return fmt.Errorf("no media store available: %w", channels.ErrSendFailed)
	}

	for _, part := range msg.Parts {
		localPath, err := store.Resolve(part.Ref)
		if err != nil {
			logger.ErrorCF("slack", "Failed to resolve media ref", map[string]any{
				"ref":   part.Ref,
				"error": err.Error(),
			})
			continue
		}

		filename := part.Filename
		if filename == "" {
			filename = "file"
		}

		title := part.Caption
		if title == "" {
			title = filename
		}

		_, err = c.api.UploadFileContext(ctx, slack.UploadFileParameters{
			Channel:  channelID,
			File:     localPath,
			Filename: filename,
			Title:    title,
		})
		if err != nil {
			logger.ErrorCF("slack", "Failed to upload media", map[string]any{
				"filename": filename,
				"error":    err.Error(),
			})
			return fmt.Errorf("slack send media: %w", channels.ErrTemporary)
		}
	}

	return nil
}

// ReactToMessage implements channels.ReactionCapable.
// It adds an "eyes" (👀) reaction to the inbound message and returns an undo function
// that removes the reaction.
func (c *SlackChannel) ReactToMessage(ctx context.Context, chatID, messageID string) (func(), error) {
	channelID, _ := parseSlackChatID(chatID)
	if channelID == "" {
		return func() {}, nil
	}

	c.api.AddReaction("eyes", slack.ItemRef{
		Channel:   channelID,
		Timestamp: messageID,
	})

	return func() {
		c.api.RemoveReaction("eyes", slack.ItemRef{
			Channel:   channelID,
			Timestamp: messageID,
		})
	}, nil
}

func (c *SlackChannel) eventLoop() {
	for {
		select {
		case <-c.ctx.Done():
			return
		case event, ok := <-c.socketClient.Events:
			if !ok {
				return
			}
			switch event.Type {
			case socketmode.EventTypeEventsAPI:
				c.handleEventsAPI(event)
			case socketmode.EventTypeSlashCommand:
				c.handleSlashCommand(event)
			case socketmode.EventTypeInteractive:
				c.handleInteractive(event)
			}
		}
	}
}

func (c *SlackChannel) handleEventsAPI(event socketmode.Event) {
	if event.Request != nil {
		c.socketClient.Ack(*event.Request)
	}

	eventsAPIEvent, ok := event.Data.(slackevents.EventsAPIEvent)
	if !ok {
		return
	}

	switch ev := eventsAPIEvent.InnerEvent.Data.(type) {
	case *slackevents.MessageEvent:
		c.handleMessageEvent(ev)
	case *slackevents.AppMentionEvent:
		c.handleAppMention(ev)
	}
}

func (c *SlackChannel) handleMessageEvent(ev *slackevents.MessageEvent) {
	if ev.User == c.botUserID || ev.User == "" {
		return
	}
	if ev.BotID != "" {
		return
	}
	if ev.SubType != "" && ev.SubType != "file_share" {
		return
	}

	// check allowlist to avoid downloading attachments for rejected users
	sender := slackSender(ev.User)
	if !c.IsAllowedSender(sender) {
		logger.DebugCF("slack", "Message rejected by allowlist", map[string]any{
			"user_id": ev.User,
		})
		return
	}

	senderID := ev.User
	channelID := ev.Channel
	threadTS := ev.ThreadTimeStamp
	messageTS := ev.TimeStamp

	chatID := channelID
	if threadTS != "" {
		chatID = channelID + "/" + threadTS
	}

	c.pendingAcks.Store(chatID, slackMessageRef{
		ChannelID: channelID,
		Timestamp: messageTS,
	})

	content := ev.Text
	content = c.stripBotMention(content)

	// In non-DM channels, apply group trigger filtering
	if !isDMChannel(channelID) {
		respond, cleaned := c.ShouldRespondInGroup(false, content)
		if !respond {
			return
		}
		content = cleaned
	}

	var mediaPaths []string

	scope := channels.BuildMediaScope("slack", chatID, messageTS)

	// Helper to register a local file with the media store
	storeMedia := func(localPath, filename string) string {
		if store := c.GetMediaStore(); store != nil {
			ref, err := store.Store(localPath, media.MediaMeta{
				Filename: filename,
				Source:   "slack",
			}, scope)
			if err == nil {
				return ref
			}
		}
		return localPath // fallback
	}

	if ev.Message != nil && len(ev.Message.Files) > 0 {
		for _, file := range ev.Message.Files {
			localPath := c.downloadSlackFile(file)
			if localPath == "" {
				continue
			}
			mediaPaths = append(mediaPaths, storeMedia(localPath, file.Name))
			content += fmt.Sprintf("\n[file: %s]", file.Name)
		}
	}

	if strings.TrimSpace(content) == "" {
		return
	}

	peerKind := "channel"
	peerID := channelID
	if isDMChannel(channelID) {
		peerKind = "direct"
		peerID = senderID
	}

	peer := bus.Peer{Kind: peerKind, ID: peerID}

	metadata := map[string]string{
		"message_ts": messageTS,
		"channel_id": channelID,
		"thread_ts":  threadTS,
		"platform":   "slack",
		"team_id":    c.teamID,
	}

	logger.DebugCF("slack", "Received message", map[string]any{
		"sender_id":  senderID,
		"chat_id":    chatID,
		"preview":    utils.Truncate(content, 50),
		"has_thread": threadTS != "",
	})

	c.HandleMessage(c.ctx, peer, messageTS, senderID, chatID, content, mediaPaths, metadata, sender)
}

func (c *SlackChannel) handleAppMention(ev *slackevents.AppMentionEvent) {
	if ev.User == c.botUserID {
		return
	}

	if !c.IsAllowedSender(slackSender(ev.User)) {
		logger.DebugCF("slack", "Mention rejected by allowlist", map[string]any{
			"user_id": ev.User,
		})
		return
	}

	senderID := ev.User
	mentionSender := slackSender(senderID)
	channelID := ev.Channel
	threadTS := ev.ThreadTimeStamp
	messageTS := ev.TimeStamp

	var chatID string
	if threadTS != "" {
		chatID = channelID + "/" + threadTS
	} else {
		chatID = channelID + "/" + messageTS
	}

	c.pendingAcks.Store(chatID, slackMessageRef{
		ChannelID: channelID,
		Timestamp: messageTS,
	})

	content := c.stripBotMention(ev.Text)

	if strings.TrimSpace(content) == "" {
		return
	}

	mentionPeerKind := "channel"
	mentionPeerID := channelID
	if isDMChannel(channelID) {
		mentionPeerKind = "direct"
		mentionPeerID = senderID
	}

	mentionPeer := bus.Peer{Kind: mentionPeerKind, ID: mentionPeerID}

	metadata := map[string]string{
		"message_ts": messageTS,
		"channel_id": channelID,
		"thread_ts":  threadTS,
		"platform":   "slack",
		"is_mention": "true",
		"team_id":    c.teamID,
	}

	c.HandleMessage(c.ctx, mentionPeer, messageTS, senderID, chatID, content, nil, metadata, mentionSender)
}

func (c *SlackChannel) handleSlashCommand(event socketmode.Event) {
	cmd, ok := event.Data.(slack.SlashCommand)
	if !ok {
		return
	}

	if event.Request != nil {
		c.socketClient.Ack(*event.Request)
	}

	cmdSender := slackSender(cmd.UserID)
	if !c.IsAllowedSender(cmdSender) {
		logger.DebugCF("slack", "Slash command rejected by allowlist", map[string]any{
			"user_id": cmd.UserID,
		})
		return
	}

	senderID := cmd.UserID
	channelID := cmd.ChannelID
	chatID := channelID
	content := cmd.Text

	if strings.TrimSpace(content) == "" {
		content = "help"
	}

	metadata := map[string]string{
		"channel_id": channelID,
		"platform":   "slack",
		"is_command": "true",
		"trigger_id": cmd.TriggerID,
		"team_id":    c.teamID,
	}

	logger.DebugCF("slack", "Slash command received", map[string]any{
		"sender_id": senderID,
		"command":   cmd.Command,
		"text":      utils.Truncate(content, 50),
	})

	c.HandleMessage(
		c.ctx,
		bus.Peer{Kind: "channel", ID: channelID},
		"",
		senderID,
		chatID,
		content,
		nil,
		metadata,
		cmdSender,
	)
}

func (c *SlackChannel) downloadSlackFile(file slack.File) string {
	downloadURL := file.URLPrivateDownload
	if downloadURL == "" {
		downloadURL = file.URLPrivate
	}
	if downloadURL == "" {
		logger.ErrorCF("slack", "No download URL for file", map[string]any{"file_id": file.ID})
		return ""
	}

	return utils.DownloadFile(downloadURL, file.Name, utils.DownloadOptions{
		LoggerPrefix: "slack",
		ExtraHeaders: map[string]string{
			"Authorization": "Bearer " + c.config.BotToken.String(),
		},
	})
}

func (c *SlackChannel) stripBotMention(text string) string {
	mention := fmt.Sprintf("<@%s>", c.botUserID)
	text = strings.ReplaceAll(text, mention, "")
	return strings.TrimSpace(text)
}

// slackSender is the sender of a message or a tap in the one canonical form
// ("slack:<id>") the allow-list and the control path compare.
func slackSender(userID string) bus.SenderInfo {
	return bus.SenderInfo{
		Platform:    "slack",
		PlatformID:  userID,
		CanonicalID: identity.BuildCanonicalID("slack", userID),
	}
}

// isDMChannel reports whether a Slack channel ID is a direct message (an IM).
func isDMChannel(channelID string) bool { return strings.HasPrefix(channelID, "D") }

func parseSlackChatID(chatID string) (channelID, threadTS string) {
	parts := strings.SplitN(chatID, "/", 2)
	channelID = parts[0]
	if len(parts) > 1 {
		threadTS = parts[1]
	}
	return channelID, threadTS
}
