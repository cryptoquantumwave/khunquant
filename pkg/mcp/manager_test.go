package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

func TestLoadEnvFile(t *testing.T) {
	tests := []struct {
		name      string
		content   string
		expected  map[string]string
		expectErr bool
	}{
		{
			name: "basic env file",
			content: `API_KEY=secret123
DATABASE_URL=postgres://localhost/db
PORT=8080`,
			expected: map[string]string{
				"API_KEY":      "secret123",
				"DATABASE_URL": "postgres://localhost/db",
				"PORT":         "8080",
			},
			expectErr: false,
		},
		{
			name: "with comments and empty lines",
			content: `# This is a comment
API_KEY=secret123

# Another comment
DATABASE_URL=postgres://localhost/db

PORT=8080`,
			expected: map[string]string{
				"API_KEY":      "secret123",
				"DATABASE_URL": "postgres://localhost/db",
				"PORT":         "8080",
			},
			expectErr: false,
		},
		{
			name: "with quoted values",
			content: `API_KEY="secret with spaces"
NAME='single quoted'
PLAIN=no-quotes`,
			expected: map[string]string{
				"API_KEY": "secret with spaces",
				"NAME":    "single quoted",
				"PLAIN":   "no-quotes",
			},
			expectErr: false,
		},
		{
			name: "with spaces around equals",
			content: `API_KEY = secret123
DATABASE_URL= postgres://localhost/db
PORT =8080`,
			expected: map[string]string{
				"API_KEY":      "secret123",
				"DATABASE_URL": "postgres://localhost/db",
				"PORT":         "8080",
			},
			expectErr: false,
		},
		{
			name:      "invalid format - no equals",
			content:   `INVALID_LINE`,
			expectErr: true,
		},
		{
			name:      "empty file",
			content:   ``,
			expected:  map[string]string{},
			expectErr: false,
		},
		{
			name: "only comments",
			content: `# Comment 1
# Comment 2`,
			expected:  map[string]string{},
			expectErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			envFile := filepath.Join(tmpDir, ".env")

			if err := os.WriteFile(envFile, []byte(tt.content), 0o644); err != nil {
				t.Fatalf("Failed to create test file: %v", err)
			}

			result, err := loadEnvFile(envFile)

			if tt.expectErr {
				if err == nil {
					t.Errorf("Expected error but got none")
				}
				return
			}

			if err != nil {
				t.Errorf("Unexpected error: %v", err)
				return
			}

			if len(result) != len(tt.expected) {
				t.Errorf("Expected %d variables, got %d", len(tt.expected), len(result))
			}

			for key, expectedValue := range tt.expected {
				if actualValue, ok := result[key]; !ok {
					t.Errorf("Expected key %s not found", key)
				} else if actualValue != expectedValue {
					t.Errorf("For key %s: expected %q, got %q", key, expectedValue, actualValue)
				}
			}
		})
	}
}

func TestLoadEnvFileNotFound(t *testing.T) {
	_, err := loadEnvFile("/nonexistent/file.env")
	if err == nil {
		t.Error("Expected error for nonexistent file")
	}
}

func TestExpandHomeCommandPath(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("USERPROFILE", homeDir)

	want := filepath.Join(homeDir, "bin", "my-mcp")
	got := expandHomeCommandPath("~" + string(os.PathSeparator) + filepath.Join("bin", "my-mcp"))
	if got != want {
		t.Fatalf("expandHomeCommandPath() = %q, want %q", got, want)
	}

	if got := expandHomeCommandPath("npx"); got != "npx" {
		t.Fatalf("expandHomeCommandPath() should leave bare commands unchanged, got %q", got)
	}
}

func TestEnvFilePriority(t *testing.T) {
	// Create a temporary .env file
	tmpDir := t.TempDir()
	envFile := filepath.Join(tmpDir, ".env")

	envContent := `API_KEY=from_file
DATABASE_URL=from_file
SHARED_VAR=from_file`

	if err := os.WriteFile(envFile, []byte(envContent), 0o644); err != nil {
		t.Fatalf("Failed to create .env file: %v", err)
	}

	// Load envFile
	envVars, err := loadEnvFile(envFile)
	if err != nil {
		t.Fatalf("Failed to load env file: %v", err)
	}

	// Verify envFile variables
	if envVars["API_KEY"] != "from_file" {
		t.Errorf("Expected API_KEY=from_file, got %s", envVars["API_KEY"])
	}

	// Simulate config.Env overriding envFile
	configEnv := map[string]string{
		"SHARED_VAR": "from_config",
		"NEW_VAR":    "from_config",
	}

	// Merge: envFile first, then config overrides
	merged := make(map[string]string)
	for k, v := range envVars {
		merged[k] = v
	}
	for k, v := range configEnv {
		merged[k] = v
	}

	// Verify priority: config.Env should override envFile
	if merged["SHARED_VAR"] != "from_config" {
		t.Errorf(
			"Expected SHARED_VAR=from_config (config should override file), got %s",
			merged["SHARED_VAR"],
		)
	}
	if merged["API_KEY"] != "from_file" {
		t.Errorf("Expected API_KEY=from_file, got %s", merged["API_KEY"])
	}
	if merged["NEW_VAR"] != "from_config" {
		t.Errorf("Expected NEW_VAR=from_config, got %s", merged["NEW_VAR"])
	}
}

func TestLoadFromMCPConfig_EmptyWorkspaceWithRelativeEnvFile(t *testing.T) {
	mgr := NewManager()

	mcpCfg := config.MCPConfig{
		ToolConfig: config.ToolConfig{
			Enabled: true,
		},
		Servers: map[string]config.MCPServerConfig{
			"test-server": {
				Enabled: true,
				Command: "echo",
				Args:    []string{"ok"},
				EnvFile: ".env",
			},
		},
	}

	err := mgr.LoadFromMCPConfig(context.Background(), mcpCfg, "")
	if err == nil {
		t.Fatal("expected error for relative env_file with empty workspace path, got nil")
	}

	if !strings.Contains(err.Error(), "workspace path is empty") {
		t.Fatalf("expected workspace path validation error, got: %v", err)
	}
}

func TestNewManager_InitialState(t *testing.T) {
	mgr := NewManager()
	if mgr == nil {
		t.Fatal("expected manager instance, got nil")
	}
	if len(mgr.GetServers()) != 0 {
		t.Fatalf("expected no servers on new manager, got %d", len(mgr.GetServers()))
	}
}

func TestLoadFromMCPConfig_DisabledOrEmptyServers(t *testing.T) {
	mgr := NewManager()

	err := mgr.LoadFromMCPConfig(
		context.Background(),
		config.MCPConfig{ToolConfig: config.ToolConfig{Enabled: false}},
		"/tmp",
	)
	if err != nil {
		t.Fatalf("expected nil error when MCP disabled, got: %v", err)
	}

	err = mgr.LoadFromMCPConfig(
		context.Background(),
		config.MCPConfig{ToolConfig: config.ToolConfig{Enabled: true}},
		"/tmp",
	)
	if err != nil {
		t.Fatalf("expected nil error when no servers configured, got: %v", err)
	}
}

func TestGetServers_ReturnsCopy(t *testing.T) {
	mgr := NewManager()
	mgr.servers["s1"] = &ServerConnection{Name: "s1"}

	servers := mgr.GetServers()
	delete(servers, "s1")

	if _, ok := mgr.GetServer("s1"); !ok {
		t.Fatal("expected internal manager state to remain unchanged")
	}
}

func TestGetAllTools_FiltersEmptyTools(t *testing.T) {
	mgr := NewManager()
	mgr.servers["empty"] = &ServerConnection{Name: "empty", Tools: nil}
	mgr.servers["with-tools"] = &ServerConnection{Name: "with-tools", Tools: []*sdkmcp.Tool{{}}}

	all := mgr.GetAllTools()
	if _, ok := all["empty"]; ok {
		t.Fatal("expected server without tools to be excluded")
	}
	if _, ok := all["with-tools"]; !ok {
		t.Fatal("expected server with tools to be included")
	}
}

func TestCallTool_ErrorsForClosedOrMissingServer(t *testing.T) {
	t.Run("manager closed", func(t *testing.T) {
		mgr := NewManager()
		mgr.closed.Store(true)

		_, err := mgr.CallTool(context.Background(), "s1", "tool", nil)
		if err == nil || !strings.Contains(err.Error(), "manager is closed") {
			t.Fatalf("expected manager closed error, got: %v", err)
		}
	})

	t.Run("server missing", func(t *testing.T) {
		mgr := NewManager()

		_, err := mgr.CallTool(context.Background(), "missing", "tool", nil)
		if err == nil || !strings.Contains(err.Error(), "not found") {
			t.Fatalf("expected server not found error, got: %v", err)
		}
	})
}

func TestCallTool_ReconnectsWhenHTTPServerLosesSession(t *testing.T) {
	originalConnectServerFunc := connectServerFunc
	t.Cleanup(func() {
		connectServerFunc = originalConnectServerFunc
	})

	staleConn, staleTransport, err := newScriptedServerConnection(
		"session-1",
		nil,
		fmt.Errorf(`sending "tools/call": failed to connect (session ID: session-1): %w`, sdkmcp.ErrSessionMissing),
	)
	if err != nil {
		t.Fatalf("newScriptedServerConnection(stale) error = %v", err)
	}
	freshConn, freshTransport, err := newScriptedServerConnection(
		"session-2",
		&sdkmcp.CallToolResult{
			Content: []sdkmcp.Content{
				&sdkmcp.TextContent{Text: "reconnected"},
			},
		},
		nil,
	)
	if err != nil {
		t.Fatalf("newScriptedServerConnection(fresh) error = %v", err)
	}

	connectCalls := 0
	connectServerFunc = func(ctx context.Context, name string, cfg config.MCPServerConfig) (*ServerConnection, error) {
		connectCalls++
		if connectCalls == 1 {
			return freshConn, nil
		}
		return nil, fmt.Errorf("unexpected reconnect attempt %d", connectCalls)
	}

	mgr := NewManager()
	mgr.servers["flaky"] = staleConn

	result, err := mgr.CallTool(context.Background(), "flaky", "echo", map[string]any{
		"query": "hello",
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if result == nil || len(result.Content) != 1 {
		t.Fatalf("CallTool() returned unexpected content: %#v", result)
	}

	text, ok := result.Content[0].(*sdkmcp.TextContent)
	if !ok {
		t.Fatalf("CallTool() content type = %T, want *sdkmcp.TextContent", result.Content[0])
	}
	if text.Text != "reconnected" {
		t.Fatalf("CallTool() text = %q, want %q", text.Text, "reconnected")
	}

	conn, ok := mgr.GetServer("flaky")
	if !ok {
		t.Fatal("expected flaky server to remain connected after reconnect")
	}
	if conn.Session.ID() != "session-2" {
		t.Fatalf("Session.ID() = %q, want %q", conn.Session.ID(), "session-2")
	}
	if connectCalls != 1 {
		t.Fatalf("connectCalls = %d, want 1", connectCalls)
	}
	if staleTransport.toolCallCalls != 1 {
		t.Fatalf("stale toolCallCalls = %d, want 1", staleTransport.toolCallCalls)
	}
	if freshTransport.toolCallCalls != 1 {
		t.Fatalf("fresh toolCallCalls = %d, want 1", freshTransport.toolCallCalls)
	}
}

func TestClose_IdempotentOnEmptyManager(t *testing.T) {
	mgr := NewManager()

	if err := mgr.Close(); err != nil {
		t.Fatalf("first close should succeed, got: %v", err)
	}
	if err := mgr.Close(); err != nil {
		t.Fatalf("second close should be idempotent, got: %v", err)
	}
}

// TestRoundTrip tests the headerTransport RoundTrip method
func TestHeaderTransport_RoundTrip(t *testing.T) {
	// Create a mock transport for testing
	mockTransport := &mockHTTPTransport{
		roundTripFunc: func(req *http.Request) (*http.Response, error) {
			// Check that headers were added before this is called
			if req.Header.Get("X-Custom") != "value" {
				t.Error("expected X-Custom header to be set")
			}
			return &http.Response{StatusCode: 200}, nil
		},
	}

	// Create headerTransport with the mock
	transport := &headerTransport{
		base: mockTransport,
		headers: map[string]string{
			"X-Custom": "value",
		},
	}

	// Create a test request
	req, err := http.NewRequest("GET", "http://example.com", nil)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}

	// Call RoundTrip
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip failed: %v", err)
	}
	if resp == nil {
		t.Fatal("expected response, got nil")
	}
}

// mockHTTPTransport implements http.RoundTripper for testing
type mockHTTPTransport struct {
	roundTripFunc func(*http.Request) (*http.Response, error)
}

func (m *mockHTTPTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if m.roundTripFunc != nil {
		return m.roundTripFunc(req)
	}
	return &http.Response{StatusCode: 200}, nil
}

// TestLoadFromConfig tests the LoadFromConfig method
func TestLoadFromConfig(t *testing.T) {
	mgr := NewManager()
	tmpDir := t.TempDir()

	cfg := &config.Config{
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				Workspace: tmpDir,
			},
		},
		Tools: config.ToolsConfig{
			MCP: config.MCPConfig{
				ToolConfig: config.ToolConfig{
					Enabled: false, // Disabled, so no servers to load
				},
			},
		},
	}

	err := mgr.LoadFromConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("LoadFromConfig failed: %v", err)
	}

	if len(mgr.GetServers()) != 0 {
		t.Fatalf("expected no servers with MCP disabled, got %d", len(mgr.GetServers()))
	}
}

// TestConnectServer tests ConnectServer error handling
func TestConnectServer_Errors(t *testing.T) {
	mgr := NewManager()

	tests := []struct {
		name    string
		cfg     config.MCPServerConfig
		wantErr string
	}{
		{
			name:    "no URL and no command",
			cfg:     config.MCPServerConfig{Type: ""},
			wantErr: "either URL or command must be provided",
		},
		{
			name:    "SSE without URL",
			cfg:     config.MCPServerConfig{Type: "sse"},
			wantErr: "URL is required for SSE/HTTP transport",
		},
		{
			name:    "stdio without command",
			cfg:     config.MCPServerConfig{Type: "stdio"},
			wantErr: "command is required for stdio transport",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := mgr.ConnectServer(context.Background(), "test-server", tt.cfg)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected error containing %q, got: %v", tt.wantErr, err)
			}
		})
	}
}

func TestConnectServer_UnsupportedTransportType(t *testing.T) {
	mgr := NewManager()
	cfg := config.MCPServerConfig{Type: "grpc", URL: "http://localhost:8080"}
	err := mgr.ConnectServer(context.Background(), "test-server", cfg)
	if err == nil {
		t.Fatal("unsupported transport type should return error")
	}
	if !strings.Contains(err.Error(), "unsupported transport type") {
		t.Errorf("expected 'unsupported transport type' error, got: %v", err)
	}
}

func TestConnectServer_SSEAutoDetect_FailsAtConnect(t *testing.T) {
	mgr := NewManager()
	cfg := config.MCPServerConfig{URL: "http://localhost:19999/mcp/invalid-endpoint-test"}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	err := mgr.ConnectServer(ctx, "autodetect-server", cfg)
	// Expected to fail at client.Connect since no server is listening — but SSE path is covered
	if err == nil {
		t.Error("ConnectServer SSE without real server should fail (no server running)")
	}
}

func TestConnectServer_SSE_WithHeaders_FailsAtConnect(t *testing.T) {
	mgr := NewManager()
	cfg := config.MCPServerConfig{
		Type:    "sse",
		URL:     "http://localhost:19999/mcp/invalid-headers-test",
		Headers: map[string]string{"Authorization": "Bearer test-token"},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	err := mgr.ConnectServer(ctx, "header-server", cfg)
	// Expected to fail at client.Connect — headers path is covered
	if err == nil {
		t.Error("ConnectServer SSE with headers but no real server should fail")
	}
}

func TestCallTool_ManagerClosed(t *testing.T) {
	mgr := NewManager()
	mgr.closed.Store(true)
	_, err := mgr.CallTool(context.Background(), "server", "tool", nil)
	if err == nil {
		t.Error("CallTool on closed manager should return error")
	}
}

func TestLoadFromMCPConfig_DisabledMCP(t *testing.T) {
	mgr := NewManager()
	cfg := config.MCPConfig{}
	cfg.ToolConfig.Enabled = false
	err := mgr.LoadFromMCPConfig(context.Background(), cfg, "")
	if err != nil {
		t.Errorf("LoadFromMCPConfig disabled should return nil, got: %v", err)
	}
}

func TestLoadFromMCPConfig_AllServersDisabled(t *testing.T) {
	mgr := NewManager()
	cfg := config.MCPConfig{
		Servers: map[string]config.MCPServerConfig{
			"s1": {Enabled: false, Command: "echo"},
		},
	}
	cfg.ToolConfig.Enabled = true
	err := mgr.LoadFromMCPConfig(context.Background(), cfg, t.TempDir())
	if err != nil {
		t.Errorf("LoadFromMCPConfig all disabled should return nil, got: %v", err)
	}
}

func newScriptedServerConnection(
	sessionID string,
	toolCallResult *sdkmcp.CallToolResult,
	toolCallErr error,
) (*ServerConnection, *scriptedTransport, error) {
	transport := &scriptedTransport{
		sessionID:      sessionID,
		toolCallResult: toolCallResult,
		toolCallErr:    toolCallErr,
	}

	client := sdkmcp.NewClient(&sdkmcp.Implementation{
		Name:    "picoclaw-test",
		Version: "1.0.0",
	}, nil)
	session, err := client.Connect(context.Background(), transport, nil)
	if err != nil {
		return nil, nil, err
	}

	return &ServerConnection{
		Name:    "flaky",
		Config:  config.MCPServerConfig{Enabled: true, Type: "http", URL: "https://example.invalid/mcp"},
		Client:  client,
		Session: session,
		Tools: []*sdkmcp.Tool{
			{
				Name:        "echo",
				Description: "Echo test tool",
				InputSchema: map[string]any{"type": "object"},
			},
		},
	}, transport, nil
}

type scriptedTransport struct {
	sessionID      string
	toolCallResult *sdkmcp.CallToolResult
	toolCallErr    error

	mu            sync.Mutex
	toolCallCalls int
	closed        bool
	incoming      chan jsonrpc.Message
}

func (t *scriptedTransport) Connect(context.Context) (sdkmcp.Connection, error) {
	if t.incoming == nil {
		t.incoming = make(chan jsonrpc.Message, 4)
	}
	return t, nil
}

func (t *scriptedTransport) Read(ctx context.Context) (jsonrpc.Message, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case msg, ok := <-t.incoming:
		if !ok {
			return nil, io.EOF
		}
		return msg, nil
	}
}

func (t *scriptedTransport) Write(ctx context.Context, msg jsonrpc.Message) error {
	req, ok := msg.(*jsonrpc.Request)
	if !ok {
		return nil
	}

	switch req.Method {
	case "server/discover":
		// go-sdk v1.7.0 (protocol 2026-07-28, SEP-2575) probes server/discover
		// before falling back to legacy initialize. This scripted server only
		// speaks the legacy handshake, so answer with a JSON-RPC MethodNotFound
		// response — the same signal a real pre-2026-07-28 server sends — which
		// triggers the client's fallback. Returning a Go error here instead would
		// tear down the transport and fail the handshake outright.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case t.incoming <- &jsonrpc.Response{
			ID: req.ID,
			Error: &jsonrpc.Error{
				Code:    jsonrpc.CodeMethodNotFound,
				Message: `method not found: "server/discover"`,
			},
		}:
			return nil
		}

	case "initialize":
		payload, err := json.Marshal(&sdkmcp.InitializeResult{
			ProtocolVersion: "2025-11-25",
			ServerInfo: &sdkmcp.Implementation{
				Name:    "scripted-test-server",
				Version: "1.0.0",
			},
			Capabilities: &sdkmcp.ServerCapabilities{
				Tools: &sdkmcp.ToolCapabilities{},
			},
		})
		if err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case t.incoming <- &jsonrpc.Response{ID: req.ID, Result: payload}:
			return nil
		}

	case "notifications/initialized":
		return nil

	case "tools/call":
		t.mu.Lock()
		t.toolCallCalls++
		t.mu.Unlock()

		if t.toolCallErr != nil {
			return t.toolCallErr
		}

		payload, err := json.Marshal(t.toolCallResult)
		if err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case t.incoming <- &jsonrpc.Response{ID: req.ID, Result: payload}:
			return nil
		}
	}

	return fmt.Errorf("unexpected method %q", req.Method)
}

func (t *scriptedTransport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil
	}
	t.closed = true
	close(t.incoming)
	return nil
}

func (t *scriptedTransport) SessionID() string {
	return t.sessionID
}

func TestShouldReconnectCallError_StringMatch(t *testing.T) {
	// errors.Is is false but string contains the sentinel text
	err := fmt.Errorf("proxy error: Session Not Found")
	if !shouldReconnectCallError(err) {
		t.Error("shouldReconnectCallError should return true when error string contains sentinel text")
	}
}

func TestShouldReconnectCallError_UnrelatedError(t *testing.T) {
	err := fmt.Errorf("connection timeout")
	if shouldReconnectCallError(err) {
		t.Error("shouldReconnectCallError should return false for unrelated errors")
	}
}

func TestExpandHomeCommandPath_TildeOnly(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("USERPROFILE", homeDir)

	got := expandHomeCommandPath("~")
	// Should return the home directory itself (not leave it as "~")
	if got == "~" {
		t.Errorf("expandHomeCommandPath('~') = %q, expected home directory", got)
	}
}

func TestExpandHomeCommandPath_Empty(t *testing.T) {
	if got := expandHomeCommandPath(""); got != "" {
		t.Errorf("expandHomeCommandPath('') = %q, want empty", got)
	}
}

func TestExpandHomeCommandPath_TildeNoSlash(t *testing.T) {
	// ~username form should be left unchanged
	got := expandHomeCommandPath("~otheruser/bin")
	if got != "~otheruser/bin" {
		t.Errorf("expandHomeCommandPath(~otheruser/bin) = %q, want unchanged", got)
	}
}

func TestReconnectServer_NilStaleConn(t *testing.T) {
	mgr := NewManager()
	_, err := mgr.reconnectServer(context.Background(), "missing", nil)
	if err == nil {
		t.Error("reconnectServer(nil) should return error")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' in error, got: %v", err)
	}
}

func TestReconnectServer_ClosedManager(t *testing.T) {
	conn, _, err := newScriptedServerConnection("session-close", nil, nil)
	if err != nil {
		t.Fatalf("newScriptedServerConnection error = %v", err)
	}

	mgr := NewManager()
	mgr.closed.Store(true)
	_, reconnErr := mgr.reconnectServer(context.Background(), "server", conn)
	if reconnErr == nil {
		t.Error("reconnectServer on closed manager should return error")
	}
}

func TestConnectServer_ManagerClosedAfterConnect(t *testing.T) {
	originalFunc := connectServerFunc
	t.Cleanup(func() { connectServerFunc = originalFunc })

	conn, _, err := newScriptedServerConnection("session-test", nil, nil)
	if err != nil {
		t.Fatalf("newScriptedServerConnection error = %v", err)
	}

	mgr := NewManager()
	connectServerFunc = func(ctx context.Context, name string, cfg config.MCPServerConfig) (*ServerConnection, error) {
		// Simulate manager being closed between connect and registration
		mgr.closed.Store(true)
		return conn, nil
	}

	err = mgr.ConnectServer(context.Background(), "test", config.MCPServerConfig{})
	if err == nil {
		t.Error("ConnectServer should return error when manager is closed after connect")
	}
	if !strings.Contains(err.Error(), "manager is closed") {
		t.Errorf("expected 'manager is closed' error, got: %v", err)
	}
}
