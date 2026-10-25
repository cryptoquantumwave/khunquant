package devices

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/devices/events"
	"github.com/cryptoquantumwave/khunquant/pkg/state"
)

type fakeNotifier struct {
	mu    sync.Mutex
	can   bool
	texts []string
}

func (f *fakeNotifier) CanDeliver() bool { return f.can }

func (f *fakeNotifier) NotifyInfo(_ context.Context, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.texts = append(f.texts, text)
	return nil
}

func (f *fakeNotifier) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.texts...)
}

func newTestService(t *testing.T) (*Service, *bus.MessageBus) {
	t.Helper()
	sm := state.NewManager(t.TempDir())
	if err := sm.SetLastChannel("telegram:777"); err != nil {
		t.Fatal(err)
	}
	b := bus.NewMessageBus()
	t.Cleanup(b.Close)
	s := NewService(Config{}, sm)
	s.SetBus(b)
	return s, b
}

func usbEvent() *events.DeviceEvent {
	return &events.DeviceEvent{Action: events.ActionAdd, Kind: events.KindUSB, DeviceID: "1-2", Vendor: "ACME", Product: "Key"}
}

func TestDevices_UseNotifierWhenDeliverable(t *testing.T) {
	s, b := newTestService(t)
	n := &fakeNotifier{can: true}
	s.SetNotifier(n)

	s.sendNotification(usbEvent())

	if got := n.got(); len(got) != 1 || got[0] != usbEvent().FormatMessage() {
		t.Fatalf("notifier got %v", got)
	}
	select {
	case m := <-b.OutboundChan():
		t.Fatalf("legacy last-channel path also fired: %+v", m)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestDevices_LegacyWhenNot(t *testing.T) {
	for name, n := range map[string]Notifier{"none": nil, "cannot-deliver": &fakeNotifier{can: false}} {
		t.Run(name, func(t *testing.T) {
			s, b := newTestService(t)
			s.SetNotifier(n)

			s.sendNotification(usbEvent())

			select {
			case m := <-b.OutboundChan():
				if m.Channel != "telegram" || m.ChatID != "777" {
					t.Fatalf("legacy message to %s:%s", m.Channel, m.ChatID)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("no legacy message")
			}
			if fn, ok := n.(*fakeNotifier); ok && len(fn.got()) != 0 {
				t.Fatalf("notifier used while it cannot deliver: %v", fn.got())
			}
		})
	}
}
