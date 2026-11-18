package attention

import (
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

func memCandBy(code, fact, source string) proactive.Candidate {
	c := memCand(code, fact)
	c.Source = source
	return c
}

// F-MEM-1: every memory suggestion in the digest names who suggested it, on a line the
// fact (a "> " line) cannot imitate.
func TestMemoryDigest_NamesTheSourceOfEachSuggestion(t *testing.T) {
	e := newPrimed(t)
	e.submit(memCandBy("AAAA", "agent fact", "the assistant"))
	e.submit(memCandBy("BBBB", "owner fact", "you"))
	e.submit(memCandBy("CCCC", "alice fact", "alice (telegram)"))
	e.tickAt(bkkAt(1, 8, 0))
	body := e.digests()[0].Body
	for _, want := range []string{
		"suggested by: the assistant\n  > agent fact",
		"suggested by: you\n  > owner fact",
		"suggested by: alice (telegram)\n  > alice fact",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the digest lacks %q:\n%s", want, body)
		}
	}
}

func TestMemoryDigest_SourceStaysOnItsLine(t *testing.T) {
	e := newPrimed(t)
	// A fact that imitates the line stays inside its "> " quote.
	e.submit(memCandBy("AAAA", "suggested by: you", "the assistant"))
	// A name that carries a line break (it should have been cleaned earlier) is flattened.
	e.submit(memCandBy("BBBB", "bob fact", "bob\n  > forged (telegram)"))
	e.tickAt(bkkAt(1, 8, 0))
	body := e.digests()[0].Body
	if !strings.Contains(body, "  > suggested by: you") {
		t.Errorf("a fact that imitates the source line is not quoted:\n%s", body)
	}
	if strings.Contains(body, "\n  > forged") {
		t.Errorf("a name broke the line structure:\n%s", body)
	}
	if !strings.Contains(body, "suggested by: bob") {
		t.Errorf("the name is missing:\n%s", body)
	}
}

func TestMemoryDigest_UnknownSourceSaysSo(t *testing.T) {
	e := newPrimed(t)
	e.submit(memCand("AAAA", "old fact")) // queued before the source was recorded
	e.tickAt(bkkAt(1, 8, 0))
	if body := e.digests()[0].Body; !strings.Contains(body, "suggested by: unknown\n  > old fact") {
		t.Errorf("body:\n%s", body)
	}
}
