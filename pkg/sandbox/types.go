package sandbox

import (
	"encoding/json"
	"net/http"
)

// Response represents a mocked HTTP response.
// Body is inline JSON and is preserved byte-for-byte on the wire (no re-marshaling).
// BodyText is used only if Body is empty (for non-JSON responses).
type Response struct {
	Status   int               `json:"status"`
	Body     json.RawMessage   `json:"body,omitempty"`
	BodyText string            `json:"body_text,omitempty"`
	Headers  map[string]string `json:"headers,omitempty"`
}

// FixtureEntry represents a single fixture (request → response) in the store.
type FixtureEntry struct {
	// Method is the HTTP method to match (e.g., "GET", "POST"). Case-insensitive.
	Method string `json:"method"`

	// PathPrefix is the path prefix to match. Matching is prefix-based, so all paths
	// starting with this prefix will match. No regex; simple string prefix.
	PathPrefix string `json:"path_prefix"`

	// Query is an optional set of query parameter constraints. If empty, the fixture
	// matches any query parameters. If non-empty, all listed keys must be present
	// in the request with exact matching values. Unlisted query parameters are ignored.
	Query map[string]string `json:"query,omitempty"`

	// Response is the mocked HTTP response.
	Response Response `json:"response"`
}

// Responder can answer a request before the static fixture store is consulted.
// Implementations must be concurrency-safe (called from the HTTP handler).
// This interface is the seam for stateful simulation (T5).
type Responder interface {
	// Respond returns (resp, true) to take precedence over any static fixture,
	// or (nil, false) to fall through to the fixture store. The full *http.Request
	// is provided so the responder can read the body for mutations (e.g., place_order).
	Respond(venue, method, path string, r *http.Request) (*Response, bool)
}

// ResponderFunc adapts a function to the Responder interface.
type ResponderFunc func(venue, method, path string, r *http.Request) (*Response, bool)

func (f ResponderFunc) Respond(venue, method, path string, r *http.Request) (*Response, bool) {
	return f(venue, method, path, r)
}
