package sim

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/protocoltypes"
)

// CanonicalRequest is a model request with everything that varies from run to run
// replaced by a placeholder, so that two runs of one scenario hash alike. It is kept in a
// cassette entry, so a reviewer can see what a key matched. The model is not part of it:
// a cassette answers whatever model name the instance is configured with.
type CanonicalRequest struct {
	Messages  []CanonicalMessage `json:"messages"`
	Tools     []string           `json:"tools,omitempty"` // name:first 16 hex of the sha256 of the parameter schema
	MaxTokens int                `json:"max_tokens,omitempty"`
}

// CanonicalMessage is one message of a CanonicalRequest.
type CanonicalMessage struct {
	Role       string          `json:"role"`
	Content    string          `json:"content,omitempty"`
	ToolCalls  []CanonicalCall `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

// CanonicalCall is a tool call of an assistant message.
type CanonicalCall struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Args string `json:"args"`
}

// What varies, and the placeholder it becomes (see canonText). Seeding the nonces and
// canonicalising are both used: seeding alone cannot fix a ULID's clock part or the
// per-machine runtime line, and canonicalising alone leaves cards unstable.
var (
	// The prompt's clock line and runtime line (pkg/agent/context.go, buildDynamicContextFor).
	reTimeLine    = regexp.MustCompile(`(## Current Time\n)[^\n]*`)
	reRuntimeLine = regexp.MustCompile(`(## Runtime\n)[^\n]*`)
	// The candidate fence (newFenceNonce) and the news fence (newsNonce).
	reFence = regexp.MustCompile(`<<<((?:END )?(?:CANDIDATES |ARTICLE )?)[0-9a-f]{8,}>>>`)
	// A ULID (proactive.NewID): 26 Crockford digits, the first at most 7.
	reULID = regexp.MustCompile(`\b[0-7][0-9A-HJKMNP-TV-Z]{25}\b`)
	// An approval button payload "<code>.<nonce>" (approval.ButtonPayload).
	reButton = regexp.MustCompile(`\b[A-HJ-NP-Z2-9]{4}\.[0-9a-f]{16}\b`)
	// RFC 3339 and "2006-01-02 15:04[:05]" timestamps.
	reTS = regexp.MustCompile(`\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}(?::\d{2}(?:\.\d+)?)?(?:Z|[+-]\d{2}:?\d{2})?`)
)

// canon holds the numbering of one request: the n-th distinct ULID is <IDn>, the n-th
// distinct tool-call id <CALLn>, by first appearance.
type canon struct {
	home  []string
	ids   map[string]string
	calls map[string]string
}

// Canonicalize canonicalises a request. model is ignored (see CanonicalRequest).
func Canonicalize(_ string, msgs []protocoltypes.Message, tools []protocoltypes.ToolDefinition, options map[string]any) CanonicalRequest {
	return canonicalize(nil, msgs, tools, options)
}

// canonicalize is Canonicalize that also masks the instance's home directories, which
// differ per run (a random name) and per machine (/var/folders vs /tmp).
func canonicalize(home []string, msgs []protocoltypes.Message, tools []protocoltypes.ToolDefinition, options map[string]any) CanonicalRequest {
	c := &canon{home: home, ids: map[string]string{}, calls: map[string]string{}}
	var out CanonicalRequest
	for _, m := range msgs {
		cm := CanonicalMessage{Role: m.Role, Content: c.text(m.Content), ToolCallID: c.call(m.ToolCallID)}
		for _, p := range m.SystemParts {
			cm.Content += c.text(p.Text)
		}
		for _, tc := range m.ToolCalls {
			cm.ToolCalls = append(cm.ToolCalls, c.toolCall(tc))
		}
		out.Messages = append(out.Messages, cm)
	}
	for _, t := range tools {
		sum := sha256.Sum256(mustJSON(t.Function.Parameters))
		out.Tools = append(out.Tools, t.Function.Name+":"+hex.EncodeToString(sum[:8]))
	}
	switch v := options["max_tokens"].(type) {
	case int:
		out.MaxTokens = v
	case int64:
		out.MaxTokens = int(v)
	case float64:
		out.MaxTokens = int(v)
	}
	return out
}

func (c *canon) toolCall(tc protocoltypes.ToolCall) CanonicalCall {
	name, args := tc.Name, tc.Arguments
	if tc.Function != nil {
		name = cmpOr(name, tc.Function.Name)
		if args == nil && tc.Function.Arguments != "" {
			_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
		}
	}
	return CanonicalCall{ID: c.call(tc.ID), Name: name, Args: c.text(string(mustJSON(args)))}
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v) // maps marshal with sorted keys, so the bytes are stable
	return b
}

func (c *canon) call(id string) string {
	if id == "" {
		return ""
	}
	if p, ok := c.calls[id]; ok {
		return p
	}
	p := fmt.Sprintf("<CALL%d>", len(c.calls)+1)
	c.calls[id] = p
	return p
}

// text applies the placeholder rules to one string. The two prompt lines come first: they
// hold a timestamp the generic rule would otherwise number differently.
func (c *canon) text(s string) string {
	for _, h := range c.home {
		if h != "" {
			s = strings.ReplaceAll(s, h, "<HOME>")
		}
	}
	for _, m := range buildIdentity() {
		s = strings.ReplaceAll(s, m[0], m[1])
	}
	s = reTimeLine.ReplaceAllString(s, "${1}<TIME>")
	s = reRuntimeLine.ReplaceAllString(s, "${1}<RUNTIME>")
	s = reFence.ReplaceAllString(s, "<<<${1}<NONCE>>>>")
	s = reButton.ReplaceAllString(s, "<CODE>.<HEX>")
	s = reTS.ReplaceAllString(s, "<TS>")
	return reULID.ReplaceAllStringFunc(s, func(id string) string {
		if p, ok := c.ids[id]; ok {
			return p
		}
		p := fmt.Sprintf("<ID%d>", len(c.ids)+1)
		c.ids[id] = p
		return p
	})
}

// buildIdentity lists the two headers that print the build, exactly as the agent prints
// them, with the version masked: getIdentity ("# khunquant 🦞 (%s)", pkg/agent/context.go) and
// proactiveIdentity ("# khunquant (%s)", pkg/agent/context_proactive.go), where %s is
// config.FormatVersion(). It differs between `go build` ("dev"), `make build` and commits.
// Only the printed header is matched: a plain build's "dev" must not be touched in "device"
// or "/dev/i2c". BuildTime and GoVersion reach no prompt, so they are not masked. Read on
// every call: a test may change the stamps.
func buildIdentity() [][2]string {
	v := config.FormatVersion()
	if v == "" {
		return nil
	}
	return [][2]string{
		{"# khunquant 🦞 (" + v + ")", "# khunquant 🦞 (<VERSION>)"},
		{"# khunquant (" + v + ")", "# khunquant (<VERSION>)"},
	}
}

// Key is the sha256 (hex) of the canonical request: the key of a cassette entry.
func Key(c CanonicalRequest) string {
	sum := sha256.Sum256(mustJSON(c))
	return hex.EncodeToString(sum[:])
}
