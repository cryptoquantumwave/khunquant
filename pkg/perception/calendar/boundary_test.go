package calendar_test

import (
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/perception/perceptiontest"
)

// The calendar decides when the agent evaluates a market. It reads files and the
// clock and nothing else: it may not reach an exchange, a tool or the owner.
func TestCalendar_ImportBoundary(t *testing.T) {
	perceptiontest.AssertImports(t, perceptiontest.Boundary{
		Root:    "pkg/perception/calendar",
		Allowed: []string{"pkg/logger"},
		Forbidden: []string{
			"pkg/agent", "pkg/tools", "pkg/providers", "pkg/notify", "pkg/channels", "pkg/attention",
		},
	})
}
