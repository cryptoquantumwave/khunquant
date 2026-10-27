// Package labclient is the HTTP client of a locked sandbox instance: the lab groups
// (POST /api/sandbox/lab/<group>), the group list, /health and /api/sandbox/status. It
// talks to loopback only, with the instance token as bearer. The supervisor and the
// scenario runner use it; the token never leaves the process that holds the Client.
package labclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labjournal"
)

const (
	callTimeout = 30 * time.Second // a lab call lasts 25 s at most (labjournal.MaxWait)
	maxReply    = 16 << 20
)

// Error is a lab failure: the HTTP status and the {"error","code"} body of lab.Error.
type Error struct {
	Status int
	Code   string // "unknown_group", "bad_args", ... ; empty when the body was not a lab error
	Msg    string
}

func (e *Error) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("lab: HTTP %d: %s", e.Status, e.Msg)
	}
	return e.Code + ": " + e.Msg
}

// GroupInfo is one lab group as GET /api/sandbox/lab lists it.
type GroupInfo struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Ops         []OpInfo `json:"ops"`
}

// OpInfo is one op; Args is the JSON Schema "properties" object of its arguments.
type OpInfo struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Args        json.RawMessage `json:"args,omitempty"`
	Required    []string        `json:"required,omitempty"`
}

// Client talks to one instance.
type Client struct {
	base  string
	token string
	hc    *http.Client
}

// New returns a Client for baseURL (http://127.0.0.1:port). It refuses any other host:
// the token must never be sent beyond the local machine.
func New(baseURL, token string) (*Client, error) {
	if token == "" {
		return nil, errors.New("labclient: an instance token is required")
	}
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme != "http" || u.Host == "" {
		return nil, fmt.Errorf("labclient: %q is not an http URL", baseURL)
	}
	if ip := net.ParseIP(u.Hostname()); u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return nil, fmt.Errorf("labclient: %q is not a loopback address; the instance token stays on this machine", baseURL)
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	return &Client{
		base:  strings.TrimRight(baseURL, "/"),
		token: token,
		hc: &http.Client{
			Timeout:       callTimeout,
			Transport:     tr,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

func (c *Client) do(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, c.mask(err.Error())
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxReply))
	if err != nil {
		return nil, err
	}
	data = bytes.ReplaceAll(data, []byte(c.token), []byte("***")) // nothing an instance says may carry its token
	if resp.StatusCode != http.StatusOK {
		le := &Error{Status: resp.StatusCode, Msg: strings.TrimSpace(string(data))}
		var eb struct{ Error, Code string }
		if json.Unmarshal(data, &eb) == nil && eb.Error != "" {
			le.Code, le.Msg = eb.Code, eb.Error
		}
		return nil, le
	}
	return data, nil
}

func (c *Client) mask(s string) error { return errors.New(strings.ReplaceAll(s, c.token, "***")) }

// Health fails unless /health answers 200.
func (c *Client) Health(ctx context.Context) error {
	_, err := c.do(ctx, http.MethodGet, "/health", nil)
	return err
}

// Status reports whether the instance says it is an enabled, locked sandbox. An error
// means it did not answer, or did not accept the token.
func (c *Client) Status(ctx context.Context) (locked bool, err error) {
	data, err := c.do(ctx, http.MethodGet, "/api/sandbox/status", nil)
	if err != nil {
		return false, err
	}
	var st struct{ Enabled, Locked bool }
	if err := json.Unmarshal(data, &st); err != nil {
		return false, fmt.Errorf("labclient: status: %w", err)
	}
	return st.Enabled && st.Locked, nil
}

// Groups lists the lab groups of the instance.
func (c *Client) Groups(ctx context.Context) ([]GroupInfo, error) {
	data, err := c.do(ctx, http.MethodGet, "/api/sandbox/lab", nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Groups []GroupInfo `json:"groups"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("labclient: groups: %w", err)
	}
	return out.Groups, nil
}

// Call posts body ({"op": ..., ...args}) to a lab group and returns the JSON result. A
// lab failure is an *Error.
func (c *Client) Call(ctx context.Context, group string, body json.RawMessage) (json.RawMessage, error) {
	if len(body) == 0 {
		body = json.RawMessage(`{}`)
	}
	return c.do(ctx, http.MethodPost, "/api/sandbox/lab/"+url.PathEscape(group), body)
}

// WaitFor runs one inspect wait_for: it blocks up to wall (the instance clamps it to
// labjournal.MaxWait) for entries matching m after afterSeq.
func (c *Client) WaitFor(ctx context.Context, m labjournal.Match, afterSeq int64, wall time.Duration) (labjournal.WaitResult, error) {
	body, err := json.Marshal(map[string]any{"op": "wait_for", "match": m, "after_seq": afterSeq, "timeout_wall": wall.String()})
	if err != nil {
		return labjournal.WaitResult{}, err
	}
	data, err := c.Call(ctx, "inspect", body)
	if err != nil {
		return labjournal.WaitResult{}, err
	}
	var res labjournal.WaitResult
	if err := json.Unmarshal(data, &res); err != nil {
		return labjournal.WaitResult{}, fmt.Errorf("labclient: wait_for: %w", err)
	}
	return res, nil
}
