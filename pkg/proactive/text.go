package proactive

// IsBidiControl reports whether r is a bidirectional control character: the
// embeddings and overrides U+202A-U+202E, the isolates U+2066-U+2069, the marks
// U+200E and U+200F and the Arabic letter mark U+061C. They change the order text
// is displayed in without being visible, so a message that holds one can show the
// owner something other than what it says. The joiners U+200C and U+200D are not
// among them: emoji and several scripts need them.
//
// pkg/perception has the same function under the same name. It cannot import this
// package (its import boundary forbids it), and the intent ledger cannot import
// perception, so each has its own; text_test.go checks that they agree on every
// rune.
func IsBidiControl(r rune) bool {
	switch {
	case r >= 0x202A && r <= 0x202E, r >= 0x2066 && r <= 0x2069,
		r == 0x200E, r == 0x200F, r == 0x061C:
		return true
	}
	return false
}
