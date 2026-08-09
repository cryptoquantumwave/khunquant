package sandbox

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/cryptoquantumwave/khunquant/pkg/logger"
)

// errorResponse is the JSON body of a sandbox error response.
type errorResponse struct {
	Error string `json:"error"`
}

// BuildRouter creates an http.HandlerFunc that routes sandbox requests. It accepts
// an optional list of responders that can intercept requests before the fixture
// store is consulted. The responder chain is queried in order; the first responder
// that returns true short-circuits the fixture store.
//
// If no responder answers and no fixture is found, the handler returns a non-2xx
// error with a descriptive message naming the venue, method, and path.
func BuildRouter(store *Store, responders ...Responder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Parse the sandbox address.
		parsed, err := ParseRequest(r.URL.EscapedPath())
		if err != nil {
			http.Error(w, fmt.Sprintf("sandbox: invalid request path: %v", err), http.StatusBadRequest)
			return
		}

		venue := parsed.Venue
		method := r.Method
		path := parsed.Path

		// Try responders in order.
		for _, responder := range responders {
			if resp, ok := responder.Respond(venue, method, path, r); ok {
				// Validate status code before calling WriteHeader (which panics for invalid codes).
				if err := ValidateStatus(resp.Status); err != nil {
					logger.WarnF(fmt.Sprintf("responder returned invalid status for %s %s (venue=%s): %v", method, path, venue, err),
						map[string]any{"venue": venue, "method": method, "path": path, "status": resp.Status})
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusInternalServerError)
					errBody, _ := json.Marshal(errorResponse{
						Error: fmt.Sprintf("responder returned invalid status %d for %s %s", resp.Status, method, path),
					})
					_, _ = w.Write(errBody)
					return
				}
				for k, v := range resp.Headers {
					w.Header().Set(k, v)
				}
				w.WriteHeader(resp.Status)
				writeResponseBody(w, resp)
				return
			}
		}

		// Try the fixture store.
		query := r.URL.Query()
		fixture := store.FindFixture(venue, method, path, query)
		if fixture != nil {
			// Validate status code before calling WriteHeader (which panics for invalid codes).
			if err := ValidateStatus(fixture.Response.Status); err != nil {
				logger.WarnF(fmt.Sprintf("fixture has invalid status for %s %s (venue=%s): %v", method, path, venue, err),
					map[string]any{"venue": venue, "method": method, "path": path, "status": fixture.Response.Status})
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				errBody, _ := json.Marshal(errorResponse{
					Error: fmt.Sprintf("fixture has invalid status %d for %s %s", fixture.Response.Status, method, path),
				})
				_, _ = w.Write(errBody)
				return
			}
			for k, v := range fixture.Response.Headers {
				w.Header().Set(k, v)
			}
			w.WriteHeader(fixture.Response.Status)
			writeResponseBody(w, &fixture.Response)
			return
		}

		// No responder, no fixture. Determine if this is a path match miss or a query constraint miss.
		var errMsg string
		var logFields map[string]any
		if store.HasPathMatch(venue, method, path) {
			// Path matches but query constraints don't. Report specifically.
			queryStr := query.Encode()
			errMsg = fmt.Sprintf("sandbox: no fixture configured for %s %s (venue=%s): fixtures exist for this path but none matched the query (%s); add a query-scoped fixture", method, path, venue, queryStr)
			logFields = map[string]any{"venue": venue, "method": method, "path": path, "query": queryStr}
		} else {
			// Path doesn't match at all.
			errMsg = fmt.Sprintf("sandbox: no fixture configured for %s %s (venue=%s)", method, path, venue)
			logFields = map[string]any{"venue": venue, "method": method, "path": path}
		}

		logger.WarnF(errMsg, logFields)
		body, _ := json.Marshal(errorResponse{Error: errMsg})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write(body)
	}
}

// RouteFromRequest extracts (venue, method, path) from an HTTP request, assuming
// the request is to a sandbox URL. Used in tests and utilities.
func RouteFromRequest(r *http.Request) (*ParsedRequest, error) {
	return ParseRequest(r.URL.EscapedPath())
}

// writeResponseBody writes the response body to the http.ResponseWriter.
// It handles both inline JSON (Body) and plain text (BodyText).
// If Body is not empty, it's written as-is (byte-preserving JSON).
// Otherwise, if BodyText is not empty, that's written as plain text.
// If Content-Type is not already set and Body is used, it's set to application/json.
func writeResponseBody(w http.ResponseWriter, resp *Response) {
	if len(resp.Body) > 0 {
		// Write inline JSON body as-is (byte-preserving).
		// Set Content-Type to application/json if not already set.
		if w.Header().Get("Content-Type") == "" {
			w.Header().Set("Content-Type", "application/json")
		}
		_, _ = w.Write(resp.Body)
	} else if resp.BodyText != "" {
		// Fallback to plain-text body.
		_, _ = w.Write([]byte(resp.BodyText))
	}
}
