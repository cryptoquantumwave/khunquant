package labjournal

import (
	"context"
	"testing"
)

// Only written rows signal a commit. A Query's own flush (a barrier alone) does not, so
// WaitFor sleeps until a real commit instead of waking on every read it makes (133-1).
func TestCommitSignal_OnlyForWrittenRows(t *testing.T) {
	j := openTest(t, Options{})
	ctx := context.Background()

	sig := j.commitSignal()
	if _, err := j.Query(ctx, Query{}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-sig:
		t.Fatal("a flush that wrote no rows signalled a commit")
	default:
	}

	j.Append(Entry{Kind: "test.row", Source: "test"})
	if got := query(t, j, Query{}); len(got) != 1 { // flushes the row
		t.Fatalf("query = %d entries, want 1", len(got))
	}
	select {
	case <-sig:
	default:
		t.Fatal("a written row did not signal a commit")
	}
}
