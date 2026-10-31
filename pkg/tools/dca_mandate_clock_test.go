package tools

import (
	"strings"
	"testing"
)

// The mandate's life starts on the injected clock, not the wall: a gateway whose clock is
// elsewhere (the sandbox lab's) must get an expiry that is its own months from its own now.
func TestMandateExpiryReadsTheInjectedClock(t *testing.T) {
	e := newMandateE2E(t, true)
	SetClock(e.clock.Now)
	t.Cleanup(func() { SetClock(nil) })

	_, m := e.planWithMandate(map[string]any{"mandate_months": 1})
	want := e.clock.Now().AddDate(0, 1, 0)
	if !m.ExpiresAt.Equal(want) {
		t.Fatalf("mandate expires at %v, want %v (the clock's now plus one month, not the wall's)", m.ExpiresAt, want)
	}
	if card := e.tg.last(); !strings.Contains(card.Title, "until "+want.Format("2006-01-02")) {
		t.Errorf("the card's date is not the clock's: %q", card.Title)
	}
}
