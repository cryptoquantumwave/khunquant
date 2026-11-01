package inspect_test

import (
	"reflect"
	"strconv"
	"testing"
)

// from_start reads the journal oldest first with no cursor, so a reader can page through
// all of it (the scenario runner does); without it the newest entries come back.
func TestJournal_FromStartPagesFromTheBeginning(t *testing.T) {
	r := newRig(t, nil)
	for i := 0; i < 5; i++ {
		r.add("chat.out", "dm:owner")
	}
	if got := seqs(r.mustCall("journal", `{"limit":2}`)); !reflect.DeepEqual(got, []int{4, 5}) {
		t.Fatalf("no cursor: %v, want the newest two", got)
	}
	first := r.mustCall("journal", `{"limit":2,"from_start":true}`)
	if got := seqs(first); !reflect.DeepEqual(got, []int{1, 2}) {
		t.Fatalf("from_start: %v, want the oldest two", got)
	}
	next := int(first["next_seq"].(float64))
	if got := seqs(r.mustCall("journal", `{"limit":10,"from_start":true,"after_seq":`+strconv.Itoa(next)+`}`)); !reflect.DeepEqual(got, []int{3, 4, 5}) {
		t.Fatalf("continued from %d: %v, want the rest", next, got)
	}
}
