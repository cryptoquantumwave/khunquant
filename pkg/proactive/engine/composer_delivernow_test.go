package engine

import (
	"context"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/attention"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// A compose turn is told whether attention decided to deliver its message now,
// from the decisions of the message and nothing else: a digest, a drop or a log
// is not a delivery decision, and a message without decisions is not one either.
func TestComposer_DeliverNowComesFromTheAttentionDecisions(t *testing.T) {
	dec := func(d proactive.Disposition) attention.Decision { return attention.Decision{Disposition: d} }
	for _, tc := range []struct {
		name      string
		decisions []attention.Decision
		digest    bool
		want      bool
	}{
		{"an escalate", []attention.Decision{dec(proactive.DispEscalate)}, false, true},
		{"a notify and an escalate", []attention.Decision{dec(proactive.DispNotify), dec(proactive.DispEscalate)}, false, true},
		{"an escalate and a digest decision", []attention.Decision{dec(proactive.DispEscalate), dec(proactive.DispDigest)}, false, false},
		{"a log", []attention.Decision{dec(proactive.DispLog)}, false, false},
		{"a drop", []attention.Decision{dec(proactive.DispDrop)}, false, false},
		{"no decisions", nil, false, false},
		{"the digest message", []attention.Decision{dec(proactive.DispEscalate)}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := &fakeRunner{res: notifyText("ok")}
			m := template()
			m.Decisions, m.Digest = tc.decisions, tc.digest
			if _, err := NewComposer(run).Compose(context.Background(), m, escalateItems()); err != nil {
				t.Fatal(err)
			}
			r := run.requests()[0]
			if r.DeliverNow != tc.want || r.InterventionID != "IV" {
				t.Errorf("DeliverNow=%v InterventionID=%q, want %v and IV", r.DeliverNow, r.InterventionID, tc.want)
			}
		})
	}
}
