package broker_test

import (
	"fmt"
	"regexp"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

func TestClientOrderID_DeterministicAndBounded(t *testing.T) {
	alpha := regexp.MustCompile(`^kq[a-z2-7]+$`)
	key := "wf-1:3:place"
	for _, n := range []int{1, 2, 16, 20, 32, 36, 54, 100} {
		got := broker.ClientOrderID(key, n)
		if again := broker.ClientOrderID(key, n); again != got {
			t.Fatalf("maxLen %d: not deterministic: %q vs %q", n, got, again)
		}
		want := n
		if want > 54 {
			want = 54
		}
		if len(got) != want {
			t.Errorf("maxLen %d: len %d, want %d", n, len(got), want)
		}
		if n > 2 && !alpha.MatchString(got) {
			t.Errorf("maxLen %d: %q is not kq + lower-case base32", n, got)
		}
	}
	if got := broker.ClientOrderID(key, 0); got != "" {
		t.Errorf("maxLen 0 = %q, want empty", got)
	}
	if got := broker.ClientOrderID(key, -3); got != "" {
		t.Errorf("negative maxLen = %q, want empty", got)
	}
	if got := broker.ClientOrderID(key, 32); got[:16] != broker.ClientOrderID(key, 16) {
		t.Error("a longer ID must extend the shorter one")
	}
	if broker.ClientOrderID("a", 32) == broker.ClientOrderID("b", 32) {
		t.Error("different keys gave the same ID")
	}
}

func TestClientOrderID_NoCollisionInFirst16CharsOverAMillionKeys(t *testing.T) {
	if testing.Short() {
		t.Skip("a million hashes")
	}
	const n = 1_000_000
	seen := make(map[string]int, n)
	for i := 0; i < n; i++ {
		id := broker.ClientOrderID(fmt.Sprintf("wf-%d:%d:place", i/7, i), 16)
		if len(id) != 16 {
			t.Fatalf("len %d", len(id))
		}
		if j, dup := seen[id]; dup {
			t.Fatalf("keys %d and %d share the first 16 characters %q", j, i, id)
		}
		seen[id] = i
	}
}
