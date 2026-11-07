// Package simtest is a fake model API for tests of sim record and live: an httptest server
// on loopback that speaks the Anthropic messages and the OpenAI chat shapes. Nothing in
// this repository's tests talks to a real model; this is what stands in for one.
package simtest

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Request is what the fake received.
type Request struct {
	Path   string
	Header http.Header
	Body   []byte
}

// Fake is the server. Its zero-value knobs give a plain "ok" answer with usage.
type Fake struct {
	srv *httptest.Server

	// Reply returns the text to answer with for a request body (default "fake reply").
	Reply func(body []byte) string
	// InTok and OutTok are the usage reported (default 100 and 20); OmitUsage reports none.
	InTok, OutTok int
	OmitUsage     bool
	// FailWith, when non-zero, answers every request with this status and an error body
	// that echoes the request's credentials back (as a careless API might).
	FailWith int

	mu   sync.Mutex
	reqs []Request
}

// New starts a fake and stops it when the test ends.
func New(t *testing.T) *Fake {
	t.Helper()
	f := &Fake{}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// Base is the API base to configure ("http://127.0.0.1:port/v1").
func (f *Fake) Base() string { return f.srv.URL + "/v1" }

// Requests returns what was received so far.
func (f *Fake) Requests() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Request(nil), f.reqs...)
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.reqs = append(f.reqs, Request{Path: r.URL.Path, Header: r.Header.Clone(), Body: body})
	f.mu.Unlock()

	if f.FailWith != 0 {
		w.WriteHeader(f.FailWith)
		_, _ = w.Write([]byte(`{"error":"denied for ` + r.Header.Get("X-Api-Key") + r.Header.Get("Authorization") + `"}`))
		return
	}
	text := "fake reply"
	if f.Reply != nil {
		text = f.Reply(body)
	}
	in, out := f.InTok, f.OutTok
	if in == 0 && out == 0 {
		in, out = 100, 20
	}
	var resp any
	switch {
	case strings.HasSuffix(r.URL.Path, "/messages"):
		m := map[string]any{"id": "msg_fake", "type": "message", "role": "assistant", "model": "fake",
			"content": []map[string]any{{"type": "text", "text": text}}, "stop_reason": "end_turn"}
		if !f.OmitUsage {
			m["usage"] = map[string]int{"input_tokens": in, "output_tokens": out}
		}
		resp = m
	case strings.HasSuffix(r.URL.Path, "/chat/completions"):
		m := map[string]any{"id": "chatcmpl-fake", "object": "chat.completion", "model": "fake",
			"choices": []map[string]any{{"index": 0, "finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": text}}}}
		if !f.OmitUsage {
			m["usage"] = map[string]int{"prompt_tokens": in, "completion_tokens": out, "total_tokens": in + out}
		}
		resp = m
	default:
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
