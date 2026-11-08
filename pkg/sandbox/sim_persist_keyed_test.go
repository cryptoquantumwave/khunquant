package sandbox

import (
	"bytes"
	"testing"
	"time"
)

// The save key leaves out LastDerivs (it moves on every step) and nothing else: a real
// change moves the key, a LastDerivs-only change does not, the bytes written always carry
// LastDerivs, and taking the key leaves LastDerivs where it was.
func TestMarshalStateKeyed_IgnoresOnlyLastDerivs(t *testing.T) {
	sm := NewStateManager()
	sim := NewStatefulSimulator(sm)
	st := sm.GetState("okx")
	at := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	st.LastDerivs = at

	s1, k1, err := sim.MarshalStateKeyed()
	if err != nil {
		t.Fatal(err)
	}
	if !st.LastDerivs.Equal(at) {
		t.Fatalf("LastDerivs = %v after taking the key, want %v", st.LastDerivs, at)
	}
	if !bytes.Contains(s1, []byte("lastDerivs")) || bytes.Contains(k1, []byte("lastDerivs")) {
		t.Fatalf("state must carry lastDerivs and the key must not\nstate=%s\nkey=%s", s1, k1)
	}
	if plain, err := sim.MarshalState(); err != nil || !bytes.Equal(plain, s1) {
		t.Fatalf("MarshalState and the keyed state differ (err %v)", err)
	}

	st.LastDerivs = at.Add(2 * time.Hour)
	s2, k2, _ := sim.MarshalStateKeyed()
	if !bytes.Equal(k1, k2) || bytes.Equal(s1, s2) {
		t.Fatal("a LastDerivs-only change must move the state and not the key")
	}

	st.Balances["USDT"] = Balance{Free: 5}
	_, k3, _ := sim.MarshalStateKeyed()
	if bytes.Equal(k2, k3) {
		t.Fatal("a balance change must move the key")
	}
}
