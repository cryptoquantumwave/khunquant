package sweep

import (
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/perception/perceptiontest"
)

// TestSweep_ImportBoundary keeps the sweep unable to place an order, call a tool or
// reach the owner: nothing it imports, at any depth, is the agent, the tool
// registry, a provider (which includes the broker), the notify service, the
// channels, attention, the config, the snapshot store, the perception sources or the
// engine. What it needs of them it is given as small interfaces.
func TestSweep_ImportBoundary(t *testing.T) {
	perceptiontest.AssertImports(t, perceptiontest.Boundary{
		Root:    "pkg/proactive/sweep",
		Allowed: []string{"pkg/logger", "pkg/perception", "pkg/proactive", "pkg/worldmodel"},
		Forbidden: []string{
			"pkg/agent", "pkg/tools", "pkg/providers", "pkg/notify", "pkg/channels", "pkg/attention",
			"pkg/config", "pkg/snapshot", "pkg/perception/sources", "pkg/proactive/engine",
		},
	})
}
