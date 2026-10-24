package logger

import (
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// lockedBuf is a writer that is itself safe for concurrent use, so that anything
// the race detector reports in this file is the logger's own state and not the
// test's sink.
type lockedBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// The gateway swaps the additional writer (dev-MCP on or off), the level and the
// file sink while every goroutine in the process logs. Each of those setters used
// to write a package variable that logMessage read with no lock, which the race
// detector reports as a data race.
func TestLogging_ConcurrentWithReconfiguration(t *testing.T) {
	initial := GetLevel()
	t.Cleanup(func() {
		SetLevel(initial)
		SetAdditionalWriter(nil)
		DisableFileLogging()
	})
	SetLevel(WARN)

	stop := make(chan struct{})
	var loggers sync.WaitGroup
	for range 4 {
		loggers.Add(1)
		go func() {
			defer loggers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				WarnC("race", "line from a concurrent goroutine")
			}
		}()
	}

	var buf lockedBuf
	logPath := filepath.Join(t.TempDir(), "race.log")
	for i := range 100 {
		SetAdditionalWriter(&buf)
		SetAdditionalWriter(nil)
		SetLevel(WARN)
		_ = GetLevel()
		if i%10 == 0 {
			if err := EnableFileLogging(logPath); err != nil {
				t.Error(err)
			}
			DisableFileLogging()
		}
	}
	close(stop)
	loggers.Wait()
}

// Once SetAdditionalWriter has returned, a log call that starts afterwards must
// reach the new writer and not the old one.
func TestSetAdditionalWriter_AppliesToLaterCalls(t *testing.T) {
	initial := GetLevel()
	t.Cleanup(func() {
		SetLevel(initial)
		SetAdditionalWriter(nil)
	})
	SetLevel(WARN)

	var first, second lockedBuf
	SetAdditionalWriter(&first)
	WarnC("test", "to the first writer")
	SetAdditionalWriter(&second)
	WarnC("test", "to the second writer")
	SetAdditionalWriter(nil)
	WarnC("test", "to neither writer")

	if !strings.Contains(first.String(), "to the first writer") || strings.Contains(first.String(), "second writer") {
		t.Errorf("first writer = %q", first.String())
	}
	if !strings.Contains(second.String(), "to the second writer") || strings.Contains(second.String(), "neither") {
		t.Errorf("second writer = %q", second.String())
	}
}
