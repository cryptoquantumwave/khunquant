package tools

import (
	"os"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// TestMain installs an always-allow rate limiter for this whole test binary.
//
// broker.DefaultLimiter is a package-level token bucket - 5 orders per
// provider per minute - shared by every test here. Tests that place orders
// against the same provider name therefore spend from one budget, so adding
// an order test can make an unrelated test fail, and `go test -count=2`
// failed outright: the second pass reuses buckets the first drained, within
// the same minute. The failures land on whichever test draws the empty
// bucket, which is rarely the test that emptied it.
//
// Nothing in this package asserts the limiter refusing - the three tests that
// swap it in-test do so only to avoid this coupling, and those swaps stay
// correct and harmless. A future test that wants real limiting should install
// its own bucket and restore the previous value, as those do.
//
// It also points KHUNQUANT_HOME at a throwaway directory. config.DefaultConfig puts the
// workspace under it, and a paper-mode order writes the paper order book into the
// workspace (broker.PaperBookRelPath): without this, a test that builds a default
// config and places a paper order would write into the developer's real ~/.khunquant.
func TestMain(m *testing.M) {
	broker.DefaultLimiter = unlimitedLimiter{}
	home, err := os.MkdirTemp("", "khunquant-tools-test-home-")
	if err != nil {
		panic(err)
	}
	os.Setenv("KHUNQUANT_HOME", home)
	code := m.Run()
	broker.ClosePaperBooks()
	os.RemoveAll(home)
	os.Exit(code)
}
