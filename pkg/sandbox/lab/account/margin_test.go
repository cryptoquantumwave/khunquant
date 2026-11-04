package account

import (
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

// A seeded position carries its margining, and a bad value is refused before anything is
// written.
func TestSeed_PositionMargining(t *testing.T) {
	r := newRig(t)
	pos := func(extra string) string {
		return `{"venue":"okx","balances":{"USDT":500},"positions":[{"symbol":"BTC-USDT-SWAP","side":"long","contracts":10,"entryPrice":50000,"leverage":5` + extra + `}]}`
	}
	if _, err := r.call("seed", pos(`,"marginMode":"Isolated","maintRate":0.01,"isolatedMargin":100`)); err != nil {
		t.Fatal(err)
	}
	var got struct {
		mode      string
		rate, iso float64
	}
	r.sim.Read("okx", func(st *sandbox.VenueState) {
		p := st.Positions["BTC-USDT-SWAP"]
		got.mode, got.rate, got.iso = p.MarginMode, p.MaintRate, p.Isolated
	})
	if got.mode != "isolated" || got.rate != 0.01 || got.iso != 100 {
		t.Errorf("seeded position = %+v, want isolated 0.01 100", got)
	}
	for _, bad := range []string{`,"marginMode":"portfolio"`, `,"maintRate":1`, `,"maintRate":-0.1`, `,"isolatedMargin":-1`} {
		if _, err := r.call("seed", pos(bad)); code(err) != "bad_args" {
			t.Errorf("seed with %s = %v, want bad_args", bad, err)
		}
	}
	if r.free("okx", "USDT") != 500 {
		t.Errorf("a refused seed changed the balance to %v", r.free("okx", "USDT"))
	}
}
