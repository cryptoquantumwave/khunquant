package tools

import (
	"net/http"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/egress"
	"github.com/cryptoquantumwave/khunquant/pkg/utils"
)

// labWebVenue is the sandbox address venue of every web request; SimWeb (pkg/sandbox/simweb)
// answers it.
const labWebVenue = "web"

// labWebClient routes a web tool's HTTP client to the sandbox when the process is locked, and
// returns c itself, unchanged, when it is not. Locked, every request is rewritten to
// http://127.0.0.1:<port>/__sbx__/web/<host>/<path> before it is dialled (and a rewrite that
// is not loopback is an error), so the transport's dialer sees only the sandbox server:
// it is the loopback-only egress dialer rather than the SSRF-safe one (which refuses
// loopback, and so the sandbox), it is a clone so the process's default transport is left
// alone, and it has no proxy. c is changed in place so a CheckRedirect closure over it keeps
// working; a nil c gets a client of its own.
func labWebClient(c *http.Client) *http.Client {
	if !sandbox.Locked() {
		return c
	}
	if c == nil {
		c = &http.Client{Timeout: searchTimeout}
	}
	if _, done := c.Transport.(*sandbox.RoundTripper); done {
		return c
	}
	t := &http.Transport{TLSHandshakeTimeout: 15 * time.Second}
	if base, ok := c.Transport.(*http.Transport); ok && base != nil {
		t = base.Clone()
	}
	t.Proxy, t.DialContext = nil, egress.DialContext(egress.Policy{})
	t.Dial, t.DialTLS, t.DialTLSContext = nil, nil, nil //nolint:staticcheck // deprecated hooks must not bypass the guard
	c.Transport = sandbox.NewRoundTripper(labWebVenue, t)
	return c
}

// searchClient is the HTTP client of a search provider.
func searchClient(proxy string, timeout time.Duration) (*http.Client, error) {
	c, err := utils.CreateHTTPClient(proxy, timeout)
	if err != nil {
		return nil, err
	}
	return labWebClient(c), nil
}
