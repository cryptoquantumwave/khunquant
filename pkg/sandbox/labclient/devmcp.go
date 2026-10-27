package labclient

import (
	"context"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// devMCPPath is the instance's dev-MCP endpoint (config.Debug.DevMCP.PathPrefix, which
// the sandbox profile leaves at its default). Under the lock its bearer is the instance token.
const devMCPPath = "/dev-mcp"

type bearer struct {
	token string
	next  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.next.RoundTrip(r)
}

// devSession opens an MCP session to the instance's dev-MCP server. It is per call: the
// server answers with plain JSON (no SSE stream), so a session costs three requests and
// survives no restart that would have to be detected.
func (c *Client) devSession(ctx context.Context) (*mcp.ClientSession, error) {
	hc := &http.Client{Timeout: callTimeout, Transport: bearer{c.token, c.hc.Transport}, CheckRedirect: c.hc.CheckRedirect}
	t := &mcp.StreamableClientTransport{Endpoint: c.base + devMCPPath, HTTPClient: hc, DisableStandaloneSSE: true, MaxRetries: -1}
	return mcp.NewClient(&mcp.Implementation{Name: "khunquant-sandbox", Version: "1"}, nil).Connect(ctx, t, nil)
}

// DevTools lists the tools of the instance's dev-MCP server.
func (c *Client) DevTools(ctx context.Context) ([]*mcp.Tool, error) {
	cs, err := c.devSession(ctx)
	if err != nil {
		return nil, c.mask(err.Error())
	}
	defer cs.Close()
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		return nil, c.mask(err.Error())
	}
	return res.Tools, nil
}

// CallDevTool calls one dev-MCP tool and returns its result, with the token masked out
// of every text part.
func (c *Client) CallDevTool(ctx context.Context, name string, args map[string]any) (*mcp.CallToolResult, error) {
	cs, err := c.devSession(ctx)
	if err != nil {
		return nil, c.mask(err.Error())
	}
	defer cs.Close()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return nil, c.mask(err.Error())
	}
	for _, part := range res.Content {
		if t, ok := part.(*mcp.TextContent); ok {
			t.Text = strings.ReplaceAll(t.Text, c.token, "***")
		}
	}
	return res, nil
}
