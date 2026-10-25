package worldmodel

import (
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/perception/perceptiontest"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex/sqlitextest"
)

// TestWorldModel_ImportBoundary pins what the model may import. It reads the
// exchange's data only through events, so nothing it reaches, at any depth, is the
// agent, the tool registry, a provider (the broker included), the notify service,
// the channels, the attention governor, the config or the snapshot store - which
// also means it does not import the perception sources, whose payloads it decodes
// through mirror types instead (the gateway has a test that keeps the mirrors true).
func TestWorldModel_ImportBoundary(t *testing.T) {
	perceptiontest.AssertImports(t, perceptiontest.Boundary{
		Root: "pkg/worldmodel",
		Allowed: []string{
			"pkg/events", "pkg/logger", "pkg/perception", "pkg/perception/orderfeed", "pkg/proactive", "pkg/sqlitex",
		},
		Forbidden: []string{
			"pkg/agent", "pkg/tools", "pkg/providers", "pkg/notify", "pkg/channels", "pkg/attention",
			"pkg/config", "pkg/snapshot", "pkg/perception/sources", "pkg/proactive/engine",
		},
	})
}

// TestWorldModel_NoCrossPackageCallsInTransaction enforces the transaction rule:
// while a transaction or an open Rows is held, nothing here calls another pkg
// package.
func TestWorldModel_NoCrossPackageCallsInTransaction(t *testing.T) {
	sqlitextest.AssertNoCrossPackageCallsInTx(t, "pkg/worldmodel")
}
