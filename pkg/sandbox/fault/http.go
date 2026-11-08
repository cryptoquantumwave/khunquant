package fault

import (
	"context"
	"net/http"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

// A request is decided once, by the handler wrap: it picks the first active HTTP-level rule
// (a venue-shaped answer or a hold), so one request fires at most one rule, in insertion
// order, and the probability draw is spent once. The wrap leaves the decision in the
// request's context for Middleware to apply when the rule is a venue-shaped answer.
type decisionKey struct{}

// decision is the rule a response-layer rule picked, or nil.
type decision struct{ resp *fired }

func isHTTP(l layer) bool     { return l == layerResponse || l == layerWrap }
func isResponse(l layer) bool { return l == layerResponse }

// HandlerWrap holds, delays, drops or half-completes a request (latency, timeout,
// drop_connection, written_then_timeout). Pass it to (*sandbox.Server).SetHandlerWrap.
func (i *Injector) HandlerWrap() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Every answer closes its connection. Go's client silently resends an idempotent
			// request (a GET) that fails on a reused connection, so a timeout or a dropped
			// connection on a kept-alive one would fire, journal and then never reach the
			// caller. On loopback a fresh connection per request costs nothing.
			w.Header().Set("Connection", "close")
			p, err := sandbox.ParseRequest(r.URL.EscapedPath())
			if err != nil {
				next.ServeHTTP(w, r)
				return
			}
			f := i.pick(isHTTP, i.newRequest(p.Venue, r.Method, p.Path, r))
			d := &decision{}
			if f != nil && f.ly == layerResponse {
				d.resp = f
			}
			r = r.WithContext(context.WithValue(r.Context(), decisionKey{}, d))
			if f == nil || f.ly == layerResponse {
				next.ServeHTTP(w, r)
				return
			}
			ctx := r.Context()
			switch f.kind {
			case "latency":
				if i.Hold(ctx, time.Duration(f.rule.Effect.Latency)) == nil {
					next.ServeHTTP(w, r)
				}
			case "written_then_timeout":
				next.ServeHTTP(discard{}, r) // the simulator creates the order; the answer is lost
				_ = i.Hold(ctx, i.maxHold)
				closeConn(w)
			case "timeout":
				_ = i.Hold(ctx, i.maxHold)
				closeConn(w)
			default: // drop_connection
				closeConn(w)
			}
		})
	}
}

// closeConn ends the connection with no answer. A writer that cannot hijack (HTTP/2, a
// test recorder) gets a bodiless 504 instead.
func closeConn(w http.ResponseWriter) {
	if hj, ok := w.(http.Hijacker); ok {
		if c, _, err := hj.Hijack(); err == nil {
			_ = c.Close()
			return
		}
	}
	w.WriteHeader(http.StatusGatewayTimeout)
}

// discard is a ResponseWriter that throws the answer away.
type discard struct{}

func (discard) Header() http.Header         { return http.Header{} }
func (discard) Write(b []byte) (int, error) { return len(b), nil }
func (discard) WriteHeader(int)             {}

// Middleware answers a request a response-layer rule (status, body, error, reject) picked.
// Install it as sandbox.StageFault. Behind the handler wrap it applies the wrap's decision;
// with no wrap in front (a chain-only rig) it picks for itself, among those rules only.
func (i *Injector) Middleware() sandbox.Middleware {
	return func(next sandbox.Responder) sandbox.Responder {
		return sandbox.ResponderFunc(func(venue, method, path string, r *http.Request) (*sandbox.Response, bool) {
			var f *fired
			if d, ok := decisionOf(r); ok {
				f = d.resp
			} else {
				f = i.pick(isResponse, i.newRequest(venue, method, path, r))
			}
			if f != nil {
				if resp, ok := i.response(f, venue); ok {
					return resp, true
				}
			}
			return next.Respond(venue, method, path, r)
		})
	}
}

func decisionOf(r *http.Request) (*decision, bool) {
	if r == nil {
		return nil, false
	}
	d, ok := r.Context().Value(decisionKey{}).(*decision)
	return d, ok
}

// response is the answer a picked response-layer rule gives.
func (i *Injector) response(f *fired, venue string) (*sandbox.Response, bool) {
	e := f.rule.Effect
	switch f.kind {
	case "error":
		return libraryResponse(venue, e.Error)
	case "reject":
		return libraryResponse(venue, e.Code)
	}
	status := e.Status
	if status == 0 {
		status = http.StatusOK
	}
	return &sandbox.Response{Status: status, Body: e.Body, Headers: map[string]string{"Content-Type": "application/json"}}, true
}
