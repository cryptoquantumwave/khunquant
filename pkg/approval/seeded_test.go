package approval

import (
	"slices"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/randsrc"
)

func seededRun(t *testing.T, seed int64) (codes, nonces []string) {
	t.Helper()
	randsrc.ResetForTest()
	randsrc.Arm()
	if err := randsrc.Seed(seed); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		codes, nonces = append(codes, newCode()), append(nonces, newNonce())
	}
	return codes, nonces
}

// TestSeededNoncesRepeat: in a process armed and seeded as a locked sandbox is, approval
// codes and button nonces repeat for one seed (so two runs of a scenario mint the same
// cards) and differ for another. Unarmed they come from crypto/rand (randsrc's own test).
func TestSeededNoncesRepeat(t *testing.T) {
	t.Cleanup(randsrc.ResetForTest)
	c1, n1 := seededRun(t, 99)
	c2, n2 := seededRun(t, 99)
	if !slices.Equal(c1, c2) || !slices.Equal(n1, n2) {
		t.Errorf("same seed, different sequence:\n%v %v\n%v %v", c1, n1, c2, n2)
	}
	c3, n3 := seededRun(t, 100)
	if slices.Equal(c1, c3) || slices.Equal(n1, n3) {
		t.Error("different seeds must differ")
	}
	for _, c := range c1 {
		if !ValidCode(c) {
			t.Errorf("seeded code %q has the wrong shape", c)
		}
	}
}

func TestUnseededCodesDiffer(t *testing.T) {
	randsrc.ResetForTest()
	var a, b []string
	for range 8 {
		a = append(a, newCode()+newNonce())
	}
	for range 8 {
		b = append(b, newCode()+newNonce())
	}
	if slices.Equal(a, b) {
		t.Errorf("codes and nonces drawn twice came out equal: %v", a)
	}
}
