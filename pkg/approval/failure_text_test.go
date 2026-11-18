package approval

import (
	"strings"
	"testing"
)

// A failed order's text depends on the mode it ran under (Proposal.Mode, which the
// recheck step pins to the execution mode): a paper order never sends the owner to
// the exchange, a live one keeps its wording.
func TestFailureText_ByMode(t *testing.T) {
	op := OrderParams{Provider: "okx"}
	for _, lang := range []string{"en", "th"} {
		m := msgs[lang]
		for _, mayExist := range []bool{true, false} {
			reason := "boom"
			f := failure{reason: reason, mayExist: mayExist}

			paper := failureText(Proposal{Code: "7F3K", Lang: lang, Mode: ModePaper}, op, f)
			if paper != m.paperFailed("7F3K", reason) {
				t.Errorf("%s mayExist=%v: paper text = %q", lang, mayExist, paper)
			}
			for _, bad := range []string{"MAY OR MAY NOT", "Check okx", "อาจถูกส่ง", "โปรดตรวจที่ okx"} {
				if strings.Contains(paper, bad) {
					t.Errorf("%s: paper text %q must not contain %q", lang, paper, bad)
				}
			}
			if !strings.Contains(paper, "PAPER") || !strings.Contains(paper, reason) {
				t.Errorf("%s: paper text lacks PAPER or the reason: %q", lang, paper)
			}
			if lang == "en" && !strings.Contains(paper, "NOTHING was placed on the exchange") {
				t.Errorf("paper text does not say nothing was placed: %q", paper)
			}

			for _, mode := range []string{ModeLive, ModeRealExit, ""} {
				got := failureText(Proposal{Code: "7F3K", Lang: lang, Mode: mode}, op, f)
				want := m.notPlaced("7F3K", reason)
				if mayExist {
					want = m.mayExist("7F3K", reason, "okx")
				}
				if got != want {
					t.Errorf("%s mode=%q mayExist=%v: got %q, want the unchanged %q", lang, mode, mayExist, got, want)
				}
			}
		}
	}
	live := failureText(Proposal{Code: "7F3K", Lang: "en", Mode: ModeLive}, op, failure{reason: "x", mayExist: true})
	if !strings.Contains(live, "MAY OR MAY NOT HAVE BEEN PLACED on okx (x). Check okx before doing anything else") {
		t.Errorf("live mayExist text changed: %q", live)
	}
}
