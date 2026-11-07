package sim

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"github.com/cryptoquantumwave/khunquant/pkg/providers/protocoltypes"
)

// Miss policies of a cassette: what a request with no entry gets.
const (
	MissError    = "error"    // the call fails and the journal says which key was missing
	MissScripted = "scripted" // the scripted rules answer
)

// maxCassetteLine bounds one JSONL line (a response with a long text).
const maxCassetteLine = 4 << 20

// Entry is one line of a cassette (JSONL): the key, the canonical request it was made
// from (so a reviewer can see what the key matches), the answer and the usage.
type Entry struct {
	Key      string                   `json:"key"`
	Request  CanonicalRequest         `json:"request"`
	Response Reply                    `json:"response"`
	Usage    *protocoltypes.UsageInfo `json:"usage,omitempty"`
}

// Reply is a model answer as a cassette stores it. Tool-call ids are not kept: replay
// mints its own.
type Reply struct {
	Content          string      `json:"content,omitempty"`
	ReasoningContent string      `json:"reasoning_content,omitempty"`
	FinishReason     string      `json:"finish_reason,omitempty"`
	ToolCalls        []ReplyCall `json:"tool_calls,omitempty"`
}

// ReplyCall is a tool call of a Reply.
type ReplyCall struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args,omitempty"`
}

// ReplyOf stores a response.
func ReplyOf(r *protocoltypes.LLMResponse) Reply {
	out := Reply{Content: r.Content, ReasoningContent: r.ReasoningContent, FinishReason: r.FinishReason}
	for _, tc := range r.ToolCalls {
		name, args := tc.Name, tc.Arguments
		if tc.Function != nil {
			name = cmpOr(name, tc.Function.Name)
			if args == nil && tc.Function.Arguments != "" {
				_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
			}
		}
		out.ToolCalls = append(out.ToolCalls, ReplyCall{Name: name, Args: args})
	}
	return out
}

// response rebuilds the answer; newID mints each tool call's id.
func (e Entry) response(newID func() string) *protocoltypes.LLMResponse {
	r := &protocoltypes.LLMResponse{
		Content: e.Response.Content, ReasoningContent: e.Response.ReasoningContent,
		FinishReason: e.Response.FinishReason, Usage: e.Usage,
	}
	for _, c := range e.Response.ToolCalls {
		args := argsCopy(c.Args)
		raw, _ := json.Marshal(args)
		r.ToolCalls = append(r.ToolCalls, protocoltypes.ToolCall{
			ID: newID(), Type: "function", Name: c.Name, Arguments: args,
			Function: &protocoltypes.FunctionCall{Name: c.Name, Arguments: string(raw)},
		})
	}
	return r
}

// ParseEntries reads a JSONL cassette. A line whose key is not the key of its own
// request is refused: a hand edit that would otherwise never match.
func ParseEntries(r io.Reader) ([]Entry, error) {
	var out []Entry
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxCassetteLine)
	for n := 1; sc.Scan(); n++ {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var e Entry
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&e); err != nil {
			return nil, fmt.Errorf("cassette line %d: %w", n, err)
		}
		if err := e.check(); err != nil {
			return nil, fmt.Errorf("cassette line %d: %w", n, err)
		}
		out = append(out, e)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("cassette: %w", err)
	}
	return out, nil
}

func (e Entry) check() error {
	if e.Key == "" || len(e.Request.Messages) == 0 {
		return fmt.Errorf("an entry needs a key and the canonical request it was made from")
	}
	if want := Key(e.Request); want != e.Key {
		return fmt.Errorf("key %.12s… is not the key of its request (%.12s…)", e.Key, want)
	}
	return nil
}

// Cassette is the replay set. Entries with one key answer in order, and the last one
// repeats: a heartbeat that asks the same thing twice can be answered differently.
type Cassette struct {
	mu      sync.Mutex
	miss    string
	entries map[string][]Entry
	used    map[string]int
}

// NewCassette builds a replay set; miss is MissError (the default) or MissScripted.
func NewCassette(entries []Entry, miss string) (*Cassette, error) {
	switch miss {
	case "":
		miss = MissError
	case MissError, MissScripted:
	default:
		return nil, fmt.Errorf("miss must be %q or %q", MissError, MissScripted)
	}
	c := &Cassette{miss: miss, entries: map[string][]Entry{}, used: map[string]int{}}
	for _, e := range entries {
		if err := e.check(); err != nil {
			return nil, err
		}
		c.entries[e.Key] = append(c.entries[e.Key], e)
	}
	return c, nil
}

// next returns the entry for key and advances that key's position.
func (c *Cassette) next(key string) (Entry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	es := c.entries[key]
	if len(es) == 0 {
		return Entry{}, false
	}
	i := min(c.used[key], len(es)-1)
	c.used[key]++
	return es[i], true
}

// Len is the number of entries.
func (c *Cassette) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, es := range c.entries {
		n += len(es)
	}
	return n
}
