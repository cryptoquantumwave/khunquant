package perception_test

import (
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/perception/perceptiontest"
)

// forbidden are the packages that could place an order, call a tool, talk to an
// exchange or reach the owner. Nothing that perception imports, at any depth, may
// be one of them.
var forbidden = []string{
	"pkg/agent", "pkg/tools", "pkg/providers", "pkg/notify", "pkg/channels", "pkg/attention",
}

func TestPerception_ImportBoundary(t *testing.T) {
	perceptiontest.AssertImports(t, perceptiontest.Boundary{
		Root:      "pkg/perception",
		Allowed:   []string{"pkg/events", "pkg/logger"},
		Forbidden: forbidden,
	})
}

func TestPerceptionSources_ImportBoundary(t *testing.T) {
	perceptiontest.AssertImports(t, perceptiontest.Boundary{
		Root:      "pkg/perception/sources",
		Allowed:   []string{"pkg/perception", "pkg/perception/calendar", "pkg/events", "pkg/proactive", "pkg/logger", "pkg/devices/events"},
		Forbidden: forbidden,
	})
}
