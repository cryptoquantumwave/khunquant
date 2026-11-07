package gateway

import (
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/randsrc"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

// nonces draws what a process mints after seeding with (seed, boot): the stream every
// approval code, button nonce and proactive id is cut from.
func nonces(t *testing.T, seed string, boot int64, n int) []string {
	t.Helper()
	sandbox.ResetLockForTest()
	t.Cleanup(sandbox.ResetLockForTest)
	if err := sandbox.Lock(sandbox.LockOptions{Token: strings.Repeat("t", 32)}); err != nil {
		t.Fatal(err)
	}
	env := func(k string) string {
		if k == sandbox.EnvSeed {
			return seed
		}
		return ""
	}
	if err := seedFromEnv(env, boot); err != nil {
		t.Fatal(err)
	}
	out := make([]string, n)
	for i := range out {
		b := make([]byte, 8)
		if _, err := randsrc.Read(b); err != nil {
			t.Fatal(err)
		}
		out[i] = string(b)
	}
	return out
}

// A restarted instance (the next boot of the same home, with the same KQ_SANDBOX_SEED)
// does not mint the codes, nonces and ids its predecessor stored, while two runs of one
// scenario (the same boots in the same order) mint the same ones.
func TestSeedAcrossRestart_NoRepeatedNonces(t *testing.T) {
	first := nonces(t, "12345", 1, 200)
	again := nonces(t, "12345", 1, 200)
	for i := range first {
		if first[i] != again[i] {
			t.Fatalf("two first boots of one seed differ at draw %d", i)
		}
	}
	restarted := nonces(t, "12345", 2, 200)
	seen := map[string]bool{}
	for _, v := range first {
		seen[v] = true
	}
	for i, v := range restarted {
		if seen[v] {
			t.Fatalf("the restarted instance repeats draw %d of its predecessor", i)
		}
	}
	again2 := nonces(t, "12345", 2, 200)
	for i := range restarted {
		if restarted[i] != again2[i] {
			t.Fatalf("two second boots of one seed differ at draw %d", i)
		}
	}
	third := nonces(t, "12345", 3, 200)
	for i, v := range third {
		if seen[v] || v == restarted[i] {
			t.Fatalf("boot 3 repeats an earlier boot at draw %d", i)
		}
	}
}
