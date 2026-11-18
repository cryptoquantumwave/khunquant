package tools

import (
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/approval"
)

// F-MEM-1: remember says who a suggestion is from, so that approval can bound a person's
// suggestions apart from the agent's and the digest can name the source.
func TestRemember_SuggesterIsTheTurnsAuthor(t *testing.T) {
	want := map[string]approval.Suggester{
		"proactive":          {},
		"governed heartbeat": {},
		"heartbeat":          {},
		"heartbeat sub-turn": {},
		"cron":               {},
		"owner sub-turn":     {Kind: approval.SuggestedByOwner},
		"console sub-turn":   {Kind: approval.SuggestedByOwner},
		"another sender":     {Kind: approval.SuggestedByPerson, Channel: "telegram", ID: "999"},
	}
	for name, suggester := range want {
		t.Run(name, func(t *testing.T) {
			r := newRememberRig(t, true, true)
			if res := r.remember(turns()[name], "I prefer weekly summaries"); res.IsError {
				t.Fatalf("remember: %s", res.ForLLM)
			}
			got := r.prop.all()
			if len(got) != 1 || got[0].Suggester != suggester {
				t.Fatalf("suggester = %+v, want %+v", got, suggester)
			}
		})
	}
}

func TestRemember_PersonsSubTurnStaysThePersons(t *testing.T) {
	r := newRememberRig(t, true, true)
	tn := turns()["another sender"]
	tn.ctx = WithSubTurn(tn.ctx)
	if res := r.remember(tn, "I prefer weekly summaries"); res.IsError {
		t.Fatalf("remember: %s", res.ForLLM)
	}
	got := r.prop.all()
	if len(got) != 1 || got[0].Suggester.Kind != approval.SuggestedByPerson || got[0].Suggester.ID != "999" {
		t.Fatalf("a delegated task must not shed its requester: %+v", got)
	}
}
