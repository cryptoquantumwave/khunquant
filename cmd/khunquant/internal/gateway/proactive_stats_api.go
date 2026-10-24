package gateway

import (
	"encoding/json"
	"net/http"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
)

// busStatsResponse is the body of GET /api/proactive/stats. It is counts only:
// no event, no payload, no scope. (events.Stats has no JSON tags of its own,
// and an explicit shape here keeps a field added there from reaching the wire
// by accident.)
type busStatsResponse struct {
	Published   uint64 `json:"published"`
	Matched     uint64 `json:"matched"`
	Delivered   uint64 `json:"delivered"`
	Dropped     uint64 `json:"dropped"`
	Blocked     uint64 `json:"blocked"`
	Closed      bool   `json:"closed"`
	Subscribers int    `json:"subscribers"`

	SubscriberStats []subscriberStatsResponse `json:"subscriber_stats"`

	// Perception is present only while the perception pipeline runs. Its counters
	// are those of the current generation of services: a config reload rebuilds the
	// pipeline and they start again from zero (the bus counters above do not).
	Perception *perceptionStatsResponse `json:"perception,omitempty"`
}

type subscriberStatsResponse struct {
	ID       uint64 `json:"id"`
	Name     string `json:"name"`
	Received uint64 `json:"received"`
	Handled  uint64 `json:"handled"`
	Failed   uint64 `json:"failed"`
	Dropped  uint64 `json:"dropped"`
	Panicked uint64 `json:"panicked"`
	TimedOut uint64 `json:"timed_out"`
}

func newBusStatsResponse(s events.Stats) busStatsResponse {
	out := busStatsResponse{
		Published: s.Published, Matched: s.Matched, Delivered: s.Delivered,
		Dropped: s.Dropped, Blocked: s.Blocked, Closed: s.Closed, Subscribers: s.Subscribers,
		SubscriberStats: make([]subscriberStatsResponse, 0, len(s.SubscriberStats)),
	}
	for _, sub := range s.SubscriberStats {
		out.SubscriberStats = append(out.SubscriberStats, subscriberStatsResponse{
			ID: sub.ID, Name: sub.Name, Received: sub.Received, Handled: sub.Handled,
			Failed: sub.Failed, Dropped: sub.Dropped, Panicked: sub.Panicked, TimedOut: sub.TimedOut,
		})
	}
	return out
}

// registerProactiveStatsAPI registers GET /api/proactive/stats, the event bus's
// counters. It is guarded like every other gateway management route: loopback
// only, the management bearer token, and no browser-originated requests (see
// guardedMux). It is registered with the other routes in registerGatewayRoutes,
// so a config reload, which recreates the mux, brings it back.
//
// The handler reads the bus it was given at registration, and the shared bus is
// the same one across reloads, so the counts do not reset when the config does.
//
// perception, when given, supplies the perception pipeline's counters (nil while it
// is not running). It is a function because the route is registered before the
// pipeline is built.
func registerProactiveStatsAPI(mux handleMux, bus *events.EventBus, token string, perception ...func() *perceptionStatsResponse) {
	mux = &guardedMux{inner: mux, token: token}

	mux.Handle("GET /api/proactive/stats", loopbackOnly(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		out := newBusStatsResponse(bus.Stats())
		for _, p := range perception {
			if p != nil {
				out.Perception = p()
			}
		}
		_ = json.NewEncoder(w).Encode(out)
	})))
}
