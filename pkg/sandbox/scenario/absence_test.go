package scenario

import (
	"strings"
	"testing"
)

// A chat step whose message was not delivered fails the step (TRUST-2); expect_undelivered
// opts out for a scenario that tests the refusal, and then a delivered message fails it.
func TestRunner_UndeliveredChatStep(t *testing.T) {
	cases := []struct {
		name, step string
		pass       bool
		failure    string
	}{
		{"delivered", "chat: {as: owner, text: hi}", true, ""},
		{"undelivered fails", "chat: {as: stranger, text: hi}", false, "the message was not delivered"},
		{"opt-out passes", "chat: {as: stranger, text: hi, expect_undelivered: true}", true, ""},
		{"opt-out but delivered fails", "chat: {as: owner, text: hi, expect_undelivered: true}", false, "expect_undelivered is set but the message was delivered"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := runOne(t, newFakeSup(t), scenarioOf(t, "timeline:\n  - "+tc.step+"\n  - advance: 1m\n")).Runs[0]
			if res.Pass != tc.pass || (tc.failure != "" && !strings.Contains(res.Failure, tc.failure)) {
				t.Fatalf("result = pass %v, failure %q", res.Pass, res.Failure)
			}
		})
	}
}
