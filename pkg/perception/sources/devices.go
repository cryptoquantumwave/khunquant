// Package sources holds the perception sources that wrap existing event
// producers. Each real source (market data, portfolio, calendar) lives in its own
// file as it lands.
package sources

import (
	"context"
	"strings"
	"sync"
	"time"
	"unicode"

	devevents "github.com/cryptoquantumwave/khunquant/pkg/devices/events"
	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

const (
	// maxDeviceText bounds each descriptor string copied off a device, and
	// maxDeviceRaw the number of raw properties: a device chooses its own names.
	maxDeviceText = 128
	maxDeviceRaw  = 32
)

// DevicePayload is the Payload of a perception.device event: what the device
// reported, bounded and with control characters removed.
type DevicePayload struct {
	Action       string            `json:"action"`
	Kind         string            `json:"kind"`
	DeviceID     string            `json:"device_id"`
	Vendor       string            `json:"vendor,omitempty"`
	Product      string            `json:"product,omitempty"`
	Serial       string            `json:"serial,omitempty"`
	Capabilities string            `json:"capabilities,omitempty"`
	Raw          map[string]string `json:"raw,omitempty"`
}

// Devices adapts a devices/events.EventSource (the legacy device monitors) to a
// perception.Source: every device event becomes a perception.device event.
//
// The strings in a device event come from the device's own descriptors, so the
// event is TrustUntrusted: a name chosen by whoever made the hardware must never
// pass for something KhunQuant computed.
type Devices struct {
	src devevents.EventSource
	now func() time.Time

	mu      sync.Mutex
	lastEvt time.Time
}

// NewDevices wraps src. now may be nil (time.Now).
func NewDevices(src devevents.EventSource, now func() time.Time) *Devices {
	if now == nil {
		//simclock:wall the default when no clock is injected
		now = time.Now
	}
	return &Devices{src: src, now: now}
}

// Name is "devices:" and the monitor's kind ("devices:usb").
func (d *Devices) Name() string { return "devices:" + string(d.src.Kind()) }

// Start starts the monitor and forwards its events until ctx is done, then stops
// it. It blocks, which the Manager allows. A monitor whose channel closes before
// ctx is done has nothing more to say (the non-Linux USB monitor closes its channel
// at once): that is not a failure, so the adapter waits for ctx instead of making
// the Manager restart it forever.
func (d *Devices) Start(ctx context.Context, out perception.Emitter) error {
	ch, err := d.src.Start(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = d.src.Stop() }()
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-ch:
			if !ok {
				logger.DebugCF("perception", "Device monitor has no more events", map[string]any{"source": d.Name()})
				<-ctx.Done()
				return nil
			}
			if ev == nil {
				continue
			}
			out.Emit(d.event(ev))
		}
	}
}

// Reconcile does nothing: a hotplug monitor has no authoritative state to re-read.
func (d *Devices) Reconcile(context.Context, perception.Emitter) error { return nil }

// Health reports when the last device event was seen. A hotplug monitor has no
// regular rhythm.
func (d *Devices) Health() perception.SourceHealth {
	d.mu.Lock()
	defer d.mu.Unlock()
	return perception.SourceHealth{LastEventAt: d.lastEvt}
}

func (d *Devices) event(ev *devevents.DeviceEvent) events.Event {
	now := d.now()
	d.mu.Lock()
	d.lastEvt = now
	d.mu.Unlock()

	p := DevicePayload{
		Action:       cleanText(string(ev.Action)),
		Kind:         cleanText(string(ev.Kind)),
		DeviceID:     cleanText(ev.DeviceID),
		Vendor:       cleanText(ev.Vendor),
		Product:      cleanText(ev.Product),
		Serial:       cleanText(ev.Serial),
		Capabilities: cleanText(ev.Capabilities),
	}
	if len(ev.Raw) > 0 {
		p.Raw = make(map[string]string, min(len(ev.Raw), maxDeviceRaw))
		for k, v := range ev.Raw {
			if len(p.Raw) >= maxDeviceRaw {
				break
			}
			p.Raw[cleanText(k)] = cleanText(v)
		}
	}
	return events.Event{
		Kind:     events.KindPerceptionDevice,
		Time:     now,
		Source:   events.Source{Component: "perception", Name: d.Name()},
		Severity: events.SeverityInfo,
		Payload:  p,
		Attrs:    map[string]any{"subject": p.Kind + ":" + p.DeviceID, "action": p.Action},
		Trust:    events.TrustPtr(proactive.TrustUntrusted),
	}
}

// cleanText removes control and invisible format characters and cuts s to
// maxDeviceText runes.
func cleanText(s string) string {
	s = strings.ToValidUTF8(s, "")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, s)
	if r := []rune(s); len(r) > maxDeviceText {
		s = string(r[:maxDeviceText])
	}
	return s
}
