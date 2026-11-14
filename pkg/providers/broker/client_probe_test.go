package broker_test

import (
	"context"
	"errors"
	"testing"
	"time"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// probeProv is a capable adapter with no by-ID lookup but a probe (Webull): a hit
// is the order, a miss proves nothing.
type probeProv struct {
	capNoLookup
	down bool // the probe finds nothing, whatever the exchange holds
}

var _ broker.ClientIDProbe = probeProv{}

func (p probeProv) ProbeOrderByClientID(_ context.Context, _, _, cid string) (ccxt.Order, bool) {
	p.ex.mu.Lock()
	defer p.ex.mu.Unlock()
	p.ex.calls["probe"]++
	if p.down {
		return ccxt.Order{}, false
	}
	for _, o := range p.ex.orders {
		if o.ClientOrderId != nil && *o.ClientOrderId == cid {
			return o, true
		}
	}
	return ccxt.Order{}, false
}

// An earlier attempt's order rests on the exchange and the listings do not show it
// (a page cut short, or a listing that lags). Only the probe can find it. A probe
// that finds nothing changes nothing: the listings decide, as without it.
func TestClientIDProbe_FindsWhatTheListingsDoNotShow(t *testing.T) {
	cid := broker.ClientOrderID(key, 32)
	for _, tc := range []struct {
		name       string
		prov       func(*exch) broker.Provider
		side       string // of the order the exchange holds under the ID
		wantPlaced int
		wantErr    error
	}{
		{"no probe: the empty listings are believed and the order is placed again", func(e *exch) broker.Provider { return capNoLookup{newPlain(e)} }, "buy", 1, nil},
		{"a probe: found, nothing placed", func(e *exch) broker.Provider { return probeProv{capNoLookup: capNoLookup{newPlain(e)}} }, "buy", 0, nil},
		{"a probe that finds nothing: as with no probe", func(e *exch) broker.Provider { return probeProv{capNoLookup: capNoLookup{newPlain(e)}, down: true} }, "buy", 1, nil},
		{"a probe that finds the ID on the other side: for a person", func(e *exch) broker.Provider { return probeProv{capNoLookup: capNoLookup{newPlain(e)}} }, "sell", 0, broker.ErrReconcileAmbiguous},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clk := newClock()
			ex := newExch(clk)
			seed(ex, "lost-1", tc.side, "market", 1, nil, "open", t0, cid)
			ex.hideListings = true
			clk.Advance(time.Hour)
			o, err := run(t, tc.prov(ex), spotReq(1), 2, clk)
			switch {
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
			case err != nil:
				t.Fatal(err)
			case tc.wantPlaced == 0 && (o.Id == nil || *o.Id != "lost-1"):
				t.Errorf("returned order %v, want the one the exchange holds", o.Id)
			}
			if ex.placed() != tc.wantPlaced {
				t.Errorf("%d orders placed, want %d", ex.placed(), tc.wantPlaced)
			}
		})
	}
}

// A probe is asked once per reconcile and never for an adapter that has the real
// lookup: that one's "not found" is an answer, the probe's is not.
func TestClientIDProbe_NotAskedWhenTheAdapterHasALookup(t *testing.T) {
	clk := newClock()
	ex := newExch(clk)
	p := struct {
		capProv
		broker.ClientIDProbe
	}{newCap(ex), probeProv{capNoLookup: capNoLookup{newPlain(ex)}}}
	clk.Advance(time.Hour)
	if _, err := run(t, p, spotReq(1), 2, clk); err != nil {
		t.Fatal(err)
	}
	if n := ex.count("probe"); n != 0 {
		t.Errorf("the probe was asked %d times", n)
	}
	if n := ex.count("lookup"); n == 0 {
		t.Error("the lookup was not asked")
	}
}
