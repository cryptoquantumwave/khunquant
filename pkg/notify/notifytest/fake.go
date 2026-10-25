// Package notifytest holds fakes for code that delivers through pkg/notify.
//
// Service stands in for *notify.Service; Sender and Appender stand in for the
// channel manager and the agent loop when a real notify.Service is under test.
package notifytest

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/channels"
	"github.com/cryptoquantumwave/khunquant/pkg/notify"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// Service records the notifications it is asked to deliver. The zero value
// reports "disabled"; set Route (and Enabled) to make it deliver.
type Service struct {
	mu sync.Mutex

	// Route is the owner route reported by OwnerRoute and put in results.
	Route proactive.OwnerRoute
	// Enabled makes Deliver succeed; otherwise it returns Reason "disabled".
	Enabled bool
	// SessionKey is returned by OwnerSessionKey and put in results.
	SessionKey string
	// Err, when set, is returned by Deliver instead of a result.
	Err error
	// Reply, when set, replaces the default successful result.
	Reply func(notify.Notification) notify.Result

	delivered []notify.Notification
	sent      map[string]string // "channel\x00id" -> intervention ID
}

// OwnerRoute reports the configured route and whether delivery is possible.
func (f *Service) OwnerRoute() (proactive.OwnerRoute, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Route, f.Enabled
}

// CanDeliver reports whether Deliver would succeed.
func (f *Service) CanDeliver() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Enabled
}

// OwnerSessionKey returns SessionKey.
func (f *Service) OwnerSessionKey() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.SessionKey
}

// Deliver records n and returns the programmed outcome.
func (f *Service) Deliver(_ context.Context, n notify.Notification) (notify.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delivered = append(f.delivered, n)
	if f.Err != nil {
		return notify.Result{}, f.Err
	}
	if !f.Enabled {
		return notify.Result{Reason: notify.ReasonDisabled}, nil
	}
	if f.Reply != nil {
		return f.Reply(n), nil
	}
	id := n.InterventionID
	if id == "" {
		id = proactive.NewID()
	}
	return notify.Result{
		Delivered:      true,
		Route:          f.Route,
		SessionKey:     f.SessionKey,
		InterventionID: id,
	}, nil
}

// LookupByPlatformMessageID answers from the pairs registered with Remember.
func (f *Service) LookupByPlatformMessageID(_ context.Context, channel, platformMsgID string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.sent[channel+"\x00"+platformMsgID]
	return id, ok, nil
}

// Remember registers a (channel, platform message ID) -> intervention pair for
// LookupByPlatformMessageID.
func (f *Service) Remember(channel, platformMsgID, interventionID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sent == nil {
		f.sent = map[string]string{}
	}
	f.sent[channel+"\x00"+platformMsgID] = interventionID
}

// Delivered returns a copy of every notification passed to Deliver.
func (f *Service) Delivered() []notify.Notification {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]notify.Notification(nil), f.delivered...)
}

// Sender is a notify.Sender that records messages and returns a programmed
// receipt. By default every send succeeds with one platform message ID.
type Sender struct {
	mu   sync.Mutex
	msgs []bus.OutboundMessage

	// Err, when set, fails every send.
	Err error
	// IDs overrides the reported platform message IDs; nil means one generated
	// ID per send, and an empty non-nil slice means none. With Err set, IDs are
	// the chunks that were delivered before the failure (a partial delivery).
	IDs []string

	seq int
}

// SendAndReport records msg and reports the programmed outcome.
func (s *Sender) SendAndReport(_ context.Context, msg bus.OutboundMessage) channels.DeliveryReceipt {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.msgs = append(s.msgs, msg)
	r := channels.DeliveryReceipt{Msg: msg, At: time.Now()}
	if s.Err != nil {
		// IDs set together with Err model a partial delivery: the chunks that
		// went out are still reported.
		r.Err = s.Err
		r.PlatformMessageIDs = append([]string(nil), s.IDs...)
		return r
	}
	if s.IDs != nil {
		r.PlatformMessageIDs = append([]string(nil), s.IDs...)
		return r
	}
	s.seq++
	r.PlatformMessageIDs = []string{"m" + strconv.Itoa(s.seq)}
	return r
}

// Sent returns a copy of the messages sent so far.
func (s *Sender) Sent() []bus.OutboundMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]bus.OutboundMessage(nil), s.msgs...)
}

// Append is one recorded session write.
type Append struct {
	SessionKey string
	Content    string
}

// Appender is a notify.SessionAppender that records writes. Set Block to hold
// every write until the channel is closed (or the context ends), the way a
// busy session does.
type Appender struct {
	mu      sync.Mutex
	appends []Append

	// Block, when non-nil, is waited on before a write is applied.
	Block chan struct{}
	// Err fails every write.
	Err error
}

// AppendAssistantMessage records the write once Block (if any) is released.
func (a *Appender) AppendAssistantMessage(ctx context.Context, sessionKey, content string) error {
	if a.Block != nil {
		select {
		case <-a.Block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.Err != nil {
		return a.Err
	}
	a.appends = append(a.appends, Append{SessionKey: sessionKey, Content: content})
	return nil
}

// Appends returns a copy of the writes applied so far.
func (a *Appender) Appends() []Append {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]Append(nil), a.appends...)
}
