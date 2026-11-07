package simweb

import (
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

// Venue is the sandbox address venue of every web request (sandbox.NewRoundTripper("web", ...)).
const Venue = "web"

// braveHost and bravePath are the one search endpoint SimWeb renders. Any other search
// provider's host is an ordinary address: 404 unless a page rule was planted for it.
const (
	braveHost = "api.search.brave.com"
	bravePath = "/res/v1/web/search"
)

// Middleware is the StageWeb stage. It answers every request for venue "web" itself, a 404
// of the sandbox's own when no rule matches, and never calls next for one; any other venue
// goes on to next untouched.
func (s *Store) Middleware() sandbox.Middleware {
	return func(next sandbox.Responder) sandbox.Responder {
		return sandbox.ResponderFunc(func(venue, method, path string, r *http.Request) (*sandbox.Response, bool) {
			if venue != Venue || r == nil || r.URL == nil {
				return next.Respond(venue, method, path, r)
			}
			return s.answer(method, path, r), true
		})
	}
}

func (s *Store) answer(method, path string, r *http.Request) *sandbox.Response {
	host := ""
	if p, err := sandbox.ParseRequest(r.URL.EscapedPath()); err == nil {
		host = strings.ToLower(p.Host)
	}
	bare := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		bare = h
	}
	plain, err := url.PathUnescape(path)
	if err != nil {
		plain = path
	}
	query := r.URL.RawQuery

	resp, rule := s.lookup(bare, plain, r.URL.Query().Get("q"))
	s.record(method, host, plain, query, resp.Status, rule)
	return resp
}

// lookup finds the answer: the Brave search endpoint is rendered from the search rules, any
// other address from the newest page rule matching its host+path (the query is ignored).
func (s *Store) lookup(host, path, q string) (*sandbox.Response, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if host == braveHost && path == bravePath {
		results, id := []Result{}, ""
		for i := len(s.searches) - 1; i >= 0; i-- {
			if glob(s.searches[i].Query).MatchString(strings.ToLower(q)) {
				results, id = s.searches[i].Results, s.searches[i].ID
				break
			}
		}
		// Brave's own shape: {"web":{"results":[...]}}. No rule is the empty list.
		body, _ := json.Marshal(map[string]any{"web": map[string]any{"results": results}})
		return &sandbox.Response{Status: http.StatusOK, Body: body, Headers: map[string]string{"Content-Type": "application/json"}}, id
	}
	target := host + path
	for i := len(s.pages) - 1; i >= 0; i-- {
		p := s.pages[i]
		if g := glob(p.URLPattern); g.MatchString(target) {
			h := map[string]string{}
			for k, v := range p.Headers {
				h[k] = v
			}
			h["Content-Type"] = p.ContentType
			return &sandbox.Response{Status: p.Status, BodyText: p.Body, Headers: h}, p.ID
		}
	}
	return &sandbox.Response{
		Status:   http.StatusNotFound,
		BodyText: "sandbox: no web fixture for " + target,
		Headers:  map[string]string{"Content-Type": "text/plain; charset=utf-8"},
	}, ""
}
