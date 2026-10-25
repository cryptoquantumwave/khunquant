package proactive_test

import (
	"testing"
	"unicode"

	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// pkg/proactive and pkg/perception each define IsBidiControl (neither may import
// the other's package from non-test code). They must never drift apart.
func TestIsBidiControl_AgreesWithPerception(t *testing.T) {
	n := 0
	for r := rune(0); r <= unicode.MaxRune; r++ {
		got, want := proactive.IsBidiControl(r), perception.IsBidiControl(r)
		if got != want {
			t.Fatalf("U+%04X: proactive says %v, perception says %v", r, got, want)
		}
		if got {
			n++
		}
	}
	// 5 embeddings and overrides, 4 isolates, 2 marks, 1 letter mark.
	if n != 12 {
		t.Fatalf("%d runes are bidi controls, want 12", n)
	}
	for _, r := range []rune{0x200C, 0x200D, 'a', 0x0E01} {
		if proactive.IsBidiControl(r) {
			t.Errorf("U+%04X must not be a bidi control", r)
		}
	}
}
