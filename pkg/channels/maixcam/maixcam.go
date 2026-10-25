package maixcam

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/channels"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/identity"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
)

type MaixCamChannel struct {
	*channels.BaseChannel
	config     config.MaixCamConfig
	listener   net.Listener
	ctx        context.Context
	cancel     context.CancelFunc
	clients    map[net.Conn]bool
	clientsMux sync.RWMutex
}

// H-12: the channel listened on 0.0.0.0 with no authentication. Anyone on the
// network could connect, inject text into an agent turn via class_name, and -
// because Send broadcasts to every connected client - read every reply the
// agent sent on this channel. A device must now authenticate first when a
// token is configured, and a token is required to listen beyond loopback.

const (
	// maxMaixCamMessageBytes caps one JSON message. Detection events are a
	// few hundred bytes; without a cap a client could stream one unbounded
	// object into memory.
	maxMaixCamMessageBytes = 64 << 10
	// authTimeout bounds how long an unauthenticated connection may idle.
	authTimeout = 10 * time.Second
	// maxClassNameLen bounds the detector label copied into agent input.
	maxClassNameLen = 64
)

var errMessageTooLarge = errors.New("maixcam message exceeds size limit")

type MaixCamMessage struct {
	Type      string         `json:"type"`
	Token     string         `json:"token,omitempty"`
	Tips      string         `json:"tips"`
	Timestamp float64        `json:"timestamp"`
	Data      map[string]any `json:"data"`
}

func NewMaixCamChannel(cfg config.MaixCamConfig, bus *bus.MessageBus) (*MaixCamChannel, error) {
	base := channels.NewBaseChannel(
		"maixcam",
		cfg,
		bus,
		cfg.AllowFrom,
		channels.WithReasoningChannelID(cfg.ReasoningChannelID),
	)

	return &MaixCamChannel{
		BaseChannel: base,
		config:      cfg,
		clients:     make(map[net.Conn]bool),
	}, nil
}

func (c *MaixCamChannel) Start(ctx context.Context) error {
	logger.InfoC("maixcam", "Starting MaixCam channel server")

	if c.config.Token == "" && !isLoopbackHost(c.config.Host) {
		return fmt.Errorf("maixcam: refusing to listen on %q without channels.maixcam.token; "+
			"set a token (and send it from the device) or bind to 127.0.0.1", c.config.Host)
	}

	c.ctx, c.cancel = context.WithCancel(ctx)

	addr := net.JoinHostPort(c.config.Host, fmt.Sprintf("%d", c.config.Port))
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		c.cancel()
		return fmt.Errorf("failed to listen on %s: %w", addr, err)
	}

	c.listener = listener
	c.SetRunning(true)

	logger.InfoCF("maixcam", "MaixCam server listening", map[string]any{
		"host": c.config.Host,
		"port": c.config.Port,
	})

	go c.acceptConnections()

	return nil
}

func (c *MaixCamChannel) acceptConnections() {
	logger.DebugC("maixcam", "Starting connection acceptor")

	for {
		select {
		case <-c.ctx.Done():
			logger.InfoC("maixcam", "Stopping connection acceptor")
			return
		default:
			conn, err := c.listener.Accept()
			if err != nil {
				if c.IsRunning() {
					logger.ErrorCF("maixcam", "Failed to accept connection", map[string]any{
						"error": err.Error(),
					})
				}
				return
			}

			logger.InfoCF("maixcam", "New connection from MaixCam device", map[string]any{
				"remote_addr": conn.RemoteAddr().String(),
			})

			go c.handleConnection(conn)
		}
	}
}

func (c *MaixCamChannel) handleConnection(conn net.Conn) {
	logger.DebugC("maixcam", "Handling MaixCam connection")

	defer func() {
		conn.Close()
		c.clientsMux.Lock()
		delete(c.clients, conn)
		c.clientsMux.Unlock()
		logger.DebugC("maixcam", "Connection closed")
	}()

	limited := &capReader{r: conn, max: maxMaixCamMessageBytes}
	decoder := json.NewDecoder(limited)

	if !c.authenticate(conn, decoder, limited) {
		return
	}

	// Registered only once authenticated: Send broadcasts to every client
	// in this map, so an unauthenticated one would receive agent replies.
	c.clientsMux.Lock()
	c.clients[conn] = true
	c.clientsMux.Unlock()

	for {
		select {
		case <-c.ctx.Done():
			return
		default:
			var msg MaixCamMessage
			if err := decoder.Decode(&msg); err != nil {
				if err.Error() != "EOF" {
					logger.ErrorCF("maixcam", "Failed to decode message", map[string]any{
						"error": err.Error(),
					})
				}
				return
			}

			limited.reset()
			c.processMessage(msg, conn)
		}
	}
}

// authenticate reads the first message and checks the token. With no token
// configured (which Start permits only on loopback) every connection is
// accepted and its first message is processed normally.
func (c *MaixCamChannel) authenticate(conn net.Conn, decoder *json.Decoder, limited *capReader) bool {
	if c.config.Token == "" {
		return true
	}

	_ = conn.SetReadDeadline(time.Now().Add(authTimeout))
	var msg MaixCamMessage
	err := decoder.Decode(&msg)
	_ = conn.SetReadDeadline(time.Time{})
	limited.reset()

	if err != nil || msg.Type != "auth" ||
		subtle.ConstantTimeCompare([]byte(msg.Token), []byte(c.config.Token)) != 1 {
		logger.WarnCF("maixcam", "Rejected unauthenticated MaixCam connection",
			map[string]any{"remote_addr": conn.RemoteAddr().String()})
		return false
	}
	return true
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// capReader fails once more than max bytes are read between resets. The
// decoder buffers ahead, so the count can include the start of the next
// message; that only makes the cap slightly stricter.
type capReader struct {
	r   io.Reader
	n   int
	max int
}

func (c *capReader) Read(p []byte) (int, error) {
	if c.n >= c.max {
		return 0, errMessageTooLarge
	}
	if room := c.max - c.n; len(p) > room {
		p = p[:room]
	}
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

func (c *capReader) reset() { c.n = 0 }

// sanitizeClassName makes a detector label safe to place in agent input: the
// device is a network peer, so its label is untrusted text.
func sanitizeClassName(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == ' ' || r == '-' || r == '_' || r == '.' {
			return r
		}
		return -1
	}, s)
	s = strings.TrimSpace(s)
	if runes := []rune(s); len(runes) > maxClassNameLen {
		s = string(runes[:maxClassNameLen])
	}
	if s == "" {
		return "person"
	}
	return s
}

func (c *MaixCamChannel) processMessage(msg MaixCamMessage, conn net.Conn) {
	switch msg.Type {
	case "person_detected":
		c.handlePersonDetection(msg)
	case "heartbeat":
		logger.DebugC("maixcam", "Received heartbeat")
	case "status":
		c.handleStatusUpdate(msg)
	default:
		logger.WarnCF("maixcam", "Unknown message type", map[string]any{
			"type": msg.Type,
		})
	}
}

func (c *MaixCamChannel) handlePersonDetection(msg MaixCamMessage) {
	logger.InfoCF("maixcam", "", map[string]any{
		"timestamp": msg.Timestamp,
		"data":      msg.Data,
	})

	senderID := "maixcam"
	chatID := "default"

	rawClass, _ := msg.Data["class_name"].(string)
	classInfo := sanitizeClassName(rawClass)

	score, _ := msg.Data["score"].(float64)
	x, _ := msg.Data["x"].(float64)
	y, _ := msg.Data["y"].(float64)
	w, _ := msg.Data["w"].(float64)
	h, _ := msg.Data["h"].(float64)

	content := fmt.Sprintf("📷 Person detected!\nClass: %s\nConfidence: %.2f%%\nPosition: (%.0f, %.0f)\nSize: %.0fx%.0f",
		classInfo, score*100, x, y, w, h)

	metadata := map[string]string{
		"timestamp": fmt.Sprintf("%.0f", msg.Timestamp),
		"class_id":  fmt.Sprintf("%.0f", msg.Data["class_id"]),
		"score":     fmt.Sprintf("%.2f", score),
		"x":         fmt.Sprintf("%.0f", x),
		"y":         fmt.Sprintf("%.0f", y),
		"w":         fmt.Sprintf("%.0f", w),
		"h":         fmt.Sprintf("%.0f", h),
	}

	sender := bus.SenderInfo{
		Platform:    "maixcam",
		PlatformID:  "maixcam",
		CanonicalID: identity.BuildCanonicalID("maixcam", "maixcam"),
	}

	if !c.IsAllowedSender(sender) {
		return
	}

	c.HandleMessage(
		c.ctx,
		bus.Peer{Kind: "channel", ID: "default"},
		"",
		senderID,
		chatID,
		content,
		[]string{},
		metadata,
		sender,
	)
}

func (c *MaixCamChannel) handleStatusUpdate(msg MaixCamMessage) {
	logger.InfoCF("maixcam", "Status update from MaixCam", map[string]any{
		"status": msg.Data,
	})
}

func (c *MaixCamChannel) Stop(ctx context.Context) error {
	logger.InfoC("maixcam", "Stopping MaixCam channel")
	c.SetRunning(false)

	// Cancel context first to signal goroutines to exit
	if c.cancel != nil {
		c.cancel()
	}

	if c.listener != nil {
		c.listener.Close()
	}

	c.clientsMux.Lock()
	defer c.clientsMux.Unlock()

	for conn := range c.clients {
		conn.Close()
	}
	c.clients = make(map[net.Conn]bool)

	logger.InfoC("maixcam", "MaixCam channel stopped")
	return nil
}

func (c *MaixCamChannel) Send(ctx context.Context, msg bus.OutboundMessage) error {
	if !c.IsRunning() {
		return channels.ErrNotRunning
	}

	// Check ctx before entering write path
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	c.clientsMux.RLock()
	defer c.clientsMux.RUnlock()

	if len(c.clients) == 0 {
		logger.WarnC("maixcam", "No MaixCam devices connected")
		return fmt.Errorf("no connected MaixCam devices")
	}

	response := map[string]any{
		"type":      "command",
		"timestamp": float64(0),
		"message":   msg.Content,
		"chat_id":   msg.ChatID,
	}

	data, err := json.Marshal(response)
	if err != nil {
		return fmt.Errorf("failed to marshal response: %w", err)
	}

	var sendErr error
	for conn := range c.clients {
		_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if _, err := conn.Write(data); err != nil {
			logger.ErrorCF("maixcam", "Failed to send to client", map[string]any{
				"client": conn.RemoteAddr().String(),
				"error":  err.Error(),
			})
			sendErr = fmt.Errorf("maixcam send: %w", channels.ErrTemporary)
		}
		_ = conn.SetWriteDeadline(time.Time{})
	}

	return sendErr
}
