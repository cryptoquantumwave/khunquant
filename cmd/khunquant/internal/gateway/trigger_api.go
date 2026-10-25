package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/triggers"
)

// triggerRoute is the path of the external trigger route.
const triggerRoute = "/api/proactive/trigger"

const (
	// maxTriggerBody bounds the request body, read before anything is parsed: the
	// message (2,000 characters, up to 4 bytes each) and the data (4 KB) fit with
	// room for the field names and JSON escapes.
	maxTriggerBody = 24 << 10

	// triggerRatePerMinute and triggerBurst are the per-process short limit of the
	// route, triggerPerHour its hourly one (burst and refill).
	triggerRatePerMinute = 30
	triggerBurst         = 10
	triggerPerHour       = 300
)

// triggerLimiter is two token buckets that a request must pass together: a short
// one (a burst, refilled every couple of seconds) that keeps a loop from flooding
// the pipeline, and an hourly one that bounds what a looping script can store (each
// accepted request leaves up to 8 KB of evidence in the database for weeks). One
// mutex covers both, and a refusal takes from neither. It is per process (it lives
// in sharedResources, so a config reload does not refill it) and takes its time from
// an injected clock.
type triggerLimiter struct {
	mu   sync.Mutex
	now  func() time.Time
	last time.Time
	b    [2]bucket
}

type bucket struct {
	perSec float64
	burst  float64
	tokens float64
}

func newTriggerLimiter(perMinute, burst int, now func() time.Time) *triggerLimiter {
	return newTriggerLimiterWithHourly(perMinute, burst, triggerPerHour, now)
}

func newTriggerLimiterWithHourly(perMinute, burst, perHour int, now func() time.Time) *triggerLimiter {
	if now == nil {
		now = time.Now
	}
	return &triggerLimiter{now: now, b: [2]bucket{
		{perSec: float64(perMinute) / 60, burst: float64(burst), tokens: float64(burst)},
		{perSec: float64(perHour) / 3600, burst: float64(perHour), tokens: float64(perHour)},
	}}
}

// allow takes one token from each bucket, or from neither. When it cannot, it
// returns how long until it could: the larger of the two waits.
func (l *triggerLimiter) allow() (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if !l.last.IsZero() {
		if el := now.Sub(l.last).Seconds(); el > 0 {
			for i := range l.b {
				l.b[i].tokens = math.Min(l.b[i].burst, l.b[i].tokens+el*l.b[i].perSec)
			}
		}
	}
	l.last = now
	var wait time.Duration
	for _, b := range l.b {
		if b.tokens < 1 {
			wait = max(wait, time.Duration((1-b.tokens)/b.perSec*float64(time.Second)))
		}
	}
	if wait > 0 {
		return false, wait
	}
	for i := range l.b {
		l.b[i].tokens--
	}
	return true, 0
}

// triggerLimiter returns the process-wide limiter, made on first use.
func (r *sharedResources) triggerLimiter() *triggerLimiter {
	if r == nil {
		return newTriggerLimiter(triggerRatePerMinute, triggerBurst, nil)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.triggerLim == nil {
		r.triggerLim = newTriggerLimiter(triggerRatePerMinute, triggerBurst, nil)
	}
	return r.triggerLim
}

// triggerRequest is the body of POST /api/proactive/trigger.
type triggerRequest struct {
	Name     string          `json:"name"`
	Message  string          `json:"message"`
	Severity string          `json:"severity"`
	Data     json.RawMessage `json:"data"`
}

// triggerResponse is the body of a 202.
type triggerResponse struct {
	EventID string `json:"event_id"`
	Status  string `json:"status"`
	// Severity is what the request will be treated as: the one asked for, capped.
	Severity string `json:"severity"`
	// Mode is the triggers mode in force: "live", or "shadow" (recorded, never
	// delivered).
	Mode string `json:"mode"`
}

// triggerAPI serves the external trigger route.
type triggerAPI struct {
	// runtime returns the running perception pipeline, nil when there is none.
	runtime func() *perceptionRuntime
	// mode returns the effective triggers mode.
	mode    func() string
	limiter *triggerLimiter
	now     func() time.Time
}

// registerTriggerAPI registers POST /api/proactive/trigger, the door through which
// a script or a hook on this machine asks the assistant to notice something. It is
// guarded like every other management route (loopback only, the management bearer
// token, no browser-originated request; see guardedMux), and it is registered only
// when perception is effectively on. The route is registered by registerGatewayRoutes
// and so comes back after a config reload.
//
// A route that holds no token is not registered at all: bearerTokenMiddleware lets
// everything through when its token is empty, and this route takes text from
// outside.
func registerTriggerAPI(mux handleMux, token string, a *triggerAPI) {
	if token == "" {
		logger.WarnCF("gateway", "The external trigger route is not registered: there is no management token", nil)
		return
	}
	mux = &guardedMux{inner: mux, token: token}
	mux.Handle("POST "+triggerRoute, loopbackOnly(http.HandlerFunc(a.handle)))
}

func writeTriggerJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func triggerError(w http.ResponseWriter, code int, msg string) {
	writeTriggerJSON(w, code, map[string]string{"error": msg})
}

func (a *triggerAPI) handle(w http.ResponseWriter, r *http.Request) {
	rt := a.runtime()
	switch {
	case rt == nil:
		triggerError(w, http.StatusConflict, "the perception pipeline is not running; nothing can be triggered")
		return
	case rt.triggers == nil:
		triggerError(w, http.StatusConflict,
			"triggers are off (proactive.triggers.mode); nothing would act on a trigger")
		return
	}

	if ok, wait := a.limiter.allow(); !ok {
		secs := int(math.Ceil(wait.Seconds()))
		w.Header().Set("Retry-After", strconv.Itoa(max(secs, 1)))
		triggerError(w, http.StatusTooManyRequests, "too many trigger requests; slow down")
		return
	}

	req, status, msg := decodeTriggerRequest(w, r)
	if status != 0 {
		logger.DebugCF("gateway", "External trigger refused", map[string]any{"status": status})
		triggerError(w, status, msg)
		return
	}

	sev := triggers.ExternalSeverity(req.Severity)
	id := proactive.NewID()
	now := a.now()
	ev := events.Event{
		ID:       id,
		Kind:     events.KindPerceptionExternalTrigger,
		Time:     now,
		Source:   events.Source{Component: "perception", Name: "external"},
		Severity: events.SeverityInfo,
		Payload: perception.ExternalTrigger{
			Name: req.Name, Message: req.Message, Severity: req.Severity, Data: req.Data, At: now,
		},
		Trust: events.TrustPtr(proactive.TrustUntrusted),
	}
	// Never blocks the handler: a full inbox is a 503 the caller can retry.
	if !rt.engine.Emitter().EmitNonBlocking(ev) {
		w.Header().Set("Retry-After", "1")
		triggerError(w, http.StatusServiceUnavailable, "the pipeline is busy; try again shortly")
		return
	}
	logger.DebugCF("gateway", "External trigger accepted", map[string]any{
		"name": req.Name, "message_bytes": len(req.Message), "data_bytes": len(req.Data),
	})
	writeTriggerJSON(w, http.StatusAccepted, triggerResponse{
		EventID: id, Status: "accepted", Severity: sev.String(), Mode: a.mode(),
	})
}

// decodeTriggerRequest reads and validates the body. It returns a non-zero status
// and a fixed message when the request is refused; the message never repeats what
// the request said.
func decodeTriggerRequest(w http.ResponseWriter, r *http.Request) (req triggerRequest, status int, msg string) {
	body := http.MaxBytesReader(w, r.Body, maxTriggerBody)
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return req, http.StatusRequestEntityTooLarge, "the request body is too large"
		}
		return req, http.StatusBadRequest, "the body must be one JSON object with the fields name, message, severity and data"
	}
	// Exactly one object: anything after it is refused.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return req, http.StatusRequestEntityTooLarge, "the request body is too large"
		}
		return req, http.StatusBadRequest, "the body must be one JSON object"
	}

	if !perception.ValidExternalName(req.Name) {
		return req, http.StatusBadRequest, "name must match ^[a-z0-9][a-z0-9_.-]{0,40}$"
	}
	if utf8.RuneCountInString(req.Message) > perception.MaxExternalMessageRunes {
		return req, http.StatusBadRequest, "message is over " + strconv.Itoa(perception.MaxExternalMessageRunes) + " characters"
	}
	switch problem := messageProblem(req.Message); problem {
	case problemControl:
		return req, http.StatusBadRequest, "message holds a control character"
	case problemBidi:
		return req, http.StatusBadRequest, "message holds a bidirectional control character"
	}
	switch strings.ToLower(strings.TrimSpace(req.Severity)) {
	case "", "info", "notice", "warning", "critical":
	default:
		return req, http.StatusBadRequest, "severity must be info, notice or warning"
	}
	data, ok := compactData(req.Data)
	if !ok {
		return req, http.StatusBadRequest, "data must be a JSON object of at most " +
			strconv.Itoa(perception.MaxExternalDataBytes) + " bytes nested at most " +
			strconv.Itoa(perception.MaxExternalDataDepth) + " levels"
	}
	req.Data = data
	return req, 0, ""
}

// What messageProblem finds wrong with a message.
const (
	problemNone    = ""
	problemControl = "control"
	problemBidi    = "bidi"
)

// messageProblem reports what keeps s from being a message: invalid text, a control
// character other than the line break, carriage return and tab, or a bidirectional
// control (they reorder what the owner reads: the joiners U+200C and U+200D are
// allowed).
func messageProblem(s string) string {
	if !utf8.ValidString(s) {
		return problemControl
	}
	for _, r := range s {
		switch {
		case perception.IsBidiControl(r):
			return problemBidi
		case unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t':
			return problemControl
		}
	}
	return problemNone
}

// compactData validates the data and returns it compacted. Absent and null data are
// no data. Anything else must be a JSON object within the size and depth bounds.
func compactData(raw json.RawMessage) (json.RawMessage, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil, true
	}
	if raw[0] != '{' || len(raw) > 2*perception.MaxExternalDataBytes {
		return nil, false
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil || buf.Len() > perception.MaxExternalDataBytes {
		return nil, false
	}
	if !withinDepth(buf.Bytes(), perception.MaxExternalDataDepth) {
		return nil, false
	}
	return buf.Bytes(), true
}

// withinDepth reports whether the JSON nests at most max levels.
func withinDepth(raw []byte, max int) bool {
	depth := 0
	inString, escaped := false, false
	for _, c := range raw {
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{', '[':
			depth++
			if depth > max {
				return false
			}
		case '}', ']':
			depth--
		}
	}
	return true
}
