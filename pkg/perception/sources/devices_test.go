package sources_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	devevents "github.com/cryptoquantumwave/khunquant/pkg/devices/events"
	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/perceptiontest"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/sources"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

var epoch = time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)

// fakeMonitor is a devices/events.EventSource driven by the test.
type fakeMonitor struct {
	mu       sync.Mutex
	ch       chan *devevents.DeviceEvent
	startErr error
	starts   int
	stops    int
}

func newMonitor() *fakeMonitor { return &fakeMonitor{ch: make(chan *devevents.DeviceEvent, 8)} }

func (f *fakeMonitor) Kind() devevents.Kind { return devevents.KindUSB }

func (f *fakeMonitor) Start(context.Context) (<-chan *devevents.DeviceEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts++
	if f.startErr != nil {
		return nil, f.startErr
	}
	return f.ch, nil
}

func (f *fakeMonitor) Stop() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops++
	return nil
}

func (f *fakeMonitor) counts() (starts, stops int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.starts, f.stops
}

// collector is a perception.Emitter that keeps what it is given.
type collector struct {
	mu  sync.Mutex
	got []events.Event
}

func (c *collector) Emit(ev events.Event) {
	c.mu.Lock()
	c.got = append(c.got, ev)
	c.mu.Unlock()
}
func (c *collector) EmitLatest(ev events.Event) { c.Emit(ev) }
func (c *collector) EmitNonBlocking(ev events.Event) bool {
	c.Emit(ev)
	return true
}
func (c *collector) events() []events.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]events.Event(nil), c.got...)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestDevicesAdapter(t *testing.T) {
	mon := newMonitor()
	src := sources.NewDevices(mon, func() time.Time { return epoch })
	if src.Name() != "devices:usb" {
		t.Fatalf("name = %q", src.Name())
	}
	out := &collector{}
	m := perception.NewManager(out)
	m.Add(src)
	m.Start(context.Background())

	mon.ch <- &devevents.DeviceEvent{
		Action: devevents.ActionAdd, Kind: devevents.KindUSB, DeviceID: "1-2",
		Vendor: "Acme\x00\n", Product: "Widget\u202e", Serial: "S1", Capabilities: "storage",
		Raw: map[string]string{"k\n": "v\x07"},
	}
	mon.ch <- nil // a nil event is skipped
	mon.ch <- &devevents.DeviceEvent{Action: devevents.ActionRemove, Kind: devevents.KindUSB, DeviceID: "1-2"}
	waitFor(t, "two events", func() bool { return len(out.events()) == 2 })

	add := out.events()[0]
	if add.Kind != events.KindPerceptionDevice || add.Source.Name != "devices:usb" || !add.Time.Equal(epoch) {
		t.Fatalf("event = %+v", add)
	}
	if add.Trust == nil || add.TrustLevel() != proactive.TrustUntrusted {
		t.Fatalf("trust = %v: text off a device must be untrusted", add.Trust)
	}
	p, ok := add.Payload.(sources.DevicePayload)
	if !ok {
		t.Fatalf("payload is %T", add.Payload)
	}
	if p.Action != "add" || p.Kind != "usb" || p.DeviceID != "1-2" || p.Vendor != "Acme" || p.Product != "Widget" ||
		p.Serial != "S1" || p.Capabilities != "storage" || p.Raw["k"] != "v" {
		t.Fatalf("payload = %+v", p)
	}
	if perception.SubjectOf(add) != "usb:1-2" {
		t.Fatalf("subject = %q", perception.SubjectOf(add))
	}
	if rm := out.events()[1]; rm.Payload.(sources.DevicePayload).Action != "remove" {
		t.Fatalf("second event = %+v", rm)
	}
	if h := src.Health(); !h.LastEventAt.Equal(epoch) {
		t.Fatalf("the source's own health = %+v", h)
	}
	if h := m.Health()["devices:usb"]; h.LastEventAt.IsZero() {
		t.Fatalf("the manager's health = %+v", h)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if starts, stops := mon.counts(); starts != 1 || stops != 1 {
		t.Fatalf("monitor started %d, stopped %d times", starts, stops)
	}
}

func TestDevicesAdapter_BoundsWhatADeviceSays(t *testing.T) {
	mon := newMonitor()
	src := sources.NewDevices(mon, nil)
	out := &collector{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- src.Start(ctx, out) }()

	raw := map[string]string{}
	for i := 0; i < 100; i++ {
		raw[strings.Repeat("k", i+1)] = "v"
	}
	mon.ch <- &devevents.DeviceEvent{DeviceID: "d", Vendor: strings.Repeat("x", 5000), Raw: raw}
	waitFor(t, "the event", func() bool { return len(out.events()) == 1 })
	p := out.events()[0].Payload.(sources.DevicePayload)
	if len([]rune(p.Vendor)) != 128 || len(p.Raw) != 32 {
		t.Fatalf("vendor %d runes, %d raw entries: not bounded", len([]rune(p.Vendor)), len(p.Raw))
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestDevicesAdapter_ClosedChannelIsNotAFailure(t *testing.T) {
	mon := newMonitor()
	close(mon.ch) // the non-Linux USB monitor does this at once
	clock := perceptiontest.NewClock(epoch)
	m := perception.NewManager(&collector{}, perception.WithClock(clock))
	m.Add(sources.NewDevices(mon, nil))
	m.Start(context.Background())
	time.Sleep(50 * time.Millisecond)
	clock.Advance(time.Hour)
	time.Sleep(20 * time.Millisecond)
	if starts, _ := mon.counts(); starts != 1 {
		t.Fatalf("the monitor was started %d times: a closed channel was treated as a crash", starts)
	}
	if h := m.Health()["devices:usb"]; h.ConsecutiveErrors != 0 {
		t.Fatalf("health = %+v", h)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.Stop(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestDevicesAdapter_StartFailureIsReturnedAndReconcileIsANoop(t *testing.T) {
	mon := newMonitor()
	mon.startErr = errors.New("no permission")
	src := sources.NewDevices(mon, nil)
	if err := src.Start(context.Background(), &collector{}); err == nil || !strings.Contains(err.Error(), "no permission") {
		t.Fatalf("Start = %v", err)
	}
	if err := src.Reconcile(context.Background(), &collector{}); err != nil {
		t.Fatalf("Reconcile = %v", err)
	}
}
