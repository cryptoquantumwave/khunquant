// Package webops is the lab group "web": plant the pages web_fetch reads and the results
// web_search lists, and read back which addresses were requested. It is a package of its own
// because lab holds a *simweb.Store, so simweb must not import lab.
package webops

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/simweb"
)

const maxArgs = 2 << 20 // a page body is capped at 1 MiB; anything bigger is refused unread

// Group is the simulated web: put pages and searches, list them, read the request log.
func Group(s *simweb.Store) lab.Group {
	return lab.Group{
		Name: "web",
		Description: "The simulated web. web_fetch and web_search (and so the news source) read only these: an address no page " +
			"matches is a 404 of the sandbox's own, and nothing reaches the internet. Every request is logged and journaled as web.req " +
			"(assert on the journal: the ring behind `requests` holds the last 1000). Only Brave's search wire format is rendered.",
		Ops: []lab.Op{
			{
				Name: "put",
				Description: "add a page (url_pattern + body) or a search (search + results). The newest matching rule wins; an id that exists is " +
					"replaced and becomes the newest. Answers the id.",
				Args: json.RawMessage(`{"id":{"type":"string"},` +
					`"url_pattern":{"type":"string","description":"host+path glob, * crosses /: attacker.example/*, *.github.io/news/*"},` +
					`"status":{"type":"integer","description":"default 200"},"content_type":{"type":"string","description":"default text/html; charset=utf-8"},` +
					`"body":{"type":"string"},"headers":{"type":"object","description":"e.g. Location for a redirect"},` +
					`"search":{"type":"string","description":"case-insensitive glob over the search query, instead of url_pattern"},` +
					`"results":{"type":"array","items":{"type":"object","description":"{title, url, description}"}}}`),
				Run: put(s),
			},
			{Name: "list", Description: "every page and search rule, oldest first", Run: list(s)},
			{
				Name:        "requests",
				Description: "the last 1000 requests: seq, sim_time, method, url (host+path?query, no scheme), host, status, rule_id",
				Args:        json.RawMessage(`{"since_seq":{"type":"integer","description":"only after this seq"},"host":{"type":"string"}}`),
				Run:         requests(s),
			},
		},
	}
}

// decode reads the call's object strictly: a misspelt key must not turn into a rule that
// matches something else.
func decode(args json.RawMessage, into any) error {
	if len(args) > maxArgs {
		return lab.Errorf(lab.CodeBadArgs, "the call is larger than %d bytes", maxArgs)
	}
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return lab.Errorf(lab.CodeBadArgs, "%v", err)
	}
	return nil
}

func put(s *simweb.Store) lab.OpFunc {
	return func(_ context.Context, args json.RawMessage) (any, error) {
		var a struct {
			Op      string          `json:"op"`
			Search  string          `json:"search"`
			Results []simweb.Result `json:"results"`
			simweb.PageRule
		}
		if err := decode(args, &a); err != nil {
			return nil, err
		}
		var id string
		var err error
		switch {
		case a.Search != "" && (a.URLPattern != "" || a.Body != "" || a.Status != 0):
			return nil, lab.Errorf(lab.CodeBadArgs, "give a page (url_pattern, body) or a search (search, results), not both")
		case a.Search != "":
			id, err = s.PutSearch(simweb.SearchRule{ID: a.ID, Query: a.Search, Results: a.Results})
		default:
			if len(a.Results) > 0 {
				return nil, lab.Errorf(lab.CodeBadArgs, "results belong to a search")
			}
			id, err = s.PutPage(a.PageRule)
		}
		if err != nil {
			return nil, lab.Errorf(lab.CodeBadArgs, "%v", err)
		}
		return map[string]any{"id": id}, nil
	}
}

func list(s *simweb.Store) lab.OpFunc {
	return func(context.Context, json.RawMessage) (any, error) {
		pages, searches := s.List()
		return map[string]any{"pages": pages, "searches": searches}, nil
	}
}

func requests(s *simweb.Store) lab.OpFunc {
	return func(_ context.Context, args json.RawMessage) (any, error) {
		var a struct {
			Op       string `json:"op"`
			SinceSeq int64  `json:"since_seq"`
			Host     string `json:"host"`
		}
		if err := decode(args, &a); err != nil {
			return nil, err
		}
		return map[string]any{"requests": s.Requests(a.SinceSeq, a.Host)}, nil
	}
}
