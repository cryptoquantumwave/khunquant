package tools

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/logger"
)

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// At Info - the default level, and what the dashboard's log view shows - a
// tool call logs its argument names and a short redacted preview, not the
// arguments themselves.
func TestToolArgsAreNotLoggedInFullAtInfo(t *testing.T) {
	const secret = "sk-arglog-secret-77d1e0"
	logger.RegisterSecret(secret)
	tail := strings.Repeat("z", 400) + "END-OF-LONG-ARG"

	prevLevel := logger.GetLevel()
	logger.SetLevel(logger.INFO)
	t.Cleanup(func() { logger.SetLevel(prevLevel) })

	var buf lockedBuffer
	logger.SetAdditionalWriter(&buf)
	t.Cleanup(func() { logger.SetAdditionalWriter(nil) })

	reg := NewToolRegistry()
	reg.Register(newMockTool("arglog_tool", "test"))
	reg.ExecuteWithContext(context.Background(), "arglog_tool",
		map[string]any{"key": secret, "body": tail}, "cli", "direct", nil)

	out := buf.String()
	if !strings.Contains(out, "arg_keys") || !strings.Contains(out, "body") {
		t.Fatalf("argument names were not logged, so the checks below prove nothing:\n%s", out)
	}
	if strings.Contains(out, secret) {
		t.Errorf("a registered secret reached the log:\n%s", out)
	}
	if strings.Contains(out, "END-OF-LONG-ARG") {
		t.Errorf("a long argument was logged in full at Info:\n%s", out)
	}
}
