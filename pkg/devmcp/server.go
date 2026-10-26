package devmcp

import (
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cryptoquantumwave/khunquant/pkg/agent"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/debugtap"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab"
)

// Deps holds the runtime objects the MCP server reads from.
type Deps struct {
	Loop     *agent.AgentLoop
	DebugTap *debugtap.Store
	LogBuf   *debugtap.LogBuffer
	Cfg      *config.Config
	Lab      *lab.Lab // nil unless the process is locked
}

// NewMCPServer constructs an mcp.Server with read-only and sandbox write tools registered.
// Tools are registered in three phases:
//  1. registerReadOnlyTools (tools.go) — read-only diagnostic tools
//  2. registerSandboxTools (sandbox_tools.go) — write-capable sandbox fixture management
//  3. registerLabTools (lab_tools.go) — the lab's sandbox_<group> tools, locked instances only
//
// All registrations use s.AddTool, the ONLY site where tools are added.
func NewMCPServer(d Deps) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{
		Name:    "khunquant-dev",
		Version: "1.0.0",
	}, nil)

	registerReadOnlyTools(s, d) // defined in tools.go
	registerSandboxTools(s, d)  // defined in sandbox_tools.go
	registerLabTools(s, d)      // defined in lab_tools.go

	return s
}

// NewHTTPHandler returns an http.Handler that speaks Streamable HTTP MCP.
// Use JSONResponse:true to avoid triggering the shared gateway's 30s WriteTimeout
// (each response is a single application/json body, no hanging SSE stream).
// DNS-rebinding and cross-origin protections are kept ON (SDK defaults).
func NewHTTPHandler(d Deps) http.Handler {
	return mcp.NewStreamableHTTPHandler(
		func(r *http.Request) *mcp.Server { return NewMCPServer(d) },
		&mcp.StreamableHTTPOptions{JSONResponse: true},
	)
}
