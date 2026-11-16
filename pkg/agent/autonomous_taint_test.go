package agent

import (
	"context"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

// The taint runAgentLoop attaches to a heartbeat or cron turn must start at untrusted: the
// policy engine reads the taint's floor (tools.turnTrust), and before the taint existed it
// read untrusted for these turns. Starting higher would loosen every verdict.
func TestAutonomousRootTaintStartsUntrusted(t *testing.T) {
	for _, origin := range []string{tools.OriginHeartbeat, tools.OriginCron} {
		ctx := withAutonomousTaint(tools.WithToolOrigin(context.Background(), origin))
		taint := proactive.TaintFrom(ctx)
		if taint == nil {
			t.Fatalf("%s: no taint attached", origin)
		}
		if got := taint.Trust(); got != proactive.TrustUntrusted {
			t.Errorf("%s: floor before any tool call = %v, want untrusted", origin, got)
		}
	}
	// A person's turn gets none, and an existing taint is kept.
	if proactive.TaintFrom(withAutonomousTaint(tools.WithToolOrigin(context.Background(), tools.OriginInternal))) != nil {
		t.Error("a non-autonomous turn got a taint")
	}
	own := proactive.NewTaint(proactive.TrustExchange)
	ctx := proactive.WithTaint(tools.WithToolOrigin(context.Background(), tools.OriginProactive), own)
	if proactive.TaintFrom(withAutonomousTaint(ctx)) != own {
		t.Error("an existing taint was replaced")
	}
}
