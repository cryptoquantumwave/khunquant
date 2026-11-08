package sources

import (
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/randsrc"
)

// TestSeededNewsNonceRepeats: armed and seeded (a locked sandbox), the news fence nonce
// repeats for one seed; unarmed it is random.
func TestSeededNewsNonceRepeats(t *testing.T) {
	t.Cleanup(randsrc.ResetForTest)
	run := func(seed int64) (a, b string) {
		randsrc.ResetForTest()
		randsrc.Arm()
		if err := randsrc.Seed(seed); err != nil {
			t.Fatal(err)
		}
		return newsNonce(), newsNonce()
	}
	a1, b1 := run(5)
	a2, b2 := run(5)
	if a1 != a2 || b1 != b2 || a1 == b1 || len(a1) != 24 {
		t.Errorf("seeded sequence = %s %s / %s %s", a1, b1, a2, b2)
	}
	randsrc.ResetForTest()
	if newsNonce() == newsNonce() {
		t.Error("an unarmed process drew the same news nonce twice")
	}
}
