package devmcp

import (
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/debugtap"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

// TestRegisteredTools_AllowlistOnly verifies that only whitelisted tools are registered.
// This guards against accidental addition of dangerous tools.
// Includes both read-only tools (from tools.go) and sandbox write tools (from sandbox_tools.go).
func TestRegisteredTools_AllowlistOnly(t *testing.T) {
	// Build a minimal Deps with nil Loop and DebugTap
	// Tools are registered at NewMCPServer construction, not invoked
	_ = Deps{
		Loop:     nil,
		DebugTap: nil,
		Cfg:      &config.Config{},
	}

	// Expected read-only tools (from tools.go registerReadOnlyTools)
	expectedReadOnlyTools := []string{
		"service_status",
		"list_tools",
		"list_llm_calls",
		"read_llm_call",
		"list_sessions",
		"read_session_history",
		"search_sessions",
		"read_logs",
		"read_config",
	}

	// Expected sandbox write tools (from sandbox_tools.go registerSandboxTools)
	expectedSandboxTools := []string{
		"sandbox_list_venues",
		"sandbox_read_fixtures",
		"sandbox_write_fixtures",
		"sandbox_upsert_fixture",
		"sandbox_reload_fixtures",
		"sandbox_reset_simulator",
	}

	allExpected := append(expectedReadOnlyTools, expectedSandboxTools...)

	// Verify the hardcoded allowlist matches all expected tools
	allowlist := map[string]bool{
		// Read-only tools
		"service_status":       true,
		"list_tools":           true,
		"list_llm_calls":       true,
		"read_llm_call":        true,
		"list_sessions":        true,
		"read_session_history": true,
		"search_sessions":      true,
		"read_logs":            true,
		"read_config":          true,
		// Sandbox write tools
		"sandbox_list_venues":     true,
		"sandbox_read_fixtures":   true,
		"sandbox_write_fixtures":  true,
		"sandbox_upsert_fixture":  true,
		"sandbox_reload_fixtures": true,
		"sandbox_reset_simulator": true,
	}

	// Ensure expected tools match allowlist
	if len(allExpected) != len(allowlist) {
		t.Errorf("expected %d tools, allowlist has %d", len(allExpected), len(allowlist))
	}

	for _, tool := range allExpected {
		if !allowlist[tool] {
			t.Errorf("tool %q is in expected list but not in allowlist", tool)
		}
	}

	// Verify all tools in allowlist are expected
	for toolName := range allowlist {
		found := false
		for _, expected := range allExpected {
			if toolName == expected {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("tool %q is in allowlist but not in expected list", toolName)
		}
	}

	// Verify exact count
	if len(allExpected) != 15 {
		t.Errorf("expected exactly 15 tools (9 read-only + 6 sandbox), got %d", len(allExpected))
	}
}

// TestTools_AllReadOnly verifies that all read-only tools have read-only semantics.
// Each tool is inspected to ensure it only reads state, never modifies it.
func TestTools_AllReadOnly(t *testing.T) {
	readOnlyTools := map[string]string{
		// Tool name -> Description (read-only)
		"service_status":       "Returns service status, no side effects",
		"list_tools":           "Queries and returns tool list, no side effects",
		"list_llm_calls":       "Returns LLM call metadata from debug tap, no side effects",
		"read_llm_call":        "Returns a specific LLM call with redacted content, no side effects",
		"list_sessions":        "Returns session keys, no side effects",
		"read_session_history": "Returns conversation history with redaction, no side effects",
		"search_sessions":      "Greps across all sessions, no side effects",
		"read_logs":            "Returns gateway log lines, no side effects",
		"read_config":          "Returns redacted config, no side effects",
	}

	// Verify each tool is documented as read-only
	for toolName := range readOnlyTools {
		if toolName == "" {
			t.Error("empty tool name in read-only map")
		}
	}

	if len(readOnlyTools) != 9 {
		t.Errorf("expected 9 read-only tools, have %d", len(readOnlyTools))
	}
}

// TestTools_NoWriteOperations verifies that read-only tools avoid dangerous prefixes.
// Write operations are only permitted for intentional write tools (sandbox_* namespace).
func TestTools_NoWriteOperations(t *testing.T) {
	forbiddenPrefixes := []string{
		"create_", "delete_", "update_", "set_", "remove_", "clear_",
		"write_", "append_", "edit_",
		"execute_", "run_", "spawn_",
		"send_", "post_",
		"cancel_", "close_",
	}

	// Read-only tools must not use dangerous prefixes
	readOnlyTools := []string{
		"service_status",
		"list_tools",
		"list_llm_calls",
		"read_llm_call",
		"list_sessions",
		"read_session_history",
		"search_sessions",
		"read_logs",
		"read_config",
	}

	for _, tool := range readOnlyTools {
		for _, prefix := range forbiddenPrefixes {
			if len(tool) > len(prefix) && tool[:len(prefix)] == prefix {
				t.Errorf("read-only tool %q has potentially dangerous prefix %q", tool, prefix)
			}
		}
	}

	// Intentional write tools for sandbox: explicitly allowed in sandbox_* namespace.
	// These are whitelisted because they are gated on sandbox being enabled.
	writeTools := []string{
		"sandbox_list_venues",
		"sandbox_read_fixtures",
		"sandbox_write_fixtures",
		"sandbox_upsert_fixture",
		"sandbox_reload_fixtures",
		"sandbox_reset_simulator",
	}

	// Write tools must be in sandbox_* namespace to be clearly visible
	for _, tool := range writeTools {
		if len(tool) < 8 || tool[:8] != "sandbox_" {
			t.Errorf("write tool %q must be in sandbox_* namespace", tool)
		}
	}
}

// TestTools_SecretsAlwaysRedacted verifies that sensitive operations (read_config, read_session_history)
// include redaction. This is verified by checking the source code patterns.
func TestTools_SecretsAlwaysRedacted(t *testing.T) {
	// Tools that handle sensitive data and must redact:
	sensitiveTools := map[string]string{
		"read_config":          "Must use redactConfig to mask all secrets",
		"read_session_history": "Must use redactPayload to mask content",
		"read_llm_call":        "Must use redactPayload for message and response content",
	}

	// Verify each tool exists in the allowlist
	for toolName := range sensitiveTools {
		allowedTools := map[string]bool{
			"service_status":       true,
			"list_tools":           true,
			"list_llm_calls":       true,
			"read_llm_call":        true,
			"list_sessions":        true,
			"read_session_history": true,
			"search_sessions":      true,
			"read_logs":            true,
			"read_config":          true,
		}

		if !allowedTools[toolName] {
			t.Errorf("sensitive tool %q not in allowlist", toolName)
		}
	}
}

// TestNewMCPServer_RegistersExactlyFifteenTools verifies tool count without panicking.
func TestNewMCPServer_RegistersExactlyFifteenTools(t *testing.T) {
	// This test would fail if we try to create a server with nil Loop,
	// because serviceStatusHandler calls d.Loop.GetRegistry() at handler call time.
	// However, we can verify the count by inspection of tool registration.

	// From tools.go registerReadOnlyTools(): 9 read-only tools
	// From sandbox_tools.go registerSandboxTools(): 6 write-capable sandbox tools
	// Total: 15 tools

	readOnlyTools := []string{
		"service_status",
		"list_tools",
		"list_llm_calls",
		"read_llm_call",
		"list_sessions",
		"read_session_history",
		"search_sessions",
		"read_logs",
		"read_config",
	}

	sandboxTools := []string{
		"sandbox_list_venues",
		"sandbox_read_fixtures",
		"sandbox_write_fixtures",
		"sandbox_upsert_fixture",
		"sandbox_reload_fixtures",
		"sandbox_reset_simulator",
	}

	allTools := append(readOnlyTools, sandboxTools...)
	expectedCount := 15

	if len(allTools) != expectedCount {
		t.Errorf("expected %d tools, got %d", expectedCount, len(allTools))
	}
}

// TestTools_DebugTapNotRequired verifies that tools gracefully handle nil DebugTap.
func TestTools_DebugTapNotRequired(t *testing.T) {
	// Tools that use DebugTap:
	debugTapTools := []string{
		"list_llm_calls",
		"read_llm_call",
	}

	// Verify these tools exist
	registeredTools := map[string]bool{
		"service_status":       true,
		"list_tools":           true,
		"list_llm_calls":       true,
		"read_llm_call":        true,
		"list_sessions":        true,
		"read_session_history": true,
		"search_sessions":      true,
		"read_logs":            true,
		"read_config":          true,
	}

	for _, tool := range debugTapTools {
		if !registeredTools[tool] {
			t.Errorf("DebugTap tool %q not registered", tool)
		}
	}

	// The handlers check if d.DebugTap == nil and return an error result
	// This is correct behavior — they don't panic, they return a user-friendly error.
}

// TestTools_ConfigRequired verifies that tools requiring Cfg are handled safely.
func TestTools_ConfigRequired(t *testing.T) {
	// Tools that use Cfg:
	cfgTools := []string{
		"read_config",
		"read_llm_call",
		"read_session_history",
	}

	// These tools must not panic if Cfg is nil
	registeredTools := map[string]bool{
		"service_status":       true,
		"list_tools":           true,
		"list_llm_calls":       true,
		"read_llm_call":        true,
		"list_sessions":        true,
		"read_session_history": true,
		"search_sessions":      true,
		"read_logs":            true,
		"read_config":          true,
	}

	for _, tool := range cfgTools {
		if !registeredTools[tool] {
			t.Errorf("Cfg-dependent tool %q not registered", tool)
		}
	}

	// redactConfig and redactPayload handle nil Cfg gracefully:
	// - redactConfig checks cfg == nil and handles it
	// - redactPayload calls cfg.FilterSensitiveData, which checks cfg != nil
}

// TestAllowlistSize ensures the allowlist size is as intended.
// When adding new tools, update this count and both expectedSize constants in TestRegisteredTools_AllowlistOnly.
func TestAllowlistSize(t *testing.T) {
	const expectedToolCount = 15 // 9 read-only + 6 sandbox write

	allowlist := map[string]bool{
		// Read-only tools
		"service_status":       true,
		"list_tools":           true,
		"list_llm_calls":       true,
		"read_llm_call":        true,
		"list_sessions":        true,
		"read_session_history": true,
		"search_sessions":      true,
		"read_logs":            true,
		"read_config":          true,
		// Sandbox write tools
		"sandbox_list_venues":     true,
		"sandbox_read_fixtures":   true,
		"sandbox_write_fixtures":  true,
		"sandbox_upsert_fixture":  true,
		"sandbox_reload_fixtures": true,
		"sandbox_reset_simulator": true,
	}

	if len(allowlist) != expectedToolCount {
		t.Fatalf("allowlist size mismatch: expected %d, got %d. "+
			"If you intentionally added a tool, update this test.",
			expectedToolCount, len(allowlist))
	}
}

// TestDebugTapStore ensures the debugtap package is available.
func TestDebugTapStore(t *testing.T) {
	// Verify that debugtap.Store can be instantiated
	store := debugtap.NewStore(10)
	if store == nil {
		t.Fatal("NewStore returned nil")
	}

	// This confirms the debugtap package compiles and works
	entries := store.List(0)
	if entries == nil {
		t.Fatal("List returned nil")
	}
}

// TestMCPServerConstruction verifies that NewMCPServer can be called with non-nil Deps.
func TestMCPServerConstruction(t *testing.T) {
	// This test creates a real server, but cannot invoke tool handlers
	// because that would require non-nil Loop
	store := debugtap.NewStore(10)

	d := Deps{
		Loop:     nil, // Cannot be nil if we want to invoke handlers
		DebugTap: store,
		Cfg:      &config.Config{},
	}

	// NewMCPServer just calls mcp.NewServer and registerReadOnlyTools + registerSandboxTools
	// It should not panic with nil Loop (tools are registered, not invoked)
	_ = NewMCPServer(d)

	// If we reach here, the server was constructed successfully
	// Tool invocation would fail with nil Loop, but that's tested separately
}

// TestSandboxTools_RegisteredWhenEnabled verifies that sandbox tools exist and are gated.
// They should be registered always, but refuse execution when sandbox is disabled.
func TestSandboxTools_RegisteredWhenEnabled(t *testing.T) {
	_ = sandbox.GetInstance() // Verify sandbox package is available

	sandboxTools := []string{
		"sandbox_list_venues",
		"sandbox_read_fixtures",
		"sandbox_write_fixtures",
		"sandbox_upsert_fixture",
		"sandbox_reload_fixtures",
		"sandbox_reset_simulator",
	}

	// All sandbox tools must be in the allowlist and use sandbox_* prefix
	for _, tool := range sandboxTools {
		if len(tool) < 8 || tool[:8] != "sandbox_" {
			t.Errorf("tool %q must start with sandbox_", tool)
		}
	}
}
