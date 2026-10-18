package proactive

import (
	"context"
	"sync"
	"testing"
)

func TestTaint_OnlyGoesDown(t *testing.T) {
	tn := NewTaint(TrustSystem)
	tn.Lower(TrustUserAuthored) // never raises
	if got := tn.Trust(); got != TrustSystem {
		t.Fatalf("Lower raised the floor: %v", got)
	}
	tn.Lower(TrustExchange)
	tn.Lower(TrustSystem)
	if got := tn.Trust(); got != TrustExchange {
		t.Fatalf("floor = %v, want exchange", got)
	}
	tn.Lower(TrustUntrusted)
	if got := tn.Trust(); got != TrustUntrusted {
		t.Fatalf("floor = %v, want untrusted", got)
	}
}

func TestTaint_NilFailsClosed(t *testing.T) {
	var tn *Taint
	tn.Lower(TrustUntrusted) // must not panic
	if got := tn.Trust(); got != TrustUntrusted {
		t.Fatalf("nil taint = %v, want untrusted", got)
	}
	if TaintFrom(context.Background()) != nil {
		t.Fatal("a context without a taint returned one")
	}
}

func TestTaint_ContextAndRace(t *testing.T) {
	tn := NewTaint(TrustSystem)
	ctx := WithTaint(context.Background(), tn)
	if TaintFrom(ctx) != tn {
		t.Fatal("TaintFrom did not return the holder")
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tn.Lower(TrustExchange)
			_ = tn.Trust()
		}()
	}
	wg.Wait()
	if got := tn.Trust(); got != TrustExchange {
		t.Fatalf("floor = %v, want exchange", got)
	}
}
