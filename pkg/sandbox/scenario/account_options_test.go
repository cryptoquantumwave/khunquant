package scenario

import "testing"

// An account block carries the venue's options, and a settrade scenario can ask for the
// placeholder account and price its symbol on the venue.
func TestValidate_AccountOptionsAndSettrade(t *testing.T) {
	sc := parseString(t, "version: 1\nscenario: t\nclock: {start: \"2026-10-20T09:00:00+07:00\"}\n"+withExpect(`instance: {accounts: [settrade]}
market:
  PTT/THB: { venue: settrade, model: set, price: 30 }
account:
  settrade:
    balances: { THB: 1000000, PTT: 500 }
    options: { auction_open: "09:55", auction_afternoon: "14:10" }
`))
	if issues := Validate(sc, nil); len(issues) != 0 {
		t.Fatalf("issues %v", issues)
	}
	seed := sc.Account["settrade"]
	if seed.Options["auction_open"] != "09:55" || seed.Balances["THB"] != 1000000 {
		t.Fatalf("seed = %+v", seed)
	}
}
