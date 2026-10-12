package devices

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/constants"
	"github.com/cryptoquantumwave/khunquant/pkg/devices/events"
	"github.com/cryptoquantumwave/khunquant/pkg/devices/sources"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/state"
)

// Notifier is the owner-routed delivery path (pkg/notify). It is declared here
// so that pkg/devices does not import pkg/notify; the gateway adapts the real
// service to it.
type Notifier interface {
	// CanDeliver reports whether a notification would currently reach the
	// owner.
	CanDeliver() bool
	// NotifyInfo sends text to the owner as a low-priority, silent message.
	NotifyInfo(ctx context.Context, text string) error
}

type Service struct {
	bus     *bus.MessageBus
	notify  Notifier
	state   *state.Manager
	sources []events.EventSource
	enabled bool
	ctx     context.Context
	cancel  context.CancelFunc
	mu      sync.RWMutex
}

type Config struct {
	Enabled    bool
	MonitorUSB bool // When true, monitor USB hotplug (Linux only)
	// Future: MonitorBluetooth, MonitorPCI, etc.
}

func NewService(cfg Config, stateMgr *state.Manager) *Service {
	s := &Service{
		state:   stateMgr,
		enabled: cfg.Enabled,
		sources: make([]EventSource, 0),
	}

	if cfg.Enabled && cfg.MonitorUSB {
		s.sources = append(s.sources, sources.NewUSBMonitor())
	}

	return s
}

func (s *Service) SetBus(msgBus *bus.MessageBus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bus = msgBus
}

// SetNotifier routes device events through n whenever n can deliver. Without
// one, or while it cannot deliver, events keep going to the last active
// channel as before. Pass nil to remove it.
func (s *Service) SetNotifier(n Notifier) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notify = n
}

func (s *Service) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.enabled || len(s.sources) == 0 {
		logger.InfoC("devices", "Device event service disabled or no sources")
		return nil
	}

	s.ctx, s.cancel = context.WithCancel(ctx)

	for _, src := range s.sources {
		eventCh, err := src.Start(s.ctx)
		if err != nil {
			logger.ErrorCF("devices", "Failed to start source", map[string]any{
				"kind":  src.Kind(),
				"error": err.Error(),
			})
			continue
		}
		go s.handleEvents(src.Kind(), eventCh)
		logger.InfoCF("devices", "Device source started", map[string]any{
			"kind": src.Kind(),
		})
	}

	logger.InfoC("devices", "Device event service started")
	return nil
}

func (s *Service) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}

	for _, src := range s.sources {
		src.Stop()
	}

	logger.InfoC("devices", "Device event service stopped")
}

func (s *Service) handleEvents(kind events.Kind, eventCh <-chan *events.DeviceEvent) {
	for ev := range eventCh {
		if ev == nil {
			continue
		}
		s.sendNotification(ev)
	}
}

func (s *Service) sendNotification(ev *events.DeviceEvent) {
	s.mu.RLock()
	msgBus := s.bus
	notifier := s.notify
	s.mu.RUnlock()

	// With an owner route the event goes to the owner's direct chat and the
	// last active chat is not consulted at all.
	if notifier != nil && notifier.CanDeliver() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := notifier.NotifyInfo(ctx, ev.FormatMessage()); err != nil {
			logger.WarnCF("devices", "Device notification via owner route failed", map[string]any{
				"kind":   ev.Kind,
				"action": ev.Action,
				"error":  err.Error(),
			})
		}
		return
	}

	if msgBus == nil {
		return
	}

	var lastChannel string
	if s.state != nil {
		lastChannel = s.state.GetLastChannel()
	}
	if lastChannel == "" {
		logger.DebugCF("devices", "No last channel, skipping notification", map[string]any{
			"event": ev.FormatMessage(),
		})
		return
	}

	platform, userID := parseLastChannel(lastChannel)
	if platform == "" || userID == "" || constants.IsInternalChannel(platform) {
		return
	}

	msg := ev.FormatMessage()
	pubCtx, pubCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer pubCancel()
	msgBus.PublishOutbound(pubCtx, bus.OutboundMessage{
		Channel: platform,
		ChatID:  userID,
		Content: msg,
	})

	logger.InfoCF("devices", "Device notification sent", map[string]any{
		"kind":   ev.Kind,
		"action": ev.Action,
		"to":     platform,
	})
}

func parseLastChannel(lastChannel string) (platform, userID string) {
	if lastChannel == "" {
		return "", ""
	}
	parts := strings.SplitN(lastChannel, ":", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", ""
	}
	return parts[0], parts[1]
}
