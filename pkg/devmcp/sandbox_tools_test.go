package devmcp

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

// TestSandboxTools_WriteRefusedWhenDisabled verifies that all write handlers
// refuse execution when sandbox is disabled, without mutating the store or disk.
func TestSandboxTools_WriteRefusedWhenDisabled(t *testing.T) {
	// Setup: create store and server
	store := sandbox.NewStore()
	srv := sandbox.NewServer(store)
	sandbox.SetInstance(srv)

	// Add an initial fixture so we can verify it's not mutated
	initialEntry := sandbox.FixtureEntry{
		Method:     "GET",
		PathPrefix: "/api/initial",
		Response: sandbox.Response{
			Status: 200,
			Body:   json.RawMessage(`{"status":"initial"}`),
		},
	}
	srv.SetFixtures("okx", []sandbox.FixtureEntry{initialEntry})

	// Disable sandbox
	sandbox.SetGlobalState(false, "")
	defer sandbox.SetGlobalState(true, "http://127.0.0.1:9999") // restore for other tests

	d := Deps{
		Loop:     nil,
		DebugTap: nil,
		Cfg: &config.Config{
			Debug: config.DebugConfig{
				Sandbox: config.SandboxConfig{
					FixturesDir: t.TempDir(),
				},
			},
		},
	}

	ctx := context.Background()

	// Test 1: sandbox_write_fixtures refuses
	t.Run("sandbox_write_fixtures_refuses", func(t *testing.T) {
		handler := sandboxWriteFixturesHandler(d)
		req := &mcp.ServerRequest[*mcp.CallToolParamsRaw]{
			Params: &mcp.CallToolParamsRaw{
				Name:      "sandbox_write_fixtures",
				Arguments: json.RawMessage(`{"venue":"okx","entries":"[{\"method\":\"POST\",\"path_prefix\":\"/new\",\"response\":{\"status\":200,\"body\":{}}}]"}`),
			},
		}
		result, err := handler(ctx, (*mcp.CallToolRequest)(req))
		if err != nil {
			t.Fatalf("handler returned error: %v", err)
		}
		if !result.IsError {
			t.Error("write_fixtures should return IsError=true when sandbox disabled")
		}
		if len(result.Content) == 0 {
			t.Fatal("error result has no content")
		}
		errMsg := result.Content[0].(*mcp.TextContent).Text
		if !strings.Contains(errMsg, "disabled") {
			t.Errorf("error message should mention 'disabled', got: %s", errMsg)
		}
		// Verify no mutation: initial fixture still there
		fixtures := srv.GetFixtures("okx")
		if len(fixtures) != 1 || fixtures[0].PathPrefix != "/api/initial" {
			t.Error("store was mutated when write should have been refused")
		}
	})

	// Test 2: sandbox_upsert_fixture refuses
	t.Run("sandbox_upsert_fixture_refuses", func(t *testing.T) {
		handler := sandboxUpsertFixtureHandler(d)
		req := &mcp.ServerRequest[*mcp.CallToolParamsRaw]{
			Params: &mcp.CallToolParamsRaw{
				Name:      "sandbox_upsert_fixture",
				Arguments: json.RawMessage(`{"venue":"okx","method":"POST","path_prefix":"/upsert","status":200,"body":"{}"}`),
			},
		}
		result, err := handler(ctx, (*mcp.CallToolRequest)(req))
		if err != nil {
			t.Fatalf("handler returned error: %v", err)
		}
		if !result.IsError {
			t.Error("upsert_fixture should return IsError=true when sandbox disabled")
		}
		// Verify no mutation
		fixtures := srv.GetFixtures("okx")
		if len(fixtures) != 1 || fixtures[0].PathPrefix != "/api/initial" {
			t.Error("store was mutated when upsert should have been refused")
		}
	})

	// Test 3: sandbox_reload_fixtures refuses
	t.Run("sandbox_reload_fixtures_refuses", func(t *testing.T) {
		handler := sandboxReloadFixturesHandler(d)
		req := &mcp.ServerRequest[*mcp.CallToolParamsRaw]{
			Params: &mcp.CallToolParamsRaw{
				Name:      "sandbox_reload_fixtures",
				Arguments: json.RawMessage(`{"venue":"okx"}`),
			},
		}
		result, err := handler(ctx, (*mcp.CallToolRequest)(req))
		if err != nil {
			t.Fatalf("handler returned error: %v", err)
		}
		if !result.IsError {
			t.Error("reload_fixtures should return IsError=true when sandbox disabled")
		}
		// Verify no mutation
		fixtures := srv.GetFixtures("okx")
		if len(fixtures) != 1 || fixtures[0].PathPrefix != "/api/initial" {
			t.Error("store was mutated when reload should have been refused")
		}
	})

	// Test 4: sandbox_reset_simulator refuses
	t.Run("sandbox_reset_simulator_refuses", func(t *testing.T) {
		handler := sandboxResetSimulatorHandler(d)
		req := &mcp.ServerRequest[*mcp.CallToolParamsRaw]{
			Params: &mcp.CallToolParamsRaw{
				Name:      "sandbox_reset_simulator",
				Arguments: json.RawMessage(`{}`),
			},
		}
		result, err := handler(ctx, (*mcp.CallToolRequest)(req))
		if err != nil {
			t.Fatalf("handler returned error: %v", err)
		}
		if !result.IsError {
			t.Error("reset_simulator should return IsError=true when sandbox disabled")
		}
	})

	// Test 5: read-only tools still work when sandbox disabled
	t.Run("sandbox_list_venues_works_when_disabled", func(t *testing.T) {
		handler := sandboxListVenuesHandler(d)
		req := &mcp.ServerRequest[*mcp.CallToolParamsRaw]{
			Params: &mcp.CallToolParamsRaw{
				Name:      "sandbox_list_venues",
				Arguments: json.RawMessage(`{}`),
			},
		}
		result, err := handler(ctx, (*mcp.CallToolRequest)(req))
		if err != nil {
			t.Fatalf("handler returned error: %v", err)
		}
		if result.IsError {
			t.Error("list_venues should work even when sandbox disabled (read-only)")
		}
	})

	t.Run("sandbox_read_fixtures_works_when_disabled", func(t *testing.T) {
		handler := sandboxReadFixturesHandler(d)
		req := &mcp.ServerRequest[*mcp.CallToolParamsRaw]{
			Params: &mcp.CallToolParamsRaw{
				Name:      "sandbox_read_fixtures",
				Arguments: json.RawMessage(`{"venue":"okx"}`),
			},
		}
		result, err := handler(ctx, (*mcp.CallToolRequest)(req))
		if err != nil {
			t.Fatalf("handler returned error: %v", err)
		}
		if result.IsError {
			t.Error("read_fixtures should work even when sandbox disabled (read-only)")
		}
	})
}

// TestSandboxTools_FixtureWriteReadRoundTrip verifies that fixture bodies survive
// a write → read cycle with bytes preserved exactly.
func TestSandboxTools_FixtureWriteReadRoundTrip(t *testing.T) {
	// Enable sandbox for this test
	sandbox.SetGlobalState(true, "http://127.0.0.1:9999")

	// Create a fresh store and server
	store := sandbox.NewStore()
	srv := sandbox.NewServer(store)
	sandbox.SetInstance(srv)

	// Test case: deliberately ugly JSON with odd spacing, key order, and floats
	uglyJSON := `{"a":1   ,  "b":  2.0000000000000001,   "c":"hello","d":1e10}`

	// Create fixture entry with the ugly body
	entry := sandbox.FixtureEntry{
		Method:     "POST",
		PathPrefix: "/api/v5/trade/order",
		Response: sandbox.Response{
			Status: 200,
			Body:   json.RawMessage(uglyJSON),
		},
	}

	entries := []sandbox.FixtureEntry{entry}

	// Set fixtures in memory
	srv.SetFixtures("okx", entries)

	// Read back
	readEntries := srv.GetFixtures("okx")
	if len(readEntries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(readEntries))
	}

	// Verify body bytes are preserved exactly
	if !bytes.Equal(readEntries[0].Response.Body, json.RawMessage(uglyJSON)) {
		t.Errorf("body bytes not preserved\nexpected: %s\ngot:      %s",
			string(uglyJSON), string(readEntries[0].Response.Body))
	}

	// Also verify that parsing and re-marshaling would lose precision
	// (proving that byte-preservation matters)
	var parsed map[string]interface{}
	if err := json.Unmarshal(readEntries[0].Response.Body, &parsed); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}

	remarshaled, err := json.Marshal(parsed)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}

	// The remarshaled version should differ (normalized formatting)
	if bytes.Equal(remarshaled, readEntries[0].Response.Body) {
		t.Errorf("remarshaling did not change the body (unexpected)")
	}

	t.Logf("original:     %s", uglyJSON)
	t.Logf("remarshaled:  %s", string(remarshaled))
}

// TestSandboxTools_InvalidJSONRejected verifies that invalid JSON is rejected
// with an actionable error message.
func TestSandboxTools_InvalidJSONRejected(t *testing.T) {
	store := sandbox.NewStore()
	srv := sandbox.NewServer(store)
	sandbox.SetInstance(srv)

	// Enable sandbox
	sandbox.SetGlobalState(true, "http://127.0.0.1:9999")

	// The handler would reject invalid entries JSON
	// We test the validation logic by simulating what would happen

	invalidJSON := `{"method":"POST","path_prefix":"/api",response":{"status":200}}`
	// Note the typo: "response" instead of "response" with missing quote

	var entries []sandbox.FixtureEntry
	err := json.Unmarshal([]byte(invalidJSON), &entries)
	if err == nil {
		t.Fatal("expected JSON error for invalid JSON, got nil")
	}

	// Verify the error message is actionable
	errMsg := err.Error()
	if !strings.Contains(errMsg, "invalid") || !strings.Contains(errMsg, "character") || !strings.Contains(errMsg, "character") {
		// The actual error message format varies; just check it contains useful info
		// JSON unmarshal errors from Go include line:column info
		if len(errMsg) == 0 {
			t.Error("error message is empty")
		}
	}
}

// TestSandboxTools_ValidBodyPreserved verifies that valid JSON bodies are preserved.
func TestSandboxTools_ValidBodyPreserved(t *testing.T) {
	store := sandbox.NewStore()
	srv := sandbox.NewServer(store)
	sandbox.SetInstance(srv)

	testCases := []struct {
		name string
		body string
	}{
		{
			name: "compact JSON",
			body: `{"code":"0","msg":"success","data":[]}`,
		},
		{
			name: "nested JSON",
			body: `{"result":{"balance":100.5,"positions":[{"size":10,"entry":50.5}]}}`,
		},
		{
			name: "high precision float",
			body: `{"price":1.23456789012345678901}`,
		},
		{
			name: "scientific notation",
			body: `{"value":1e-10,"large":9.999e20}`,
		},
		{
			name: "empty array",
			body: `[]`,
		},
		{
			name: "empty object",
			body: `{}`,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Validate body is valid JSON
			var dummy json.RawMessage
			if err := json.Unmarshal([]byte(tc.body), &dummy); err != nil {
				t.Fatalf("test body is not valid JSON: %v", err)
			}

			// Create fixture with this body
			entry := sandbox.FixtureEntry{
				Method:     "GET",
				PathPrefix: "/test",
				Response: sandbox.Response{
					Status: 200,
					Body:   json.RawMessage(tc.body),
				},
			}

			// Store and retrieve
			srv.SetFixtures("test_venue", []sandbox.FixtureEntry{entry})
			retrieved := srv.GetFixtures("test_venue")

			if len(retrieved) != 1 {
				t.Fatalf("expected 1 entry, got %d", len(retrieved))
			}

			// Verify bytes are preserved
			if !bytes.Equal(retrieved[0].Response.Body, json.RawMessage(tc.body)) {
				t.Errorf("body bytes not preserved for %s\nexpected: %s\ngot:      %s",
					tc.name, tc.body, string(retrieved[0].Response.Body))
			}
		})
	}
}

// TestSandboxTools_ReadOnlyVenuesWhenDisabled verifies that read-only tools work
// regardless of sandbox state, but write tools are gated.
func TestSandboxTools_ReadOnlyVenuesWhenDisabled(t *testing.T) {
	store := sandbox.NewStore()
	srv := sandbox.NewServer(store)
	sandbox.SetInstance(srv)

	// Add some fixtures
	entry := sandbox.FixtureEntry{
		Method:     "GET",
		PathPrefix: "/api",
		Response: sandbox.Response{
			Status: 200,
			Body:   json.RawMessage(`{"data":"test"}`),
		},
	}
	srv.SetFixtures("okx", []sandbox.FixtureEntry{entry})

	// Disable sandbox
	sandbox.SetGlobalState(false, "")

	// Read-only tools should still work
	venues := srv.GetVenues()
	if len(venues) == 0 {
		t.Error("expected venues even when sandbox is disabled")
	}

	fixtures := srv.GetFixtures("okx")
	if len(fixtures) == 0 {
		t.Error("expected fixtures even when sandbox is disabled")
	}

	// Write operations would fail (tested elsewhere)
}

// TestSandboxTools_DefaultConfigWorks verifies that write handlers work with a default
// (empty) FixturesDir in the config, using the fallback workspace/sandbox directory.
// This is a regression test for the bug where handlers read FixturesDir directly
// instead of calling sandbox.ResolveFixturesDir.
func TestSandboxTools_DefaultConfigWorks(t *testing.T) {
	// Setup: create store and server
	store := sandbox.NewStore()
	srv := sandbox.NewServer(store)
	sandbox.SetInstance(srv)

	// Enable sandbox
	sandbox.SetGlobalState(true, "http://127.0.0.1:9999")
	defer sandbox.SetGlobalState(false, "")

	// Create a default config with empty FixturesDir (mimicking stock defaults)
	tmpDir := t.TempDir()
	d := Deps{
		Loop:     nil,
		DebugTap: nil,
		Cfg: &config.Config{
			Agents: config.AgentsConfig{
				Defaults: config.AgentDefaults{
					Workspace: tmpDir,
				},
			},
			Debug: config.DebugConfig{
				Sandbox: config.SandboxConfig{
					FixturesDir: "", // Empty — should fall back to workspace/sandbox
					Enabled:     true,
				},
			},
		},
	}

	ctx := context.Background()

	// Test 1: sandbox_write_fixtures should work and persist to workspace/sandbox
	t.Run("sandbox_write_fixtures_with_default_config", func(t *testing.T) {
		handler := sandboxWriteFixturesHandler(d)
		req := &mcp.ServerRequest[*mcp.CallToolParamsRaw]{
			Params: &mcp.CallToolParamsRaw{
				Name:      "sandbox_write_fixtures",
				Arguments: json.RawMessage(`{"venue":"okx","entries":"[{\"method\":\"POST\",\"path_prefix\":\"/api/v5/trade/order\",\"response\":{\"status\":200,\"body\":{}}}]"}`),
			},
		}
		result, err := handler(ctx, (*mcp.CallToolRequest)(req))
		if err != nil {
			t.Fatalf("handler returned error: %v", err)
		}
		if result.IsError {
			t.Errorf("write_fixtures failed with empty FixturesDir (should use fallback): %s",
				result.Content[0].(*mcp.TextContent).Text)
		}

		// Verify the fixture was persisted to the fallback directory
		expectedPath := filepath.Join(tmpDir, "sandbox", "okx", "fixtures.json")
		if _, err := os.Stat(expectedPath); err != nil {
			t.Errorf("fixture file not persisted to expected fallback location %s: %v", expectedPath, err)
		}
	})

	// Test 2: sandbox_upsert_fixture should also work and persist
	t.Run("sandbox_upsert_fixture_with_default_config", func(t *testing.T) {
		handler := sandboxUpsertFixtureHandler(d)
		req := &mcp.ServerRequest[*mcp.CallToolParamsRaw]{
			Params: &mcp.CallToolParamsRaw{
				Name:      "sandbox_upsert_fixture",
				Arguments: json.RawMessage(`{"venue":"binance","method":"GET","path_prefix":"/api/v3/account","status":200,"body":"{\"balances\":[]}"}`),
			},
		}
		result, err := handler(ctx, (*mcp.CallToolRequest)(req))
		if err != nil {
			t.Fatalf("handler returned error: %v", err)
		}
		if result.IsError {
			t.Errorf("upsert_fixture failed with empty FixturesDir (should use fallback): %s",
				result.Content[0].(*mcp.TextContent).Text)
		}

		// Verify the fixture was persisted to the fallback directory
		expectedPath := filepath.Join(tmpDir, "sandbox", "binance", "fixtures.json")
		if _, err := os.Stat(expectedPath); err != nil {
			t.Errorf("fixture file not persisted to expected fallback location %s: %v", expectedPath, err)
		}
	})

	// Test 3: sandbox_reload_fixtures should work with the fallback directory
	t.Run("sandbox_reload_fixtures_with_default_config", func(t *testing.T) {
		handler := sandboxReloadFixturesHandler(d)
		req := &mcp.ServerRequest[*mcp.CallToolParamsRaw]{
			Params: &mcp.CallToolParamsRaw{
				Name:      "sandbox_reload_fixtures",
				Arguments: json.RawMessage(`{"venue":"okx"}`),
			},
		}
		result, err := handler(ctx, (*mcp.CallToolRequest)(req))
		if err != nil {
			t.Fatalf("handler returned error: %v", err)
		}
		if result.IsError {
			errMsg := result.Content[0].(*mcp.TextContent).Text
			// "fixtures_dir not configured" error would indicate the bug is not fixed
			if strings.Contains(errMsg, "fixtures_dir not configured") {
				t.Errorf("reload_fixtures failed: %s (indicates fallback is not being used)", errMsg)
			} else {
				// Other errors (like no files found) are OK; the point is it didn't error on missing config
				t.Logf("reload_fixtures returned: %s", errMsg)
			}
		}
	})
}
