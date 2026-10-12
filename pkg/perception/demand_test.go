package perception

import (
	"sync"
	"testing"
)

func TestStaticDemands_ZeroAndNilAreEmpty(t *testing.T) {
	var z StaticDemands
	if len(z.Demands()) != 0 {
		t.Fatal("the zero value is not empty")
	}
	var n *StaticDemands
	if len(n.Demands()) != 0 {
		t.Fatal("a nil source is not empty")
	}
}

func TestStaticDemands_SetAddAndSnapshotsAreStable(t *testing.T) {
	s := NewStaticDemands(Demand{Provider: "binance", Symbol: "BTC/USDT"})
	snap := s.Demands()
	s.Add(Demand{Provider: "okx", Symbol: "ETH/USDT"})
	if len(snap) != 1 {
		t.Fatalf("a snapshot changed under its holder: %d", len(snap))
	}
	if got := s.Demands(); len(got) != 2 || got[1].Provider != "okx" {
		t.Fatalf("demands = %+v", got)
	}
	in := []Demand{{Symbol: "A"}}
	s.Set(in)
	in[0].Symbol = "changed"
	if got := s.Demands(); len(got) != 1 || got[0].Symbol != "A" {
		t.Fatalf("Set kept the caller's slice: %+v", got)
	}
	s.Set(nil)
	if len(s.Demands()) != 0 {
		t.Fatal("Set(nil) did not empty the source")
	}
}

func TestStaticDemands_Concurrent(t *testing.T) {
	s := NewStaticDemands()
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				s.Add(Demand{Symbol: "X"})
				s.Set([]Demand{{Symbol: "Y"}})
			}
		}()
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				for range s.Demands() {
				}
			}
		}()
	}
	wg.Wait()
}

func TestDemandKind_String(t *testing.T) {
	for k, want := range map[DemandKind]string{
		DemandTicker: "ticker", DemandOHLCV: "ohlcv", DemandFuturesPositions: "futures_positions", DemandKind(99): "unknown",
	} {
		if k.String() != want {
			t.Errorf("%d = %q, want %q", int(k), k.String(), want)
		}
	}
}
