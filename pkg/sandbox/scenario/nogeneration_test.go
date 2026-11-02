package scenario

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labclient"
)

// Every lab group gives one answer while no generation of services is published, and the
// runner's noServices recognises it for each of them. The text is the gateway's
// (errNoGeneration, pinned there by TestLabBetweenGenerations_OneAnswerForEveryGroup).
func TestNoServices_MatchesTheAnswerOfEveryGroup(t *testing.T) {
	const msg = "no generation of services is running (a reload is in progress?)"
	for _, g := range []string{"inspect", "call_tool", "manage"} {
		err := fmt.Errorf("%s: %w", g, &labclient.Error{Status: http.StatusServiceUnavailable, Code: "unavailable", Msg: msg})
		if !noServices(err) {
			t.Errorf("%s: noServices(%v) = false", g, err)
		}
	}
	// The answers it must not match: a feature the instance lacks is not a reload.
	for _, msg := range []string{"settle is not available in this instance", "the quiescence registry is not available in this instance"} {
		if noServices(&labclient.Error{Status: http.StatusServiceUnavailable, Code: "unavailable", Msg: msg}) {
			t.Errorf("noServices matched %q", msg)
		}
	}
}
