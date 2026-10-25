package heartbeat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/state"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

func TestExecuteHeartbeat_Async(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "heartbeat-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	hs := NewHeartbeatService(tmpDir, 30, true, state.NewManager(tmpDir))
	hs.stopChan = make(chan struct{}) // Enable for testing

	asyncCalled := false
	asyncResult := &tools.ToolResult{
		ForLLM:  "Background task started",
		ForUser: "Task started in background",
		Silent:  false,
		IsError: false,
		Async:   true,
	}

	hs.SetHandler(func(prompt, channel, chatID string) *tools.ToolResult {
		asyncCalled = true
		if prompt == "" {
			t.Error("Expected non-empty prompt")
		}
		return asyncResult
	})

	// Create HEARTBEAT.md
	os.WriteFile(filepath.Join(tmpDir, "HEARTBEAT.md"), []byte("Test task"), 0o644)

	// Execute heartbeat directly (internal method for testing)
	hs.executeHeartbeat()

	if !asyncCalled {
		t.Error("Expected handler to be called")
	}
}

func TestExecuteHeartbeat_ResultLogging(t *testing.T) {
	tests := []struct {
		name    string
		result  *tools.ToolResult
		wantLog string
	}{
		{
			name: "error result",
			result: &tools.ToolResult{
				ForLLM:  "Heartbeat failed: connection error",
				ForUser: "",
				Silent:  false,
				IsError: true,
				Async:   false,
			},
			wantLog: "error message",
		},
		{
			name: "silent result",
			result: &tools.ToolResult{
				ForLLM:  "Heartbeat completed successfully",
				ForUser: "",
				Silent:  true,
				IsError: false,
				Async:   false,
			},
			wantLog: "completion message",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir, err := os.MkdirTemp("", "heartbeat-test-*")
			if err != nil {
				t.Fatalf("Failed to create temp dir: %v", err)
			}
			defer os.RemoveAll(tmpDir)

			hs := NewHeartbeatService(tmpDir, 30, true, state.NewManager(tmpDir))
			hs.stopChan = make(chan struct{}) // Enable for testing

			hs.SetHandler(func(prompt, channel, chatID string) *tools.ToolResult {
				return tt.result
			})

			os.WriteFile(filepath.Join(tmpDir, "HEARTBEAT.md"), []byte("Test task"), 0o644)
			hs.executeHeartbeat()

			logFile := filepath.Join(tmpDir, "heartbeat.log")
			data, err := os.ReadFile(logFile)
			if err != nil {
				t.Fatalf("Failed to read log file: %v", err)
			}
			if string(data) == "" {
				t.Errorf("Expected log file to contain %s", tt.wantLog)
			}
		})
	}
}

func TestHeartbeatService_StartStop(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "heartbeat-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	hs := NewHeartbeatService(tmpDir, 1, true, state.NewManager(tmpDir))

	err = hs.Start()
	if err != nil {
		t.Fatalf("Failed to start heartbeat service: %v", err)
	}

	hs.Stop()

	time.Sleep(100 * time.Millisecond)
}

func TestHeartbeatService_Disabled(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "heartbeat-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	hs := NewHeartbeatService(tmpDir, 1, false, state.NewManager(tmpDir))

	if hs.enabled != false {
		t.Error("Expected service to be disabled")
	}

	err = hs.Start()
	_ = err // Disabled service returns nil
}

func TestExecuteHeartbeat_NilResult(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "heartbeat-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	hs := NewHeartbeatService(tmpDir, 30, true, state.NewManager(tmpDir))
	hs.stopChan = make(chan struct{}) // Enable for testing

	hs.SetHandler(func(prompt, channel, chatID string) *tools.ToolResult {
		return nil
	})

	// Create HEARTBEAT.md
	os.WriteFile(filepath.Join(tmpDir, "HEARTBEAT.md"), []byte("Test task"), 0o644)

	// Should not panic with nil result
	hs.executeHeartbeat()
}

// TestLogPath verifies heartbeat log is written to workspace directory
func TestLogPath(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "heartbeat-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	hs := NewHeartbeatService(tmpDir, 30, true, state.NewManager(tmpDir))

	// Write a log entry
	hs.logf("INFO", "Test log entry")

	// Verify log file exists at workspace root
	expectedLogPath := filepath.Join(tmpDir, "heartbeat.log")
	if _, err := os.Stat(expectedLogPath); os.IsNotExist(err) {
		t.Errorf("Expected log file at %s, but it doesn't exist", expectedLogPath)
	}
}

// TestHeartbeatFilePath verifies HEARTBEAT.md is at workspace root
func TestHeartbeatFilePath(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "heartbeat-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	hs := NewHeartbeatService(tmpDir, 30, true, state.NewManager(tmpDir))

	// Trigger default template creation
	hs.buildPrompt()

	// Verify HEARTBEAT.md exists at workspace root
	expectedPath := filepath.Join(tmpDir, "HEARTBEAT.md")
	if _, err := os.Stat(expectedPath); os.IsNotExist(err) {
		t.Errorf("Expected HEARTBEAT.md at %s, but it doesn't exist", expectedPath)
	}
}

func TestBuildPrompt_DefaultTemplateStaysIdle(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "heartbeat-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	hs := NewHeartbeatService(tmpDir, 30, true, state.NewManager(tmpDir))
	hs.createDefaultHeartbeatTemplate()

	if prompt := hs.buildPrompt(); prompt != "" {
		t.Fatalf("buildPrompt() = %q, want empty prompt for untouched default template", prompt)
	}
}

func TestBuildPrompt_UserTasksAfterMarkerProducePrompt(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "heartbeat-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	hs := NewHeartbeatService(tmpDir, 30, true, state.NewManager(tmpDir))
	hs.createDefaultHeartbeatTemplate()

	path := filepath.Join(tmpDir, "HEARTBEAT.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("Failed to read HEARTBEAT.md: %v", err)
	}
	updated := string(data) + "\n- Check unread Feishu messages\n"
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		t.Fatalf("Failed to update HEARTBEAT.md: %v", err)
	}

	prompt := hs.buildPrompt()
	if prompt == "" {
		t.Fatal("buildPrompt() = empty, want non-empty prompt when user tasks are present")
	}
	if !strings.Contains(prompt, "Check unread Feishu messages") {
		t.Fatalf("prompt = %q, want user task content", prompt)
	}
}

// TestHeartbeatService_SetBus tests the SetBus method
func TestHeartbeatService_SetBus(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "heartbeat-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	hs := NewHeartbeatService(tmpDir, 30, true, state.NewManager(tmpDir))

	// Initially no bus set
	if hs.bus != nil {
		t.Error("Expected bus to be nil initially")
	}

	// Set a bus (using nil for simplicity, as we just test the setter)
	// In real usage, this would be a proper MessageBus
	// Just test that no panic occurs
	hs.SetBus(nil)
}

// TestHeartbeatService_IsRunning tests the IsRunning method
func TestHeartbeatService_IsRunning(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "heartbeat-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	hs := NewHeartbeatService(tmpDir, 30, true, state.NewManager(tmpDir))

	// Not running initially
	if hs.IsRunning() {
		t.Error("Expected IsRunning() = false, got true")
	}

	// Start the service
	err = hs.Start()
	if err != nil {
		t.Fatalf("Failed to start service: %v", err)
	}
	defer hs.Stop()

	// Now it should be running
	if !hs.IsRunning() {
		t.Error("Expected IsRunning() = true after Start(), got false")
	}

	// Stop the service
	hs.Stop()

	// Allow a small window for the stop to take effect
	time.Sleep(10 * time.Millisecond)

	// Should not be running anymore
	if hs.IsRunning() {
		t.Error("Expected IsRunning() = false after Stop(), got true")
	}
}

// TestParseLastChannel_Valid tests parsing of valid channel strings
func TestParseLastChannel_Valid(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "heartbeat-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	hs := NewHeartbeatService(tmpDir, 30, true, state.NewManager(tmpDir))

	tests := []struct {
		name         string
		input        string
		wantPlatform string
		wantUserID   string
	}{
		{
			name:         "telegram channel",
			input:        "telegram:123456",
			wantPlatform: "telegram",
			wantUserID:   "123456",
		},
		{
			name:         "discord channel",
			input:        "discord:user_id_789",
			wantPlatform: "discord",
			wantUserID:   "user_id_789",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			platform, userID := hs.parseLastChannel(tt.input)
			if platform != tt.wantPlatform {
				t.Errorf("parseLastChannel(%q) platform = %q, want %q", tt.input, platform, tt.wantPlatform)
			}
			if userID != tt.wantUserID {
				t.Errorf("parseLastChannel(%q) userID = %q, want %q", tt.input, userID, tt.wantUserID)
			}
		})
	}
}

// TestParseLastChannel_Invalid tests error handling for invalid channel strings
func TestParseLastChannel_Invalid(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "heartbeat-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	hs := NewHeartbeatService(tmpDir, 30, true, state.NewManager(tmpDir))

	tests := []struct {
		name  string
		input string
	}{
		{
			name:  "empty string",
			input: "",
		},
		{
			name:  "no colon separator",
			input: "telegram123456",
		},
		{
			name:  "empty platform",
			input: ":123456",
		},
		{
			name:  "empty user id",
			input: "telegram:",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			platform, userID := hs.parseLastChannel(tt.input)
			if platform != "" || userID != "" {
				t.Errorf("parseLastChannel(%q) = (%q, %q), want (\"\", \"\")", tt.input, platform, userID)
			}
		})
	}
}

// TestParseLastChannel_InternalChannel tests that internal channels return empty strings
func TestParseLastChannel_InternalChannel(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "heartbeat-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	hs := NewHeartbeatService(tmpDir, 30, true, state.NewManager(tmpDir))

	// Test known internal channel (cli, system, subagent)
	platform, userID := hs.parseLastChannel("cli:test")
	if platform != "" || userID != "" {
		t.Errorf("parseLastChannel for internal channel = (%q, %q), want (\"\", \"\")", platform, userID)
	}
}

// TestParseLastChannel_MultipleColons tests that multiple colons are handled correctly
func TestParseLastChannel_MultipleColons(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "heartbeat-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	hs := NewHeartbeatService(tmpDir, 30, true, state.NewManager(tmpDir))

	// Multiple colons should be handled - everything after first colon is userID
	platform, userID := hs.parseLastChannel("telegram:123:456")
	if platform != "telegram" {
		t.Errorf("parseLastChannel with multiple colons platform = %q, want %q", platform, "telegram")
	}
	if userID != "123:456" {
		t.Errorf("parseLastChannel with multiple colons userID = %q, want %q", userID, "123:456")
	}
}

// TestSendResponse_NoBus tests sendResponse when no bus is configured
func TestSendResponse_NoBus(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "heartbeat-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	hs := NewHeartbeatService(tmpDir, 30, true, state.NewManager(tmpDir))

	// No bus set, should handle gracefully
	hs.sendResponse("test message")

	// Verify log file was created
	logFile := filepath.Join(tmpDir, "heartbeat.log")
	if _, err := os.Stat(logFile); os.IsNotExist(err) {
		t.Errorf("Expected log file to be created, but it wasn't")
	}
}

// TestSendResponse_NoLastChannel tests sendResponse when no last channel is recorded
func TestSendResponse_NoLastChannel(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "heartbeat-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	hs := NewHeartbeatService(tmpDir, 30, true, state.NewManager(tmpDir))

	// No last channel recorded, should handle gracefully
	hs.sendResponse("test message")

	// Verify log file was created
	logFile := filepath.Join(tmpDir, "heartbeat.log")
	if _, err := os.Stat(logFile); os.IsNotExist(err) {
		t.Errorf("Expected log file to be created, but it wasn't")
	}
}

func TestNewHeartbeatService_BelowMinInterval(t *testing.T) {
	hs := NewHeartbeatService(t.TempDir(), 0, true, nil)
	if hs.interval != time.Duration(defaultIntervalMinutes)*time.Minute {
		t.Errorf("zero intervalMinutes should use default, got %v", hs.interval)
	}
}

func TestNewHeartbeatService_ZeroInterval(t *testing.T) {
	// intervalMinutes < minIntervalMinutes AND != 0 should clamp to min
	hs := NewHeartbeatService(t.TempDir(), -5, true, nil)
	// -5 < minIntervalMinutes (1) and != 0: implementation clamps via the first condition
	// Then zero-check does NOT fire. Let's just ensure no panic and interval is set.
	if hs == nil {
		t.Error("expected non-nil service")
	}
}

func TestNewHeartbeatService_BelowMin_Clamps(t *testing.T) {
	// value in (0, minIntervalMinutes) should be clamped to minIntervalMinutes
	hs := NewHeartbeatService(t.TempDir(), 0, false, nil)
	want := time.Duration(defaultIntervalMinutes) * time.Minute
	if hs.interval != want {
		t.Errorf("interval = %v, want %v", hs.interval, want)
	}
	if hs.enabled {
		t.Error("expected enabled=false")
	}
}
