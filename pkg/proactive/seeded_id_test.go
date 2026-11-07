package proactive

import (
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/randsrc"
)

// TestSeededIDEntropyRepeats: armed and seeded (a locked sandbox), the entropy of the
// first ID of a millisecond repeats for one seed. The time part is the clock's, which the
// model canonicaliser masks, so only the entropy is compared.
func TestSeededIDEntropyRepeats(t *testing.T) {
	t.Cleanup(randsrc.ResetForTest)
	entropy := func(seed int64) [entropyLen]byte {
		randsrc.ResetForTest()
		randsrc.Arm()
		if err := randsrc.Seed(seed); err != nil {
			t.Fatal(err)
		}
		var e [entropyLen]byte
		fillEntropy(&e)
		return e
	}
	if entropy(3) != entropy(3) {
		t.Error("same seed, different entropy")
	}
	if entropy(3) == entropy(4) {
		t.Error("different seeds, same entropy")
	}
	randsrc.ResetForTest()
	var a, b [entropyLen]byte
	fillEntropy(&a)
	fillEntropy(&b)
	if a == b {
		t.Error("unarmed entropy repeated")
	}
}
