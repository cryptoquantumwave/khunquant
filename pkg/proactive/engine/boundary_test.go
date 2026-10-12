package engine

import (
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/perception/perceptiontest"
)

// TestEngine_ImportBoundary keeps the pipeline unable to place an order or call a
// tool: nothing it imports, at any depth, is the agent, the tool registry, a
// provider (which includes the broker), the notify service or the channels.
func TestEngine_ImportBoundary(t *testing.T) {
	perceptiontest.AssertImports(t, perceptiontest.Boundary{
		Root:    "pkg/proactive/engine",
		Allowed: []string{"pkg/attention", "pkg/events", "pkg/logger", "pkg/perception", "pkg/proactive"},
		Forbidden: []string{
			"pkg/agent", "pkg/tools", "pkg/providers", "pkg/notify", "pkg/channels",
		},
	})
}
