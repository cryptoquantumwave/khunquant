package teamswebhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	goteamsnotify "github.com/atc0005/go-teams-notify/v2"
	"github.com/atc0005/go-teams-notify/v2/adaptivecard"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/channels"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
)

// statusCodeRe extracts HTTP status codes from error messages like "401 Unauthorized".
var statusCodeRe = regexp.MustCompile(`\b([45]\d{2})\b`)

// markdownTableRe matches a markdown table block (header + separator + rows).
// It captures the entire table including all rows.
var markdownTableRe = regexp.MustCompile(`(?m)^(\|[^\n]+\|)\n(\|[-:\|\s]+\|)\n((?:\|[^\n]+\|\n?)+)`)

// paragraphSplitRe splits content on blank-line boundaries. It is the same
// separator markdownTableRe relies on to keep a whole table on one side of a
// split (see splitContentWithTables), so the paragraph pass used to degrade
// an oversized message never slices a table in half.
var paragraphSplitRe = regexp.MustCompile(`\n{2,}`)

const (
	// defaultSendTimeout bounds a single webhook POST. The vendored
	// go-teams-notify client's own http.Client.Timeout is left unset (its
	// comment says a context should be used instead), and without this the
	// long-lived ctx the manager passes down would let a stalled endpoint
	// block a send indefinitely.
	defaultSendTimeout = 30 * time.Second

	// teamsCardSizeBudget is the byte ceiling for one Adaptive Card message,
	// kept safely under Teams' ~28 KB webhook payload limit (see
	// WithMaxMessageLength below) so an oversized card isn't silently
	// dropped by Teams with a permanent 4xx.
	teamsCardSizeBudget = 25 * 1024

	// redactedWebhookURL replaces a webhook URL wherever it turns up in an
	// error message returned from this package.
	redactedWebhookURL = "[redacted-webhook-url]"
)

// suspiciousWebhookURLPattern is a defensive fallback for sanitizeWebhookError:
// it matches any https:// URL that looks like a Teams/Power-Automate webhook
// endpoint, in case an error embeds one that isn't the exact configured URL
// (for example a URL surfaced by a redirect).
var suspiciousWebhookURLPattern = regexp.MustCompile(
	`https://\S*(?:webhook|logic\.azure|office\.com|powerautomate)\S*`,
)

// sanitizeWebhookError removes every URL in knownURLs, and defensively any
// URL matching suspiciousWebhookURLPattern, from err's message. The webhook
// URL is the bearer secret for this channel: the vendored go-teams-notify
// client embeds it verbatim in its own URL-pattern-validation errors and in
// net/http errors, and an unredacted error reaches logs/callers via
// manager.go's err.Error() logging. HTTP status text (e.g. "401
// Unauthorized") is untouched, so classifyTeamsError still works on the
// result.
//
// Exact replacement runs before the regex pass so the regex cannot partially
// mangle a URL that the literal pass would otherwise catch whole.
func sanitizeWebhookError(err error, knownURLs ...string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	for _, u := range knownURLs {
		if u == "" {
			continue
		}
		msg = strings.ReplaceAll(msg, u, redactedWebhookURL)
		// net/url wraps a rejected URL via %q (see url.Error.Error), which
		// re-escapes control/non-printable bytes - e.g. a raw 0x7f byte
		// becomes the four characters `\x7f` - so it no longer matches the
		// raw URL byte-for-byte. Also try the %q-quoted form.
		if quoted := strconv.Quote(u); len(quoted) >= 2 {
			msg = strings.ReplaceAll(msg, quoted[1:len(quoted)-1], redactedWebhookURL)
		}
	}
	msg = suspiciousWebhookURLPattern.ReplaceAllString(msg, redactedWebhookURL)
	return errors.New(msg)
}

// teamsMessageSender abstracts the Teams client for testability.
type teamsMessageSender interface {
	SendWithContext(ctx context.Context, webhookURL string, message goteamsnotify.TeamsMessage) error
}

// classifyTeamsError extracts HTTP status code from error message and classifies it.
// The go-teams-notify library returns errors like "error on notification: 401 Unauthorized, ...".
// This allows proper retry behavior: 4xx errors are permanent, 5xx are temporary.
func classifyTeamsError(err error) error {
	if err == nil {
		return nil
	}
	errMsg := err.Error()
	if matches := statusCodeRe.FindStringSubmatch(errMsg); len(matches) > 1 {
		if statusCode, parseErr := strconv.Atoi(matches[1]); parseErr == nil {
			return channels.ClassifySendError(statusCode, err)
		}
	}
	// Fallback: treat as temporary network error (retryable)
	return channels.ClassifyNetError(err)
}

// TeamsWebhookChannel is an output-only channel that sends messages
// to Microsoft Teams via Power Automate workflow webhooks.
// Multiple webhook targets can be configured and selected via ChatID.
type TeamsWebhookChannel struct {
	*channels.BaseChannel
	config config.TeamsWebhookConfig
	client teamsMessageSender

	// sendTimeout bounds a single webhook POST; zero means defaultSendTimeout.
	// It is only overridden by tests.
	sendTimeout time.Duration
}

// effectiveSendTimeout returns the configured per-send timeout, falling back
// to defaultSendTimeout when unset.
func (c *TeamsWebhookChannel) effectiveSendTimeout() time.Duration {
	if c.sendTimeout > 0 {
		return c.sendTimeout
	}
	return defaultSendTimeout
}

// NewTeamsWebhookChannel creates a new Teams webhook channel.
func NewTeamsWebhookChannel(
	cfg config.TeamsWebhookConfig,
	bus *bus.MessageBus,
) (*TeamsWebhookChannel, error) {
	if len(cfg.Webhooks) == 0 {
		return nil, fmt.Errorf("teams_webhook: at least one webhook target is required")
	}

	// Require "default" webhook target
	if _, hasDefault := cfg.Webhooks["default"]; !hasDefault {
		return nil, fmt.Errorf("teams_webhook: a 'default' webhook target is required")
	}

	// Validate all webhook targets have valid HTTPS URLs
	for name, target := range cfg.Webhooks {
		webhookURL := target.WebhookURL.String()
		if webhookURL == "" {
			return nil, fmt.Errorf("teams_webhook: webhook %q has empty webhook_url", name)
		}
		parsed, err := url.Parse(webhookURL)
		if err != nil {
			// url.Parse errors embed the raw string it failed on, which is the
			// webhook URL itself here.
			return nil, fmt.Errorf(
				"teams_webhook: webhook %q has invalid URL: %w",
				name, sanitizeWebhookError(err, webhookURL),
			)
		}
		if !strings.EqualFold(parsed.Scheme, "https") {
			return nil, fmt.Errorf("teams_webhook: webhook %q must use HTTPS (got %q)", name, parsed.Scheme)
		}
	}

	base := channels.NewBaseChannel(
		"teams_webhook",
		cfg,
		bus,
		[]string{
			"*",
		}, // Output-only channel; "*" suppresses misleading "allows EVERYONE" audit warning
		channels.WithMaxMessageLength(24000), // Power Automate webhook payload limit is 28KB
	)

	client := goteamsnotify.NewTeamsClient()

	return &TeamsWebhookChannel{
		BaseChannel: base,
		config:      cfg,
		client:      client,
	}, nil
}

// Start initializes the channel. For output-only channels, this is a no-op.
func (c *TeamsWebhookChannel) Start(ctx context.Context) error {
	targets := make([]string, 0, len(c.config.Webhooks))
	for name := range c.config.Webhooks {
		targets = append(targets, name)
	}
	sort.Strings(targets)
	logger.InfoCF("teams_webhook", "Starting Teams webhook channel (output-only)", map[string]any{
		"targets": targets,
	})
	c.SetRunning(true)
	return nil
}

// Stop shuts down the channel.
func (c *TeamsWebhookChannel) Stop(ctx context.Context) error {
	logger.InfoC("teams_webhook", "Stopping Teams webhook channel")
	c.SetRunning(false)
	return nil
}

// Send delivers a message to the specified Teams webhook target.
// The target is selected by msg.ChatID which must match a key in the webhooks map.
func (c *TeamsWebhookChannel) Send(ctx context.Context, msg bus.OutboundMessage) error {
	if !c.IsRunning() {
		return channels.ErrNotRunning
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	// Look up webhook target by ChatID, fall back to "default" if empty or unknown
	targetName := msg.ChatID
	if targetName == "" {
		targetName = "default"
	}

	target, ok := c.config.Webhooks[targetName]
	if !ok {
		logger.WarnCF("teams_webhook", "Unknown target, falling back to default", map[string]any{
			"requested": msg.ChatID,
			"using":     "default",
		})
		target, ok = c.config.Webhooks["default"]
	}
	// Upstream falls through with a zero-value target here, which posts to an
	// empty URL and reports success. Refuse instead: a message that silently
	// goes nowhere is worse than one that fails loudly.
	if !ok || strings.TrimSpace(target.WebhookURL.String()) == "" {
		return fmt.Errorf("teams_webhook: no webhook configured for target %q", targetName)
	}

	// Build one or more Adaptive Card messages for rich formatting. More than
	// one comes back only when the content is too large for a single card
	// even after degrading table rendering; see buildTeamsMessages.
	teamsMessages, err := c.buildTeamsMessages(msg, target)
	if err != nil {
		return fmt.Errorf("teams_webhook: failed to build card: %w", err)
	}

	// Send each card in turn. This is not atomic: if a later card fails,
	// earlier ones have already been delivered to Teams.
	webhookURL := target.WebhookURL.String()
	for i, teamsMsg := range teamsMessages {
		sendCtx, cancel := context.WithTimeout(ctx, c.effectiveSendTimeout())
		sendErr := c.client.SendWithContext(sendCtx, webhookURL, teamsMsg)
		cancel()
		if sendErr != nil {
			// Log without raw error to avoid leaking webhook URL (embedded in net/http errors)
			logger.ErrorCF("teams_webhook", "Failed to send message to Teams webhook", map[string]any{
				"target": msg.ChatID,
				"part":   i + 1,
				"parts":  len(teamsMessages),
			})
			// Sanitize before classification: the go-teams-notify client
			// embeds the raw webhook URL in its own validation errors and in
			// net/http errors, and that text must never reach a caller or log.
			sanitizedErr := sanitizeWebhookError(sendErr, webhookURL)
			// Classify error based on status code extracted from error message.
			// The go-teams-notify library includes status in errors like "401 Unauthorized".
			// Use ClassifySendError for proper retry behavior (4xx = permanent, 5xx = temporary).
			classifiedErr := classifyTeamsError(sanitizedErr)
			if len(teamsMessages) > 1 {
				return fmt.Errorf("teams_webhook: send failed on part %d/%d: %w", i+1, len(teamsMessages), classifiedErr)
			}
			return fmt.Errorf("teams_webhook: send failed: %w", classifiedErr)
		}
	}

	logger.DebugCF("teams_webhook", "Message sent successfully", map[string]any{
		"target": msg.ChatID,
		"parts":  len(teamsMessages),
	})

	return nil
}

// buildTeamsMessages builds one or more Adaptive Card messages for msg,
// degrading in steps to stay under teamsCardSizeBudget so Teams doesn't
// silently drop an oversized payload with a permanent 4xx:
//
//  1. tables rendered as native Adaptive Card Table elements (the normal,
//     best-looking path)
//  2. if that message is over budget, tables rendered as a fenced code block
//     instead - Table elements inflate roughly 10x in JSON size versus the
//     markdown they came from
//  3. if still over budget, the content is split into multiple cards at
//     paragraph boundaries (tables-as-code), sent sequentially by the caller
func (c *TeamsWebhookChannel) buildTeamsMessages(
	msg bus.OutboundMessage,
	target config.TeamsWebhookTarget,
) ([]goteamsnotify.TeamsMessage, error) {
	for _, tablesAsCode := range []bool{false, true} {
		card, err := c.buildAdaptiveCard(msg, target, tablesAsCode)
		if err != nil {
			return nil, err
		}
		size, err := cardMessageSize(card)
		if err != nil {
			return nil, err
		}
		if size <= teamsCardSizeBudget {
			teamsMsg, err := adaptivecard.NewMessageFromCard(card)
			if err != nil {
				return nil, err
			}
			return []goteamsnotify.TeamsMessage{teamsMsg}, nil
		}
	}

	return c.buildSplitTeamsMessages(msg, target)
}

// cardMessageSize measures the JSON size of the message actually sent over
// the wire (adaptivecard.NewMessageFromCard's wrapper), not just the bare
// card, since that wrapper is what counts against Teams' payload limit.
func cardMessageSize(card adaptivecard.Card) (int, error) {
	teamsMsg, err := adaptivecard.NewMessageFromCard(card)
	if err != nil {
		return 0, err
	}
	b, err := json.Marshal(teamsMsg)
	if err != nil {
		return 0, err
	}
	return len(b), nil
}

// buildSplitTeamsMessages is the last-resort degrade step: it splits msg's
// content into paragraph-sized chunks (tables rendered as code) so each
// chunk that fits on its own stays under teamsCardSizeBudget. A paragraph
// that still doesn't fit alone (an unusually large single table, say) is
// further split line-by-line via splitTextToFit.
func (c *TeamsWebhookChannel) buildSplitTeamsMessages(
	msg bus.OutboundMessage,
	target config.TeamsWebhookTarget,
) ([]goteamsnotify.TeamsMessage, error) {
	fits := func(text string) (bool, error) {
		card, err := c.buildAdaptiveCard(bus.OutboundMessage{Content: text}, target, true)
		if err != nil {
			return false, err
		}
		size, err := cardMessageSize(card)
		if err != nil {
			return false, err
		}
		return size <= teamsCardSizeBudget, nil
	}

	var chunks []string
	var current string
	for _, p := range splitIntoParagraphs(msg.Content) {
		candidate := p
		if current != "" {
			candidate = current + "\n\n" + p
		}
		ok, err := fits(candidate)
		if err != nil {
			return nil, err
		}
		if ok {
			current = candidate
			continue
		}
		if current != "" {
			chunks = append(chunks, current)
			current = ""
		}
		aloneOK, err := fits(p)
		if err != nil {
			return nil, err
		}
		if aloneOK {
			current = p
			continue
		}
		pieces, err := splitTextToFit(p, fits)
		if err != nil {
			return nil, err
		}
		chunks = append(chunks, pieces...)
	}
	if current != "" {
		chunks = append(chunks, current)
	}
	if len(chunks) == 0 {
		chunks = []string{msg.Content}
	}

	messages := make([]goteamsnotify.TeamsMessage, 0, len(chunks))
	for _, chunk := range chunks {
		chunkMsg := msg
		chunkMsg.Content = chunk
		card, err := c.buildAdaptiveCard(chunkMsg, target, true)
		if err != nil {
			return nil, err
		}
		teamsMsg, err := adaptivecard.NewMessageFromCard(card)
		if err != nil {
			return nil, err
		}
		messages = append(messages, teamsMsg)
	}
	return messages, nil
}

// splitIntoParagraphs splits content on blank-line boundaries. Markdown
// tables in this package's input are always preceded/followed by a blank
// line (see splitContentWithTables), so a table lands in exactly one
// paragraph and is never sliced in half by this pass alone.
func splitIntoParagraphs(content string) []string {
	parts := paragraphSplitRe.Split(content, -1)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if strings.TrimSpace(p) == "" {
			continue
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return []string{content}
	}
	return out
}

// splitTextToFit recursively halves text's lines until each resulting piece
// satisfies fits. It always terminates: a single line is returned as-is even
// if it still doesn't fit, since there is nothing smaller left to try.
func splitTextToFit(text string, fits func(string) (bool, error)) ([]string, error) {
	ok, err := fits(text)
	if err != nil {
		return nil, err
	}
	if ok {
		return []string{text}, nil
	}
	lines := strings.Split(text, "\n")
	if len(lines) <= 1 {
		return []string{text}, nil
	}
	mid := len(lines) / 2
	first := strings.Join(lines[:mid], "\n")
	second := strings.Join(lines[mid:], "\n")
	firstParts, err := splitTextToFit(first, fits)
	if err != nil {
		return nil, err
	}
	secondParts, err := splitTextToFit(second, fits)
	if err != nil {
		return nil, err
	}
	return append(firstParts, secondParts...), nil
}

// buildAdaptiveCard creates a formatted Adaptive Card from the outbound message.
// It detects markdown tables and converts them to native Adaptive Card Table
// elements, since TextBlocks only support a limited markdown subset (no
// tables) - unless tablesAsCode is set, in which case tables are rendered as
// a fenced code block instead. A Table element's per-cell TextBlock objects
// inflate roughly 10x in JSON size versus the markdown source, so
// buildTeamsMessages falls back to tablesAsCode when the native rendering
// pushes a message over budget.
func (c *TeamsWebhookChannel) buildAdaptiveCard(
	msg bus.OutboundMessage,
	target config.TeamsWebhookTarget,
	tablesAsCode bool,
) (adaptivecard.Card, error) {
	card := adaptivecard.NewCard()
	card.Type = adaptivecard.TypeAdaptiveCard

	// Set full width for Teams rendering
	card.MSTeams.Width = "Full"

	// Add title if configured on the target
	title := target.Title
	if title == "" {
		title = "KhunQuant Notification"
	}

	titleBlock := adaptivecard.NewTextBlock(title, true)
	titleBlock.Size = adaptivecard.SizeLarge
	titleBlock.Weight = adaptivecard.WeightBolder
	titleBlock.Style = adaptivecard.TextBlockStyleHeading

	if err := card.AddElement(false, titleBlock); err != nil {
		return card, err
	}

	content := msg.Content
	if content == "" {
		content = "(empty message)"
	}

	// Split content into text segments and tables
	// TextBlocks support: bold, italic, bullet/numbered lists, links
	// TextBlocks do NOT support: headers, tables, images
	segments := splitContentWithTables(content)

	for _, seg := range segments {
		if seg.isTable {
			if tablesAsCode {
				block := adaptivecard.NewTextBlock("```\n"+seg.content+"\n```", true)
				block.Wrap = true
				if err := card.AddElement(false, block); err != nil {
					return card, err
				}
				continue
			}
			// Convert markdown table to Adaptive Card Table element
			tableElement, err := parseMarkdownTable(seg.content)
			if err != nil {
				// Fallback: render as preformatted text if parsing fails
				logger.WarnCF("teams_webhook", "Failed to parse markdown table, using fallback", map[string]any{
					"error": err.Error(),
				})
				block := adaptivecard.NewTextBlock("```\n"+seg.content+"\n```", true)
				block.Wrap = true
				if err := card.AddElement(false, block); err != nil {
					return card, err
				}
				continue
			}
			if err := card.AddElement(false, tableElement); err != nil {
				return card, err
			}
		} else {
			// Regular text content
			text := strings.TrimSpace(seg.content)
			if text == "" {
				continue
			}
			block := adaptivecard.NewTextBlock(text, true)
			block.Wrap = true
			if err := card.AddElement(false, block); err != nil {
				return card, err
			}
		}
	}

	return card, nil
}

// contentSegment represents either a text block or a table in the message content.
type contentSegment struct {
	content string
	isTable bool
}

// splitContentWithTables splits content into alternating text and table segments.
func splitContentWithTables(content string) []contentSegment {
	var segments []contentSegment

	matches := markdownTableRe.FindAllStringSubmatchIndex(content, -1)
	if len(matches) == 0 {
		// No tables found, return entire content as text
		return []contentSegment{{content: content, isTable: false}}
	}

	lastEnd := 0
	for _, match := range matches {
		// Text before this table
		if match[0] > lastEnd {
			segments = append(segments, contentSegment{
				content: content[lastEnd:match[0]],
				isTable: false,
			})
		}
		// The table itself
		segments = append(segments, contentSegment{
			content: content[match[0]:match[1]],
			isTable: true,
		})
		lastEnd = match[1]
	}

	// Text after the last table
	if lastEnd < len(content) {
		segments = append(segments, contentSegment{
			content: content[lastEnd:],
			isTable: false,
		})
	}

	return segments
}

// parseMarkdownTable converts a markdown table string to an Adaptive Card Table element.
func parseMarkdownTable(tableStr string) (adaptivecard.Element, error) {
	lines := strings.Split(strings.TrimSpace(tableStr), "\n")
	if len(lines) < 2 {
		return adaptivecard.Element{}, fmt.Errorf("table must have at least header and separator rows")
	}

	// Track header content length per column for width calculation
	var headerLengths []int

	// Parse all rows (header + data rows, skip separator)
	var allRows [][]adaptivecard.TableCell
	for i, line := range lines {
		// Skip separator row (contains only |, -, :, and spaces)
		if i == 1 && isSeparatorRow(line) {
			continue
		}

		cells := parseTableRow(line)
		if len(cells) == 0 {
			continue
		}

		var tableCells []adaptivecard.TableCell
		for _, cellText := range cells {
			trimmedText := strings.TrimSpace(cellText)

			// Use header row (first row) to determine column widths
			if i == 0 {
				headerLengths = append(headerLengths, len(trimmedText))
			}

			textBlock := adaptivecard.Element{
				Type: adaptivecard.TypeElementTextBlock,
				Text: trimmedText,
				Wrap: true,
			}
			cell := adaptivecard.TableCell{
				Type:  adaptivecard.TypeTableCell,
				Items: []*adaptivecard.Element{&textBlock},
			}
			tableCells = append(tableCells, cell)
		}
		allRows = append(allRows, tableCells)
	}

	if len(allRows) == 0 {
		return adaptivecard.Element{}, fmt.Errorf("no valid rows found in table")
	}

	// Create table with first row as headers
	firstRowAsHeaders := true
	showGridLines := true

	table, err := adaptivecard.NewTableFromTableCells(allRows, 0, firstRowAsHeaders, showGridLines)
	if err != nil {
		return adaptivecard.Element{}, fmt.Errorf("failed to create table: %w", err)
	}

	// Set column widths based on header content length
	table.Columns = calculateColumnWidths(headerLengths)

	return table, nil
}

// calculateColumnWidths creates TableColumnDefinition entries with widths
// proportional to the max content length of each column.
func calculateColumnWidths(maxLengths []int) []adaptivecard.Column {
	if len(maxLengths) == 0 {
		return nil
	}

	// Use content length as relative weight, with a minimum of 1
	columns := make([]adaptivecard.Column, len(maxLengths))
	for i, length := range maxLengths {
		weight := length
		if weight < 1 {
			weight = 1
		}
		columns[i] = adaptivecard.Column{
			Type:  "TableColumnDefinition",
			Width: weight,
		}
	}

	return columns
}

// isSeparatorRow checks if a line is a markdown table separator (e.g., |---|---|).
func isSeparatorRow(line string) bool {
	// Remove pipes and spaces, check if only dashes and colons remain
	cleaned := strings.ReplaceAll(line, "|", "")
	cleaned = strings.ReplaceAll(cleaned, " ", "")
	cleaned = strings.ReplaceAll(cleaned, "-", "")
	cleaned = strings.ReplaceAll(cleaned, ":", "")
	return cleaned == ""
}

// parseTableRow extracts cell values from a markdown table row.
func parseTableRow(line string) []string {
	// Trim leading/trailing pipes and split by |
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(line, "|")
	line = strings.TrimSuffix(line, "|")

	if line == "" {
		return nil
	}

	parts := strings.Split(line, "|")
	var cells []string
	for _, p := range parts {
		cells = append(cells, strings.TrimSpace(p))
	}
	return cells
}
