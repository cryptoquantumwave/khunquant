package line

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/channels"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/identity"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/media"
	"github.com/cryptoquantumwave/khunquant/pkg/utils"
)

const (
	lineAPIBase          = "https://api.line.me/v2/bot"
	lineDataAPIBase      = "https://api-data.line.me/v2/bot"
	lineReplyEndpoint    = lineAPIBase + "/message/reply"
	linePushEndpoint     = lineAPIBase + "/message/push"
	lineContentEndpoint  = lineDataAPIBase + "/message/%s/content"
	lineBotInfoEndpoint  = lineAPIBase + "/info"
	lineLoadingEndpoint  = lineAPIBase + "/chat/loading/start"
	lineReplyTokenMaxAge = 25 * time.Second

	// Limit request body to prevent memory exhaustion (DoS).
	// LINE webhook payloads are typically a few KB; 1 MiB is generous.
	maxWebhookBodySize = 1 << 20 // 1 MiB

	// maxAPIResponseSize bounds the body read from a successful send.
	maxAPIResponseSize = 1 << 16

	// maxMessageLength is the length LINE allows a text message (the Manager
	// splits to it).
	maxMessageLength = 5000
)

type replyTokenEntry struct {
	token     string
	timestamp time.Time
}

// LINEChannel implements the Channel interface for LINE Official Account
// using the LINE Messaging API with HTTP webhook for receiving messages
// and REST API for sending messages.
type LINEChannel struct {
	*channels.BaseChannel
	config         config.LINEConfig
	infoClient     *http.Client // for bot info lookups (short timeout)
	apiClient      *http.Client // for messaging API calls
	botUserID      string       // Bot's user ID
	botBasicID     string       // Bot's basic ID (e.g. @216ru...)
	botDisplayName string       // Bot's display name for text-based mention detection
	replyTokens    sync.Map     // chatID -> replyTokenEntry
	quoteTokens    sync.Map     // chatID -> quoteToken (string)
	lastCard       sync.Map     // chatID -> LINE message id of the latest message sent with buttons
	ctx            context.Context
	cancel         context.CancelFunc
}

// NewLINEChannel creates a new LINE channel instance.
func NewLINEChannel(cfg config.LINEConfig, messageBus *bus.MessageBus) (*LINEChannel, error) {
	if cfg.ChannelSecret.String() == "" || cfg.ChannelAccessToken.String() == "" {
		return nil, fmt.Errorf("line channel_secret and channel_access_token are required")
	}

	base := channels.NewBaseChannel("line", cfg, messageBus, cfg.AllowFrom,
		channels.WithMaxMessageLength(maxMessageLength),
		channels.WithGroupTrigger(cfg.GroupTrigger),
		channels.WithReasoningChannelID(cfg.ReasoningChannelID),
	)

	return &LINEChannel{
		BaseChannel: base,
		config:      cfg,
		infoClient:  &http.Client{Timeout: 10 * time.Second},
		apiClient:   &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// Start initializes the LINE channel.
func (c *LINEChannel) Start(ctx context.Context) error {
	logger.InfoC("line", "Starting LINE channel (Webhook Mode)")

	c.ctx, c.cancel = context.WithCancel(ctx)

	// Fetch bot profile to get bot's userId for mention detection
	if err := c.fetchBotInfo(); err != nil {
		logger.WarnCF("line", "Failed to fetch bot info (mention detection disabled)", map[string]any{
			"error": err.Error(),
		})
	} else {
		logger.InfoCF("line", "Bot info fetched", map[string]any{
			"bot_user_id":  c.botUserID,
			"basic_id":     c.botBasicID,
			"display_name": c.botDisplayName,
		})
	}

	c.SetRunning(true)
	logger.InfoC("line", "LINE channel started (Webhook Mode)")
	return nil
}

// fetchBotInfo retrieves the bot's userId, basicId, and displayName from the LINE API.
func (c *LINEChannel) fetchBotInfo() error {
	req, err := http.NewRequest(http.MethodGet, lineBotInfoEndpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.config.ChannelAccessToken.String())

	resp, err := c.infoClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bot info API returned status %d", resp.StatusCode)
	}

	var info struct {
		UserID      string `json:"userId"`
		BasicID     string `json:"basicId"`
		DisplayName string `json:"displayName"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return err
	}

	c.botUserID = info.UserID
	c.botBasicID = info.BasicID
	c.botDisplayName = info.DisplayName
	return nil
}

// Stop gracefully stops the LINE channel.
func (c *LINEChannel) Stop(ctx context.Context) error {
	logger.InfoC("line", "Stopping LINE channel")

	if c.cancel != nil {
		c.cancel()
	}

	c.SetRunning(false)
	logger.InfoC("line", "LINE channel stopped")
	return nil
}

// WebhookPath returns the path for registering on the shared HTTP server.
func (c *LINEChannel) WebhookPath() string {
	if c.config.WebhookPath != "" {
		return c.config.WebhookPath
	}
	return "/webhook/line"
}

// ServeHTTP implements http.Handler for the shared HTTP server.
func (c *LINEChannel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.webhookHandler(w, r)
}

// webhookHandler handles incoming LINE webhook requests.
func (c *LINEChannel) webhookHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBodySize+1))
	if err != nil {
		logger.ErrorCF("line", "Failed to read request body", map[string]any{
			"error": err.Error(),
		})
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	if int64(len(body)) > maxWebhookBodySize {
		logger.WarnC("line", "Webhook request body too large, rejected")
		http.Error(w, "Request entity too large", http.StatusRequestEntityTooLarge)
		return
	}

	signature := r.Header.Get("X-Line-Signature")
	if !c.verifySignature(body, signature) {
		logger.WarnC("line", "Invalid webhook signature")
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	var payload struct {
		Events []lineEvent `json:"events"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		logger.ErrorCF("line", "Failed to parse webhook payload", map[string]any{
			"error": err.Error(),
		})
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	// Return 200 immediately, process events asynchronously
	w.WriteHeader(http.StatusOK)

	for _, event := range payload.Events {
		go c.processEvent(event)
	}
}

// verifySignature validates the X-Line-Signature using HMAC-SHA256.
func (c *LINEChannel) verifySignature(body []byte, signature string) bool {
	if signature == "" {
		return false
	}

	mac := hmac.New(sha256.New, []byte(c.config.ChannelSecret.String()))
	mac.Write(body)
	expected := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	return hmac.Equal([]byte(expected), []byte(signature))
}

// LINE webhook event types
type lineEvent struct {
	Type           string          `json:"type"`
	ReplyToken     string          `json:"replyToken"`
	Source         lineSource      `json:"source"`
	Message        json.RawMessage `json:"message"`
	Timestamp      int64           `json:"timestamp"`
	WebhookEventID string          `json:"webhookEventId"`
	// Postback is set on a "postback" event: the tap of a quick-reply button.
	Postback *linePostback `json:"postback"`
}

type linePostback struct {
	Data string `json:"data"`
}

type lineSource struct {
	Type    string `json:"type"` // "user", "group", "room"
	UserID  string `json:"userId"`
	GroupID string `json:"groupId"`
	RoomID  string `json:"roomId"`
}

type lineMessage struct {
	ID         string `json:"id"`
	Type       string `json:"type"` // "text", "image", "video", "audio", "file", "sticker"
	Text       string `json:"text"`
	QuoteToken string `json:"quoteToken"`
	Mention    *struct {
		Mentionees []lineMentionee `json:"mentionees"`
	} `json:"mention"`
	ContentProvider struct {
		Type string `json:"type"`
	} `json:"contentProvider"`
}

type lineMentionee struct {
	Index  int    `json:"index"`
	Length int    `json:"length"`
	Type   string `json:"type"` // "user", "all"
	UserID string `json:"userId"`
}

func (c *LINEChannel) processEvent(event lineEvent) {
	if event.Type == "postback" {
		c.processPostback(event)
		return
	}
	if event.Type != "message" {
		logger.DebugCF("line", "Ignoring non-message event", map[string]any{
			"type": event.Type,
		})
		return
	}

	senderID := event.Source.UserID
	chatID := c.resolveChatID(event.Source)
	isGroup := isGroupSource(event.Source)

	var msg lineMessage
	if err := json.Unmarshal(event.Message, &msg); err != nil {
		logger.ErrorCF("line", "Failed to parse message", map[string]any{
			"error": err.Error(),
		})
		return
	}

	// Store reply token for later use
	if event.ReplyToken != "" {
		c.replyTokens.Store(chatID, replyTokenEntry{
			token:     event.ReplyToken,
			timestamp: time.Now(),
		})
	}

	// Store quote token for quoting the original message in reply
	if msg.QuoteToken != "" {
		c.quoteTokens.Store(chatID, msg.QuoteToken)
	}

	var content string
	var mediaPaths []string

	scope := channels.BuildMediaScope("line", chatID, msg.ID)

	// Helper to register a local file with the media store
	storeMedia := func(localPath, filename string) string {
		if store := c.GetMediaStore(); store != nil {
			ref, err := store.Store(localPath, media.MediaMeta{
				Filename: filename,
				Source:   "line",
			}, scope)
			if err == nil {
				return ref
			}
		}
		return localPath // fallback
	}

	switch msg.Type {
	case "text":
		content = msg.Text
		// Strip bot mention from text in group chats
		if isGroup {
			content = c.stripBotMention(content, msg)
		}
	case "image":
		localPath := c.downloadContent(msg.ID, "image.jpg")
		if localPath != "" {
			mediaPaths = append(mediaPaths, storeMedia(localPath, "image.jpg"))
			content = "[image]"
		}
	case "audio":
		localPath := c.downloadContent(msg.ID, "audio.m4a")
		if localPath != "" {
			mediaPaths = append(mediaPaths, storeMedia(localPath, "audio.m4a"))
			content = "[audio]"
		}
	case "video":
		localPath := c.downloadContent(msg.ID, "video.mp4")
		if localPath != "" {
			mediaPaths = append(mediaPaths, storeMedia(localPath, "video.mp4"))
			content = "[video]"
		}
	case "file":
		content = "[file]"
	case "sticker":
		content = "[sticker]"
	default:
		content = fmt.Sprintf("[%s]", msg.Type)
	}

	if strings.TrimSpace(content) == "" {
		return
	}

	// In group chats, apply unified group trigger filtering
	if isGroup {
		isMentioned := c.isBotMentioned(msg)
		respond, cleaned := c.ShouldRespondInGroup(isMentioned, content)
		if !respond {
			logger.DebugCF("line", "Ignoring group message by group trigger", map[string]any{
				"chat_id": chatID,
			})
			return
		}
		content = cleaned
	}

	metadata := map[string]string{
		"platform":    "line",
		"source_type": event.Source.Type,
	}

	var peer bus.Peer
	if isGroup {
		peer = bus.Peer{Kind: "group", ID: chatID}
	} else {
		peer = bus.Peer{Kind: "direct", ID: senderID}
	}

	logger.DebugCF("line", "Received message", map[string]any{
		"sender_id":    senderID,
		"chat_id":      chatID,
		"message_type": msg.Type,
		"is_group":     isGroup,
		"preview":      utils.Truncate(content, 50),
	})

	sender := lineSender(senderID)

	if !c.IsAllowedSender(sender) {
		return
	}

	c.HandleMessage(c.ctx, peer, msg.ID, senderID, chatID, content, mediaPaths, metadata, sender)
}

// isBotMentioned checks if the bot is mentioned in the message.
// It first checks the mention metadata (userId match), then falls back
// to text-based detection using the bot's display name, since LINE may
// not include userId in mentionees for Official Accounts.
func (c *LINEChannel) isBotMentioned(msg lineMessage) bool {
	// Check mention metadata
	if msg.Mention != nil {
		for _, m := range msg.Mention.Mentionees {
			if m.Type == "all" {
				return true
			}
			if c.botUserID != "" && m.UserID == c.botUserID {
				return true
			}
		}
		// Mention metadata exists with mentionees but bot not matched by userId.
		// The bot IS likely mentioned (LINE includes mention struct when bot is @-ed),
		// so check if any mentionee overlaps with bot display name in text.
		if c.botDisplayName != "" {
			for _, m := range msg.Mention.Mentionees {
				if m.Index >= 0 && m.Length > 0 {
					runes := []rune(msg.Text)
					end := m.Index + m.Length
					if end <= len(runes) {
						mentionText := string(runes[m.Index:end])
						if strings.Contains(mentionText, c.botDisplayName) {
							return true
						}
					}
				}
			}
		}
	}

	// Fallback: text-based detection with display name
	if c.botDisplayName != "" && strings.Contains(msg.Text, "@"+c.botDisplayName) {
		return true
	}

	return false
}

// stripBotMention removes the @BotName mention text from the message.
func (c *LINEChannel) stripBotMention(text string, msg lineMessage) string {
	stripped := false

	// Try to strip using mention metadata indices
	if msg.Mention != nil {
		runes := []rune(text)
		for i := len(msg.Mention.Mentionees) - 1; i >= 0; i-- {
			m := msg.Mention.Mentionees[i]
			// Strip if userId matches OR if the mention text contains the bot display name
			shouldStrip := false
			if c.botUserID != "" && m.UserID == c.botUserID {
				shouldStrip = true
			} else if c.botDisplayName != "" && m.Index >= 0 && m.Length > 0 {
				end := m.Index + m.Length
				if end <= len(runes) {
					mentionText := string(runes[m.Index:end])
					if strings.Contains(mentionText, c.botDisplayName) {
						shouldStrip = true
					}
				}
			}
			if shouldStrip {
				start := m.Index
				end := m.Index + m.Length
				if start >= 0 && end <= len(runes) {
					runes = append(runes[:start], runes[end:]...)
					stripped = true
				}
			}
		}
		if stripped {
			return strings.TrimSpace(string(runes))
		}
	}

	// Fallback: strip @DisplayName from text
	if c.botDisplayName != "" {
		text = strings.ReplaceAll(text, "@"+c.botDisplayName, "")
	}

	return strings.TrimSpace(text)
}

// resolveChatID determines the chat ID from the event source.
// For group/room messages, use the group/room ID; for 1:1, use the user ID.
func (c *LINEChannel) resolveChatID(source lineSource) string {
	switch source.Type {
	case "group":
		return source.GroupID
	case "room":
		return source.RoomID
	default:
		return source.UserID
	}
}

// lineSender is the sender of a message or a tap in the one canonical form
// ("line:<userId>") the allow-list and the control path compare.
func lineSender(userID string) bus.SenderInfo {
	return bus.SenderInfo{
		Platform:    "line",
		PlatformID:  userID,
		CanonicalID: identity.BuildCanonicalID("line", userID),
	}
}

// isGroupSource reports whether an event came from a group or a multi-person room.
func isGroupSource(src lineSource) bool {
	return src.Type == "group" || src.Type == "room"
}

// Send sends a message to LINE. See SendWithIDs.
func (c *LINEChannel) Send(ctx context.Context, msg bus.OutboundMessage) error {
	_, err := c.SendWithIDs(ctx, msg)
	return err
}

// SendWithIDs implements channels.IDReportingSender. It first tries the Reply
// API (free) using a cached reply token, then falls back to the Push API, and
// returns the ID LINE gave each message it created. The IDs are what a later
// "snooze" or "useful" button is tied back to its alert with.
func (c *LINEChannel) SendWithIDs(ctx context.Context, msg bus.OutboundMessage) ([]string, error) {
	if !c.IsRunning() {
		return nil, channels.ErrNotRunning
	}

	// Load and consume quote token for this chat
	var quoteToken string
	if qt, ok := c.quoteTokens.LoadAndDelete(msg.ChatID); ok {
		quoteToken = qt.(string)
	}

	// Buttons: the text fallback of the actions stays in the body (the Manager
	// leaves it out for a channel that renders actions), and the quick-reply
	// buttons go on the last message. If any action cannot be encoded the message
	// goes without buttons: all of them or none.
	texts := []string{msg.Content}
	var quickReply map[string]any
	if len(msg.Actions) > 0 {
		texts = channels.AppendActionFooter(texts, msg.Actions, maxMessageLength)
		var qrErr error
		if quickReply, qrErr = buildQuickReply(msg.Actions); qrErr != nil {
			logger.WarnCF("line", "Invalid message actions, sending the message without buttons", map[string]any{
				"intervention_id": msg.InterventionID,
				"actions":         len(msg.Actions),
				"error":           qrErr.Error(),
			})
			quickReply = nil
		}
	}
	messages := buildMessages(texts, quoteToken, quickReply)

	ids, err := c.deliver(ctx, msg.ChatID, messages)
	if err != nil {
		return nil, err
	}
	// Quick-reply buttons show on the newest message of the chat only, so the
	// latest message with buttons is the card a tap came from; any other message
	// sent after it takes the buttons away.
	if quickReply != nil && len(ids) > 0 {
		c.lastCard.Store(msg.ChatID, ids[len(ids)-1])
	} else {
		c.lastCard.Delete(msg.ChatID)
	}
	return ids, nil
}

// deliver sends messages to a chat: the Reply API with a cached reply token
// first (free, valid for ~25 seconds), then the Push API.
func (c *LINEChannel) deliver(ctx context.Context, chatID string, messages []map[string]any) ([]string, error) {
	if entry, ok := c.replyTokens.LoadAndDelete(chatID); ok {
		tokenEntry := entry.(replyTokenEntry)
		if time.Since(tokenEntry.timestamp) < lineReplyTokenMaxAge {
			if ids, err := c.sendReply(ctx, tokenEntry.token, messages); err == nil {
				logger.DebugCF("line", "Message sent via Reply API", map[string]any{
					"chat_id": chatID,
				})
				return ids, nil
			}
			logger.DebugC("line", "Reply API failed, falling back to Push API")
		}
	}

	// Fall back to Push API
	return c.sendPush(ctx, chatID, messages)
}

// SendMedia implements the channels.MediaSender interface.
// LINE requires media to be accessible via public URL; since we only have local files,
// we fall back to sending a text message with the filename/caption.
// For full support, an external file hosting service would be needed.
func (c *LINEChannel) SendMedia(ctx context.Context, msg bus.OutboundMediaMessage) error {
	if !c.IsRunning() {
		return channels.ErrNotRunning
	}

	store := c.GetMediaStore()
	if store == nil {
		return fmt.Errorf("no media store available: %w", channels.ErrSendFailed)
	}

	// LINE Messaging API requires publicly accessible URLs for media messages.
	// Since we only have local file paths, send caption text as fallback.
	for _, part := range msg.Parts {
		caption := part.Caption
		if caption == "" {
			caption = fmt.Sprintf("[%s: %s]", part.Type, part.Filename)
		}

		if _, err := c.sendPush(ctx, msg.ChatID, []map[string]any{buildTextMessage(caption, "")}); err != nil {
			return err
		}
		c.lastCard.Delete(msg.ChatID)
	}

	return nil
}

// buildTextMessage creates a text message object, optionally with quoteToken.
func buildTextMessage(content, quoteToken string) map[string]any {
	msg := map[string]any{
		"type": "text",
		"text": content,
	}
	if quoteToken != "" {
		msg["quoteToken"] = quoteToken
	}
	return msg
}

// buildMessages makes one text message per text. The quote token belongs to the
// first, the quick-reply buttons to the last.
func buildMessages(texts []string, quoteToken string, quickReply map[string]any) []map[string]any {
	messages := make([]map[string]any, 0, len(texts))
	for i, t := range texts {
		qt := ""
		if i == 0 {
			qt = quoteToken
		}
		messages = append(messages, buildTextMessage(t, qt))
	}
	if quickReply != nil && len(messages) > 0 {
		messages[len(messages)-1]["quickReply"] = quickReply
	}
	return messages
}

// sendReply sends messages using the LINE Reply API and returns their IDs.
func (c *LINEChannel) sendReply(ctx context.Context, replyToken string, messages []map[string]any) ([]string, error) {
	payload := map[string]any{
		"replyToken": replyToken,
		"messages":   messages,
	}

	return c.callAPIWithIDs(ctx, lineReplyEndpoint, payload)
}

// sendPush sends messages using the LINE Push API and returns their IDs.
func (c *LINEChannel) sendPush(ctx context.Context, to string, messages []map[string]any) ([]string, error) {
	payload := map[string]any{
		"to":       to,
		"messages": messages,
	}

	return c.callAPIWithIDs(ctx, linePushEndpoint, payload)
}

// StartTyping implements channels.TypingCapable using LINE's loading animation.
//
// NOTE: The LINE loading animation API only works for 1:1 chats.
// Group/room chat IDs (starting with "C" or "R") are detected automatically;
// for these, a no-op stop function is returned without calling the API.
func (c *LINEChannel) StartTyping(ctx context.Context, chatID string) (func(), error) {
	if chatID == "" {
		return func() {}, nil
	}

	// Group/room chats: LINE loading animation is 1:1 only.
	if strings.HasPrefix(chatID, "C") || strings.HasPrefix(chatID, "R") {
		return func() {}, nil
	}

	typingCtx, cancel := context.WithCancel(ctx)
	var once sync.Once
	stop := func() { once.Do(cancel) }

	// Send immediately, then refresh periodically for long-running tasks.
	if err := c.sendLoading(typingCtx, chatID); err != nil {
		stop()
		return stop, err
	}

	ticker := time.NewTicker(50 * time.Second)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-typingCtx.Done():
				return
			case <-ticker.C:
				if err := c.sendLoading(typingCtx, chatID); err != nil {
					logger.DebugCF("line", "Failed to refresh loading indicator", map[string]any{
						"error": err.Error(),
					})
				}
			}
		}
	}()

	return stop, nil
}

// sendLoading sends a loading animation indicator to the chat.
func (c *LINEChannel) sendLoading(ctx context.Context, chatID string) error {
	payload := map[string]any{
		"chatId":         chatID,
		"loadingSeconds": 60,
	}
	return c.callAPI(ctx, lineLoadingEndpoint, payload)
}

// callAPI makes an authenticated POST request to the LINE API.
func (c *LINEChannel) callAPI(ctx context.Context, endpoint string, payload any) error {
	_, err := c.callAPIWithIDs(ctx, endpoint, payload)
	return err
}

// callAPIWithIDs makes an authenticated POST request to the LINE API and returns
// the IDs of the messages it reports having sent (the reply and push endpoints
// answer {"sentMessages":[{"id":"..."}]}).
func (c *LINEChannel) callAPIWithIDs(ctx context.Context, endpoint string, payload any) ([]string, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.config.ChannelAccessToken.String())

	resp, err := c.apiClient.Do(req)
	if err != nil {
		return nil, channels.ClassifyNetError(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, channels.ClassifySendError(resp.StatusCode, fmt.Errorf("reading LINE API error response: %w", err))
		}
		return nil, channels.ClassifySendError(resp.StatusCode, fmt.Errorf("LINE API error: %s", string(respBody)))
	}

	var sent struct {
		SentMessages []struct {
			ID string `json:"id"`
		} `json:"sentMessages"`
	}
	// A body that is not the expected JSON only costs the IDs: the send worked.
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxAPIResponseSize)).Decode(&sent); err != nil {
		return nil, nil
	}
	var ids []string
	for _, m := range sent.SentMessages {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	return ids, nil
}

// downloadContent downloads media content from the LINE API.
func (c *LINEChannel) downloadContent(messageID, filename string) string {
	url := fmt.Sprintf(lineContentEndpoint, messageID)
	return utils.DownloadFile(url, filename, utils.DownloadOptions{
		LoggerPrefix: "line",
		ExtraHeaders: map[string]string{
			"Authorization": "Bearer " + c.config.ChannelAccessToken.String(),
		},
	})
}
