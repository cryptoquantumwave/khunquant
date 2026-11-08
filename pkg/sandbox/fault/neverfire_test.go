package fault

import (
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

// A rule that could never fire is refused at Add rather than stored, so a scenario cannot
// pass for the wrong reason (176-2, 179-2). Each refused rule has a twin, differing only
// in what makes it unfireable, that is accepted.
func TestAdd_RefusesRulesThatCanNeverFire(t *testing.T) {
	i, _, _ := newInj(t, Options{MaxHold: time.Second})
	i.EnableOrderKinds("accept_no_fill")
	for _, c := range []struct {
		name          string
		refused, twin Rule
	}{
		{"unknown venue", Rule{Match: Match{Venue: "okex"}, Effect: status(500)},
			Rule{Match: Match{Venue: "okx"}, Effect: status(500)}},
		{"order field without a classifier", Rule{Match: Match{Venue: "bitkub", Order: "create"}, Effect: status(500)},
			Rule{Match: Match{Venue: "binance", Order: "create"}, Effect: status(500)}},
		{"symbol without a classifier", Rule{Match: Match{Venue: "bitkub", Symbol: "BTC/THB"}, Effect: status(500)},
			Rule{Match: Match{Venue: "binance", Symbol: "BTC/USDT"}, Effect: status(500)}},
		{"written_then_timeout without a classifier", Rule{Match: Match{Venue: "bitkub"}, Effect: Effect{Order: "written_then_timeout"}},
			Rule{Match: Match{Venue: "binance"}, Effect: Effect{Order: "written_then_timeout"}}},
		{"order state without an order ledger", Rule{Match: Match{Venue: "binanceth"}, Effect: Effect{Order: "accept_no_fill"}},
			Rule{Match: Match{Venue: "okx"}, Effect: Effect{Order: "accept_no_fill"}}},
		{"symbol on binanceth", Rule{Match: Match{Venue: "binanceth", Symbol: "BTC/USDT"}, Effect: status(500)},
			Rule{Match: Match{Venue: "binanceth", Order: "create"}, Effect: status(500)}},
		{"window already over", Rule{Match: Match{Venue: "okx"}, Effect: status(500), When: When{To: "2000-01-01T00:00:00Z"}},
			Rule{Match: Match{Venue: "okx"}, Effect: status(500), When: When{To: "+5m"}}},
		{"empty window", Rule{Match: Match{Venue: "okx"}, Effect: status(500), When: When{From: "+5m", To: "+5m"}},
			Rule{Match: Match{Venue: "okx"}, Effect: status(500), When: When{From: "+5m", To: "+6m"}}},
	} {
		if _, err := i.Add(c.refused); err == nil {
			t.Errorf("%s: a rule that can never fire was accepted", c.name)
		}
		if _, err := i.Add(c.twin); err != nil {
			t.Errorf("%s: its fireable twin was refused: %v", c.name, err)
		}
	}
	if _, err := i.Add(Rule{Match: Match{Venue: "*", Order: "create"}, Effect: status(500)}); err != nil {
		t.Errorf("a rule for every venue was refused: %v", err)
	}
	if _, err := i.Add(Rule{Match: Match{Venue: "web"}, Effect: status(500)}); err != nil {
		t.Errorf("a rule for the web venue (SimWeb) was refused: %v", err)
	}
}

// The refusals follow the code, not a list written beside it: every venue with a simulator
// (and so an order ledger) takes order-state effects, and has a row in the classifier
// table, so a Settrade or Webull rule that can fire is not refused. A venue added to
// sandbox.AccountVenues without a classifier row fails here.
func TestAdd_AcceptsRulesOnEveryVenueWithALedgerAndClassifier(t *testing.T) {
	i, _, _ := newInj(t, Options{MaxHold: time.Second})
	i.EnableOrderKinds("accept_no_fill", "partial")
	if len(sandbox.AccountVenues) == 0 {
		t.Fatal("no account venues")
	}
	for _, v := range sandbox.AccountVenues {
		if !classifiesOrders(v) {
			t.Errorf("%s has a simulator but no order classifier row", v)
		}
		for _, r := range []Rule{
			{Match: Match{Venue: v, Order: "create", Side: "buy"}, Effect: status(500)},
			{Match: Match{Venue: v}, Effect: Effect{Order: "written_then_timeout"}},
			{Match: Match{Venue: v}, Effect: Effect{Order: "accept_no_fill"}},
			{Match: Match{Venue: v, Order: "create"}, Effect: Effect{Order: "partial", Fraction: 0.5}},
		} {
			if _, err := i.Add(r); err != nil {
				t.Errorf("%s: a rule that can fire was refused: %v", v, err)
			}
		}
	}
	if _, err := i.Add(Rule{Match: Match{Venue: "bitkub", Order: "create"}, Effect: status(500)}); err == nil {
		t.Error("bitkub has no order classifier, yet an order match was accepted")
	}
}
