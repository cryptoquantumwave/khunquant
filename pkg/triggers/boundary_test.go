package triggers

import (
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/perception/perceptiontest"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex/sqlitextest"
)

// TestTriggers_ImportBoundary pins what the package may import. A trigger reads the
// world model and returns candidates, so nothing it reaches, at any depth, is the
// agent, the tool registry, a provider (the broker included, so no order can be
// placed or changed), the notify service, the channels, the attention governor, the
// config, the perception sources or the pipeline engine.
func TestTriggers_ImportBoundary(t *testing.T) {
	perceptiontest.AssertImports(t, perceptiontest.Boundary{
		Root: "pkg/triggers",
		Allowed: []string{
			"pkg/events", "pkg/logger", "pkg/perception", "pkg/proactive", "pkg/sqlitex", "pkg/ta", "pkg/worldmodel",
		},
		Forbidden: []string{
			"pkg/agent", "pkg/tools", "pkg/providers", "pkg/notify", "pkg/channels", "pkg/attention",
			"pkg/config", "pkg/snapshot", "pkg/perception/sources", "pkg/proactive/engine",
			// The DCA plan store and its gate reach the broker; the dca_due trigger gets
			// its gate as an injected function and its plan's facts from the event.
			"pkg/dca", "pkg/cron", "pkg/exchanges",
		},
	})
}

// TestTriggers_NoCrossPackageCallsInTransaction enforces the transaction rule: while
// a transaction or an open Rows is held, nothing here calls another pkg package.
func TestTriggers_NoCrossPackageCallsInTransaction(t *testing.T) {
	sqlitextest.AssertNoCrossPackageCallsInTx(t, "pkg/triggers")
}
