package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"
)

// This file serves the Proactive page's dashboard: the interventions timeline, the
// metrics and the live event stream.
//
// Like the Intents, Proposals and Mandates tabs (agent_proactive_manage.go), the launcher
// opens no database for them. Each read is proxied to a gateway management route
// (cmd/khunquant/internal/gateway/dashboard_api.go), which holds the data and returns a
// projection, never rows; the management token is added here, server-side. Every route sits
// behind the launcher's dashboard authentication (IPAllowlist -> PasswordAuth ->
// SessionAuth), as the rest of /api does. All of them only read: there is nothing to
// protect with a CSRF check, and none is added.
//
// The browser's query is never forwarded as it came: only the parameters named below are
// kept, each checked, and the query is rebuilt.
//
// The live stream (GET /api/agent/proactive/events, Server-Sent Events) is built by polling
// the gateway, which has no SSE: every two seconds the launcher asks "what changed since
// this cursor" and writes each item as an event. The path ends in /events, as the JSON
// content-type middleware requires of an SSE route.

const (
	// eventsMaxStreams bounds the streams one launcher serves at once; one more is a 429.
	eventsMaxStreams = 4
	// eventsPollEvery is how often a stream asks the gateway for changes.
	eventsPollEvery = 2 * time.Second
	// eventsHeartbeatEvery keeps an idle stream (and any proxy in front of it) alive.
	eventsHeartbeatEvery = 15 * time.Second
	// eventsBatch caps the items one poll carries.
	eventsBatch = 200
	// eventsPollTimeout bounds one request to the gateway.
	eventsPollTimeout = 5 * time.Second
	// eventsRetryMs is the reconnect delay a browser is told to use.
	eventsRetryMs = 5000
)

// queryRule says whether a parameter's value is acceptable.
type queryRule func(string) bool

func matches(re string) queryRule {
	rx := regexp.MustCompile(re)
	return rx.MatchString
}

func intBetween(lo, hi int) queryRule {
	return func(s string) bool {
		n, err := strconv.Atoi(s)
		return err == nil && n >= lo && n <= hi
	}
}

func rfc3339(s string) bool {
	_, err := time.Parse(time.RFC3339, s)
	return err == nil
}

var (
	interventionsQuery = map[string]queryRule{
		"from": rfc3339, "to": rfc3339,
		"disposition": matches(`^[a-z_]{1,32}$`),
		"trigger":     matches(`^[A-Za-z0-9_.-]{1,64}$`),
		"cursor":      matches(`^[0-9.]{1,80}$`),
		"limit":       intBetween(1, eventsBatch),
	}
	metricsQuery = map[string]queryRule{"days": intBetween(1, 90)}
	cursorRule   = matches(`^[0-9.]{1,80}$`)
)

func (h *Handler) registerAgentProactiveDashboardRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/agent/proactive/interventions", h.dashboardGet("/api/proactive/interventions", interventionsQuery))
	mux.HandleFunc("GET /api/agent/proactive/metrics", h.dashboardGet("/api/proactive/metrics", metricsQuery))
	mux.HandleFunc("GET /api/agent/proactive/state", h.dashboardGet("/api/proactive/state", nil))
	mux.HandleFunc("GET /api/agent/proactive/events", h.handleProactiveEvents)
}

// dashboardGet proxies a read, forwarding only the parameters in rules, each valid.
func (h *Handler) dashboardGet(path string, rules map[string]queryRule) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := url.Values{}
		for name, ok := range rules {
			if v := r.URL.Query().Get(name); v != "" {
				if !ok(v) {
					writeJSON(w, http.StatusBadRequest, map[string]string{"error": "the " + name + " parameter is not valid"})
					return
				}
				q.Set(name, v)
			}
		}
		target := path
		if len(q) > 0 {
			target += "?" + q.Encode()
		}
		h.proxyManage(w, r, target)
	}
}

// ---- the live stream --------------------------------------------------------

// acquireStream takes one of the stream slots, or reports that none is free.
func (s *proactiveState) acquireStream() bool {
	s.streamMu.Lock()
	defer s.streamMu.Unlock()
	if s.streams >= eventsMaxStreams {
		return false
	}
	s.streams++
	return true
}

func (s *proactiveState) releaseStream() {
	s.streamMu.Lock()
	s.streams--
	s.streamMu.Unlock()
}

// shutdownCh is closed when the launcher shuts down (Handler.Close).
func (s *proactiveState) shutdownCh() chan struct{} {
	s.streamMu.Lock()
	defer s.streamMu.Unlock()
	if s.shutdown == nil {
		s.shutdown = make(chan struct{})
	}
	return s.shutdown
}

// closeStreams ends every stream, now and for good. Safe to call more than once.
func (s *proactiveState) closeStreams() {
	ch := s.shutdownCh()
	s.streamMu.Lock()
	defer s.streamMu.Unlock()
	select {
	case <-ch:
	default:
		close(ch)
	}
}

func (s *proactiveState) intervals() (poll, heartbeat time.Duration) {
	poll, heartbeat = s.pollEvery, s.heartbeatEvery
	if poll <= 0 {
		poll = eventsPollEvery
	}
	if heartbeat <= 0 {
		heartbeat = eventsHeartbeatEvery
	}
	return poll, heartbeat
}

// feedReply is the gateway's /api/proactive/changes answer, as far as the stream reads it.
type feedReply struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason"`
	Cursor    string `json:"cursor"`
	Items     []struct {
		Type   string          `json:"type"`
		Cursor string          `json:"cursor"`
		Data   json.RawMessage `json:"data"`
	} `json:"items"`
}

var eventTypeRule = matches(`^(attention\.decision|approval\.[a-z_]{1,32})$`)

// pollChanges asks the gateway for the changes after cursor.
func (h *Handler) pollChanges(ctx context.Context, cursor string) (feedReply, error) {
	var reply feedReply
	base, token, err := h.gatewayManagementAPI()
	if err != nil {
		return reply, err
	}
	ctx, cancel := context.WithTimeout(ctx, eventsPollTimeout)
	defer cancel()
	target := base + "/api/proactive/changes?limit=" + strconv.Itoa(eventsBatch)
	if cursor != "" {
		target += "&cursor=" + url.QueryEscape(cursor)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return reply, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return reply, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return reply, fmt.Errorf("the gateway answered %d", resp.StatusCode)
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&reply)
	return reply, err
}

// handleProactiveEvents streams "attention.decision" and "approval.<status>" events, each
// carrying the same projection as the interventions route, with the feed's cursor as the
// event id so that a reconnect (Last-Event-ID, or ?cursor=) resumes without a gap or a
// repeat. It ends with the client, with the launcher's shutdown, or when a write fails.
//
// Beyond the items there is one more event, "stream.status", with {"state": "live"} or
// {"state": "unavailable", "reason": ...}, sent when the gateway becomes unreachable or
// returns: the stream itself stays open meanwhile.
func (h *Handler) handleProactiveEvents(w http.ResponseWriter, r *http.Request) {
	if !h.proactive.acquireStream() {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": fmt.Sprintf("at most %d live streams at once", eventsMaxStreams)})
		return
	}
	defer h.proactive.releaseStream()

	cursor := r.URL.Query().Get("cursor")
	if cursor == "" {
		cursor = r.Header.Get("Last-Event-ID")
	}
	if cursor != "" && !cursorRule(cursor) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "the cursor is not valid"})
		return
	}
	rc := http.NewResponseController(w)
	// The server's write timeout would end every stream after a minute.
	_ = rc.SetWriteDeadline(time.Time{})
	hd := w.Header()
	hd.Set("Content-Type", "text/event-stream")
	hd.Set("Cache-Control", "no-cache, no-store")
	hd.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	send := func(format string, a ...any) bool {
		if _, err := fmt.Fprintf(w, format, a...); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	if !send("retry: %d\n: connected\n\n", eventsRetryMs) {
		return
	}

	pollEvery, heartbeatEvery := h.proactive.intervals()
	poll, heartbeat := time.NewTicker(pollEvery), time.NewTicker(heartbeatEvery)
	defer poll.Stop()
	defer heartbeat.Stop()
	shutdown := h.proactive.shutdownCh()
	state := ""
	setState := func(next, reason string) bool {
		if next == state {
			return true
		}
		state = next
		body, _ := json.Marshal(map[string]string{"state": next, "reason": reason})
		return send("event: stream.status\ndata: %s\n\n", body)
	}

	for {
		reply, err := h.pollChanges(r.Context(), cursor)
		switch {
		case r.Context().Err() != nil:
			return
		case err != nil:
			if !setState("unavailable", "the gateway could not be reached") {
				return
			}
		case !reply.Available:
			if !setState("unavailable", reply.Reason) {
				return
			}
		default:
			if !setState("live", "") {
				return
			}
			items := reply.Items
			if len(items) > eventsBatch {
				items = items[:eventsBatch]
			}
			for _, it := range items {
				var data bytes.Buffer
				// The event name and id are written into a line-based format: only the
				// shapes the gateway produces go through, never free text.
				if !eventTypeRule(it.Type) || !cursorRule(it.Cursor) || json.Compact(&data, it.Data) != nil {
					continue
				}
				if !send("id: %s\nevent: %s\ndata: %s\n\n", it.Cursor, it.Type, data.Bytes()) {
					return
				}
				cursor = it.Cursor
			}
			if len(items) == 0 && cursorRule(reply.Cursor) {
				cursor = reply.Cursor // the head, for a stream that started without one
			}
		}
	wait:
		for {
			select {
			case <-r.Context().Done():
				return
			case <-shutdown:
				return
			case <-heartbeat.C:
				if !send(": heartbeat\n\n") {
					return
				}
			case <-poll.C:
				break wait
			}
		}
	}
}
