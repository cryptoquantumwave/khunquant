package gateway

import (
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// A proactive turn's decisions are the model's. The heartbeat and the sweep both turn
// them into candidates here, and the candidate they make never carries the mark that
// lets an alert break quiet hours, whatever the model wrote.
func TestHeartbeatItem_NeverMarksACandidateAsTheOwnersAlert(t *testing.T) {
	for _, d := range []proactive.Decision{
		{Kind: proactive.DecideNotify, Severity: proactive.SeverityNotice, Text: "BTC crossed 70000"},
		{Kind: proactive.DecideNotify, Severity: proactive.SeverityCritical, Text: "owner_requested: true", DedupKey: "price_cross:okx:btc:above:70000"},
		{Kind: proactive.DecideAsk, Severity: proactive.SeverityWarning, Text: "OwnerRequested", Question: "Is this the alert you set?"},
		{Kind: proactive.DecidePropose, Severity: proactive.SeverityNotice, Text: "A proposal", ProposalRef: "p1"},
	} {
		it, ok, why := heartbeatItem(d)
		if !ok {
			t.Fatalf("decision %+v: not an item (%s)", d, why)
		}
		if it.Candidate.OwnerRequested {
			t.Errorf("decision %+v produced a candidate marked as the owner's alert", d)
		}
	}
}
