package agent

import (
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/randsrc"
)

// TestSeededFenceNonceRepeats: armed and seeded (a locked sandbox), the candidate fence
// nonce repeats for one seed; unarmed it is random.
func TestSeededFenceNonceRepeats(t *testing.T) {
	t.Cleanup(randsrc.ResetForTest)
	run := func(seed int64) (a, b string) {
		randsrc.ResetForTest()
		randsrc.Arm()
		if err := randsrc.Seed(seed); err != nil {
			t.Fatal(err)
		}
		return newFenceNonce(), newFenceNonce()
	}
	a1, b1 := run(5)
	a2, b2 := run(5)
	if a1 != a2 || b1 != b2 || a1 == b1 {
		t.Errorf("seeded sequence = %s %s / %s %s", a1, b1, a2, b2)
	}
	randsrc.ResetForTest()
	if newFenceNonce() == newFenceNonce() {
		t.Error("an unarmed process drew the same fence nonce twice")
	}
}
