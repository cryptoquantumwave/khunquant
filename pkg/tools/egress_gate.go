package tools

import (
	"context"

	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// egressTools are the tools that send a model-chosen string to a third party: the
// URL of web_fetch, the query of web_search. A tool belongs here when what the model
// writes in its arguments leaves the machine (an MCP tool that posts, a future
// webhook or mail tool). It does not belong here for merely reading outside text:
// that is what trustedDataTools decides.
var egressTools = map[string]bool{NameWebFetch: true, NameWebSearch: true}

// checkEgress is the rule "no egress after outside text": once an autonomous turn
// has read outside text, an egress tool is refused for the rest of that turn.
//
// "Read outside text" is the turn's ledger (proactive.Taint.BeginBatch), not its
// trust floor. The tool loop lowers the floor for a whole batch before any call of
// it runs, so the first web_fetch of a turn would already find the floor lowered by
// itself. The ledger keeps the state from before the batch, so a call is refused
// only when an EARLIER batch read outside text. The first outside read of a turn
// (the page a heartbeat checklist names) is allowed; following a link out of it is not.
//
// Within one batch the calls run at the same time, so none has read what another
// returns, and the batch hands out at most one egress call (ClaimEgress): a batch
// [web_fetch A, web_fetch B] sends A or B, never both, whatever the scheduling. A
// batch [read_file, web_fetch] allows the fetch, because the model wrote its
// arguments before it saw the file; the next batch is refused.
//
// Only autonomous turns (and the sub-turns they spawn, which share the parent's
// taint) are bound. Every autonomous turn gets a taint where it starts (runAgentLoop,
// RunProactiveTurn, subturn), so an autonomous context with none lost it, and egress
// is refused: fail closed.
func checkEgress(ctx context.Context, name string) *ToolResult {
	if !egressTools[name] || !IsAutonomousOrigin(ToolOrigin(ctx)) {
		return nil
	}
	t := proactive.TaintFrom(ctx)
	if t != nil && t.ClaimEgress() {
		return nil
	}
	return policyRefusal(name, name+" is not available: this turn has already read outside text (a web page, "+
		"a file, a skill, news or a tool result from outside), so it may not send anything out. Finish "+
		"the turn with what you have.")
}
