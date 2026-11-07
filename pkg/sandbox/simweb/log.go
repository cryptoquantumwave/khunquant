package simweb

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labjournal"
)

// ringSize is how many requests Requests can return. The journal's web.req entries are
// unbounded: an assertion that must not miss one reads those.
const ringSize = 1000

// Request is one request SimWeb answered. URL is host+path[?query]: the sandbox address
// carries no scheme, so none is shown.
type Request struct {
	Seq     int64     `json:"seq"`
	SimTime time.Time `json:"sim_time"`
	Method  string    `json:"method"`
	URL     string    `json:"url"`
	Host    string    `json:"host"`
	Path    string    `json:"path"`
	Query   string    `json:"query,omitempty"`
	Status  int       `json:"status"`
	RuleID  string    `json:"rule_id,omitempty"`
}

// record keeps the request in the ring and journals it as web.req.
func (s *Store) record(method, host, path, query string, status int, rule string) {
	now := s.now()
	u := host + path
	if query != "" {
		u += "?" + query
	}
	s.mu.Lock()
	s.seq++
	r := Request{Seq: s.seq, SimTime: now, Method: method, URL: u, Host: host, Path: path, Query: query, Status: status, RuleID: rule}
	if len(s.ring) == ringSize {
		s.ring = s.ring[1:]
	}
	s.ring = append(s.ring, r)
	s.mu.Unlock()
	if s.journal == nil {
		return
	}
	payload, _ := json.Marshal(map[string]any{
		"method": method, "url": u, "host": host, "path": path, "query": query, "status": status, "rule_id": rule,
	})
	s.journal.Append(labjournal.Entry{Kind: KindReq, Source: "sandbox", Subject: host, SimTime: now, Payload: payload})
}

// Requests returns the logged requests after sequence number since (0: all of the ring),
// oldest first, optionally only those to host (compared without a port, case-insensitively).
func (s *Store) Requests(since int64, host string) []Request {
	host = strings.ToLower(host)
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Request{}
	for _, r := range s.ring {
		if r.Seq > since && (host == "" || r.Host == host || strings.HasPrefix(r.Host, host+":")) {
			out = append(out, r)
		}
	}
	return out
}
