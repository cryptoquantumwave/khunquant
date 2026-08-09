package sandbox

import (
	"fmt"
	"net/url"
	"strings"
)

const (
	sandboxPathPrefix = "/__sbx__/"
)

// BuildURL rewrites an original URL to a sandbox address. It preserves query
// strings, method, headers, and body. The returned URL follows the contract:
// http://127.0.0.1:<port>/__sbx__/<venue>/<original-host>/<original-path>
//
// The original path does NOT include the query string; it is preserved in the
// returned URL's query component (RawQuery).
//
// Returns an error if baseURL is empty or malformed.
func BuildURL(venue string, originalURL *url.URL, baseURL string) (*url.URL, error) {
	if baseURL == "" {
		return nil, fmt.Errorf("sandbox: baseURL is empty")
	}

	// Extract host and path from original URL.
	host := originalURL.Host
	path := originalURL.EscapedPath()

	// Build the sandbox path.
	sandboxPath := fmt.Sprintf("%s%s/%s%s", sandboxPathPrefix, venue, host, path)

	// Parse the base URL.
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("sandbox: invalid baseURL %q: %w", baseURL, err)
	}

	// Set the path and query.
	u.Path = sandboxPath
	u.RawQuery = originalURL.RawQuery

	return u, nil
}

// ParsedRequest contains the components of a parsed sandbox request.
type ParsedRequest struct {
	Venue string
	Host  string
	Path  string
}

// ParseRequest extracts (venue, host, path) from an inbound sandbox request.
// The request path must start with /__sbx__/ and be in the form:
// /__sbx__/<venue>/<host>/<path>
//
// The path component is the original API path (e.g., /api/v1/account). It does
// NOT include the query string; query strings are preserved in r.URL.RawQuery.
// Path components containing encoded slashes (e.g., %2F) are preserved correctly.
func ParseRequest(escapedPath string) (*ParsedRequest, error) {
	if !strings.HasPrefix(escapedPath, sandboxPathPrefix) {
		return nil, fmt.Errorf("request path must start with %s", sandboxPathPrefix)
	}

	// Remove the prefix and leading slash.
	rest := strings.TrimPrefix(escapedPath, sandboxPathPrefix)

	// Split into exactly 3 components: venue, host, remainder.
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) < 2 {
		return nil, fmt.Errorf("invalid sandbox path format: need at least /__sbx__/<venue>/<host>, got %s", escapedPath)
	}

	venue := parts[0]
	host := parts[1]
	path := "/"
	if len(parts) == 3 {
		path = "/" + parts[2]
	}

	return &ParsedRequest{
		Venue: venue,
		Host:  host,
		Path:  path,
	}, nil
}
