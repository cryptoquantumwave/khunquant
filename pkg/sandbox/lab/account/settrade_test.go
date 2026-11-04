package account

import (
	"testing"
)

// settrade is a venue with a simulator: cash and holdings are balances, the options set the
// auction times, and everything wrong is refused before anything is written.
func TestSeed_Settrade(t *testing.T) {
	r := newRig(t)
	out, err := r.call("seed", `{"venue":"settrade","balances":{"THB":1000000,"PTT":500},"options":{"auction_open":"09:55","auction_afternoon":"14:10"}}`)
	if err != nil {
		t.Fatal(err)
	}
	bal := out["balances"].(map[string]any)
	if bal["THB"].(map[string]any)["free"] != 1000000.0 || bal["PTT"].(map[string]any)["free"] != 500.0 {
		t.Fatalf("balances = %v", bal)
	}
	if r.free("binance", "USDT") != 100 {
		t.Fatal("seeding settrade touched binance")
	}

	for name, args := range map[string]string{
		"unknown option":    `{"venue":"settrade","balances":{"THB":1},"options":{"list_seed":1}}`,
		"option not a time": `{"venue":"settrade","balances":{"THB":1},"options":{"auction_open":"10"}}`,
		"option not text":   `{"venue":"settrade","balances":{"THB":1},"options":{"auction_open":10}}`,
		"time out of range": `{"venue":"settrade","balances":{"THB":1},"options":{"auction_afternoon":"17:00"}}`,
		"a position":        `{"venue":"settrade","positions":[{"symbol":"PTT","side":"long","contracts":1,"entryPrice":30}]}`,
		"options on okx":    `{"venue":"okx","balances":{"USDT":1},"options":{"auction_open":"10:00"}}`,
		"negative holdings": `{"venue":"settrade","balances":{"PTT":-5}}`,
	} {
		if _, err := r.call("seed", args); code(err) != "bad_args" {
			t.Errorf("%s: %v, want bad_args", name, err)
		}
	}
	if r.free("settrade", "THB") != 1000000 {
		t.Errorf("a refused seed changed the balance to %v", r.free("settrade", "THB"))
	}
}
