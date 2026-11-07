// Package simweb is the simulated web of a locked sandbox: the pages web_fetch reads and
// the results web_search lists, served by the sandbox server as venue "web". Nothing here
// touches the network; an address no rule matches is a 404 from the sandbox.
package simweb

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/fileutil"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labjournal"
)

// Limits: rules come from an agent, so their size and count are bounded.
const (
	maxRules   = 500
	maxBody    = 1 << 20
	maxPattern = 512
	// KindReq is the journal kind of one request.
	KindReq = "web.req"
)

// PageRule answers a page: every request whose host+path matches URLPattern.
type PageRule struct {
	ID          string            `json:"id"`
	URLPattern  string            `json:"url_pattern"`  // host+path glob; * crosses "/": "attacker.example/*", "*.github.io/news/*"
	Status      int               `json:"status"`       // default 200
	ContentType string            `json:"content_type"` // default "text/html; charset=utf-8"
	Body        string            `json:"body"`
	Headers     map[string]string `json:"headers,omitempty"` // e.g. Location for a redirect
}

// Result is one hit of a search rule, in Brave's field names.
type Result struct {
	Title       string `json:"title"`
	URL         string `json:"url"`
	Description string `json:"description"`
}

// SearchRule answers a Brave search whose q matches Query.
type SearchRule struct {
	ID      string   `json:"id"`
	Query   string   `json:"query"` // case-insensitive glob over the query text
	Results []Result `json:"results"`
}

// Journal is where web.req entries go (labjournal.Journal).
type Journal interface{ Append(e labjournal.Entry) }

// Options configures Open.
type Options struct {
	Path    string // <home>/lab/web.json; empty keeps the rules in memory only
	Journal Journal
	Now     func() time.Time
}

// Store holds the rules and the request log. The rules outlive a gateway generation and, via
// Path, a restart.
type Store struct {
	mu       sync.Mutex
	path     string
	journal  Journal
	now      func() time.Time
	pages    []PageRule // oldest first: the newest match wins
	searches []SearchRule
	next     int
	ring     []Request
	seq      int64
}

type file struct {
	Pages    []PageRule   `json:"pages"`
	Searches []SearchRule `json:"searches"`
	Next     int          `json:"next"`
}

// Open loads the rules from o.Path (none when the file does not exist). A file that does
// not parse is an error: silently starting empty would hide the rules a scenario set.
func Open(o Options) (*Store, error) {
	s := &Store{path: o.Path, journal: o.Journal, now: o.Now}
	if s.now == nil {
		s.now = time.Now
	}
	if o.Path == "" {
		return s, nil
	}
	b, err := os.ReadFile(o.Path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("simweb: %w", err)
	}
	var f file
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("simweb: %s: %w", o.Path, err)
	}
	s.pages, s.searches, s.next = f.Pages, f.Searches, f.Next
	return s, nil
}

// PutPage adds a page rule, or replaces the rule with the same ID (which makes it the newest),
// and returns its ID.
func (s *Store) PutPage(r PageRule) (string, error) {
	r.URLPattern = strings.TrimSpace(r.URLPattern)
	switch {
	case r.URLPattern == "" || len(r.URLPattern) > maxPattern:
		return "", fmt.Errorf("url_pattern must be 1..%d characters", maxPattern)
	case len(r.Body) > maxBody:
		return "", fmt.Errorf("body is larger than %d bytes", maxBody)
	}
	if r.Status == 0 {
		r.Status = 200
	}
	if err := sandbox.ValidateStatus(r.Status); err != nil {
		return "", err
	}
	if r.ContentType == "" {
		r.ContentType = "text/html; charset=utf-8"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id, err := s.slot(r.ID, "page", len(s.pages), slices.ContainsFunc(s.pages, func(p PageRule) bool { return p.ID == r.ID }))
	if err != nil {
		return "", err
	}
	r.ID = id
	s.pages = append(slices.DeleteFunc(slices.Clone(s.pages), func(p PageRule) bool { return p.ID == id }), r)
	return id, s.saveLocked()
}

// PutSearch adds a search rule the same way.
func (s *Store) PutSearch(r SearchRule) (string, error) {
	r.Query = strings.TrimSpace(r.Query)
	if r.Query == "" || len(r.Query) > maxPattern {
		return "", fmt.Errorf("search must be 1..%d characters", maxPattern)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id, err := s.slot(r.ID, "search", len(s.searches), slices.ContainsFunc(s.searches, func(p SearchRule) bool { return p.ID == r.ID }))
	if err != nil {
		return "", err
	}
	r.ID = id
	s.searches = append(slices.DeleteFunc(slices.Clone(s.searches), func(p SearchRule) bool { return p.ID == id }), r)
	return id, s.saveLocked()
}

// slot picks the ID of a rule and enforces the count limit; the caller holds mu.
func (s *Store) slot(id, kind string, n int, exists bool) (string, error) {
	if exists {
		return id, nil
	}
	if n >= maxRules {
		return "", fmt.Errorf("at most %d %s rules", maxRules, kind)
	}
	if id == "" {
		s.next++
		id = fmt.Sprintf("%s-%d", kind, s.next)
	}
	return id, nil
}

// List returns copies of the rules, oldest first.
func (s *Store) List() (pages []PageRule, searches []SearchRule) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.pages), slices.Clone(s.searches)
}

// saveLocked writes the rules atomically. The caller holds mu.
func (s *Store) saveLocked() error {
	if s.path == "" {
		return nil
	}
	b, err := json.Marshal(file{Pages: s.pages, Searches: s.searches, Next: s.next})
	if err != nil {
		return err
	}
	return fileutil.WriteFileAtomic(s.path, b, 0o600)
}

// glob compiles a case-insensitive pattern in which * matches any run of characters
// (slashes too) and ? matches one.
func glob(p string) *regexp.Regexp {
	q := regexp.QuoteMeta(strings.ToLower(p))
	q = strings.ReplaceAll(strings.ReplaceAll(q, `\*`, `.*`), `\?`, `.`)
	return regexp.MustCompile(`(?s)^` + q + `$`)
}
