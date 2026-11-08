package account

import "testing"

// webull is a venue with a simulator: cash and holdings are balances; the options are a
// duplicate-ID switch and a whole-number list seed, and everything wrong is refused before
// anything is written.
func TestSeed_Webull(t *testing.T) {
	r := newRig(t)
	out, err := r.call("seed", `{"venue":"webull","balances":{"USD":50000,"AAPL":10},"options":{"reject_duplicate_client_id":true,"list_seed":7}}`)
	if err != nil {
		t.Fatal(err)
	}
	if bal := out["balances"].(map[string]any); bal["USD"].(map[string]any)["free"] != 50000.0 || bal["AAPL"].(map[string]any)["free"] != 10.0 {
		t.Fatalf("balances = %v", bal)
	}
	for name, args := range map[string]string{
		"unknown option":            `{"venue":"webull","balances":{"USD":1},"options":{"auction_open":"10:00"}}`,
		"switch not a bool":         `{"venue":"webull","balances":{"USD":1},"options":{"reject_duplicate_client_id":"yes"}}`,
		"seed is a string":          `{"venue":"webull","balances":{"USD":1},"options":{"list_seed":"7"}}`,
		"seed is fractional":        `{"venue":"webull","balances":{"USD":1},"options":{"list_seed":1.5}}`,
		"seed is negative":          `{"venue":"webull","balances":{"USD":1},"options":{"list_seed":-1}}`,
		"seed is too large":         `{"venue":"webull","balances":{"USD":1},"options":{"list_seed":1e300}}`,
		"a position":                `{"venue":"webull","positions":[{"symbol":"AAPL","side":"long","contracts":1,"entryPrice":150}]}`,
		"settrade option on webull": `{"venue":"webull","options":{"auction_afternoon":"14:30"}}`,
	} {
		if _, err := r.call("seed", args); code(err) != "bad_args" {
			t.Errorf("%s: %v, want bad_args", name, err)
		}
	}
	if r.free("webull", "USD") != 50000 {
		t.Errorf("a refused seed changed the balance to %v", r.free("webull", "USD"))
	}
}
