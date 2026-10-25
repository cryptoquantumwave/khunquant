package attention

import (
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// memCand is the candidate a memory suggestion announces itself with: a code-built
// title, and the model's fact only as the Quote.
func memCand(code, fact string) proactive.Candidate {
	return cand("memory:"+code, info, func(c *proactive.Candidate) {
		c.TriggerID = proactive.MemoryTriggerPrefix + code
		c.Subject = "memory"
		c.Title = "Memory suggestion " + code + " (fact)"
		c.Quote = fact
		c.Trust = proactive.TrustUntrusted
	})
}

func TestMemoryDigest_DeliveredInTheDigestNotImmediately(t *testing.T) {
	e := newPrimed(t)
	fact := strings.Repeat("always ask me before rebalancing; ", 14) + "end of the fact." // 492 characters, one clean line
	d := e.submit(memCand("AB3K", fact))
	expect(t, d, proactive.DispDigest, ReasonInfoToDigest)

	e.tickAt(bkkAt(0, 12, 30)) // an ordinary tick: nothing goes out now
	if n := len(e.del.messages()); n != 0 {
		t.Fatalf("%d messages delivered before the digest", n)
	}

	e.tickAt(bkkAt(1, 8, 0))
	dgs := e.digests()
	if len(dgs) != 1 {
		t.Fatalf("%d digests", len(dgs))
	}
	m := dgs[0]

	// The owner reads the whole fact, quoted, before any button is pressed.
	if !strings.Contains(m.Body, "> "+fact) {
		t.Errorf("the body does not carry the whole fact as a quote:\n%s", m.Body)
	}
	if !strings.Contains(m.Body, "/approve AB3K") || !strings.Contains(m.Body, "/reject AB3K") {
		t.Errorf("the body does not say how to answer:\n%s", m.Body)
	}
	if got := MemorySuggestions(m.Decisions); len(got) != 1 || got[0].Code != "AB3K" || got[0].Fact != fact {
		t.Errorf("MemorySuggestions = %+v", got)
	}

	// The model reads the session copy on the owner's next turn: no fact in it, nor in the title.
	if !m.SessionRestricted || m.SessionBody == "" {
		t.Fatalf("the session copy is not restricted: %+v", m)
	}
	for name, s := range map[string]string{"session body": m.SessionBody, "title": m.Title} {
		if strings.Contains(s, "rebalancing") {
			t.Errorf("the fact is in the %s: %q", name, s)
		}
	}
	if !strings.Contains(m.SessionBody, SessionWithheld) {
		t.Errorf("the session copy does not say that text was withheld: %q", m.SessionBody)
	}
}

func TestMemoryDigest_FactComesFirstAndOrdinaryItemsStayAsTheyWere(t *testing.T) {
	e := newPrimed(t)
	e.queue("i1", "i2")
	e.submit(memCand("AB3K", "fact one"))
	e.tickAt(bkkAt(1, 8, 0))
	m := e.digests()[0]
	if m.Title != "Daily digest — 3 items" {
		t.Errorf("title = %q", m.Title)
	}
	mem, other := strings.Index(m.Body, "fact one"), strings.Index(m.Body, "title i1")
	if mem < 0 || other < 0 || mem > other {
		t.Errorf("the suggestion must precede the ordinary items, so that a body cut to a channel's length keeps the fact:\n%s", m.Body)
	}
	if !strings.Contains(m.SessionBody, "title i1") {
		t.Errorf("the session copy lost the ordinary items: %q", m.SessionBody)
	}

	// A digest with no suggestion is exactly what it was: not restricted, no session body.
	e2 := newPrimed(t)
	e2.queue("i1")
	e2.tickAt(bkkAt(1, 8, 0))
	if m := e2.digests()[0]; m.SessionRestricted || m.SessionBody != "" || strings.Contains(m.Body, "Memory suggestions") {
		t.Errorf("an ordinary digest changed: %+v", m)
	}
}

func TestMemoryDigest_AtMostFourSuggestionsAreShownWithButtons(t *testing.T) {
	e := newPrimed(t)
	for _, code := range []string{"AAAA", "BBBB", "CCCC", "DDDD", "EEEE"} {
		e.submit(memCand(code, "fact "+code))
	}
	e.tickAt(bkkAt(1, 8, 0))
	m := e.digests()[0]
	if got := MemorySuggestions(m.Decisions); len(got) != maxDigestMemory {
		t.Fatalf("%d codes, want %d (two buttons each, at most bus.MaxActions in a message)", len(got), maxDigestMemory)
	}
	if shown := strings.Count(m.Body, "\n  > "); shown != maxDigestMemory {
		t.Errorf("%d facts shown, want %d", shown, maxDigestMemory)
	}
	if !strings.Contains(m.Body, "1 more memory suggestions") {
		t.Errorf("the fifth is not announced:\n%s", m.Body)
	}
}

// A candidate that merely claims a memory trigger id without a quote is an ordinary item.
func TestMemoryDigest_NeedsAQuoteToBeASuggestion(t *testing.T) {
	d := Decision{Candidate: proactive.Candidate{TriggerID: proactive.MemoryTriggerPrefix + "AB3K", Title: "t"}}
	if isMemoryItem(d) {
		t.Error("an item with no fact is a suggestion")
	}
	if _, body, sess := renderDigest([]Decision{d}); sess != "" || strings.Contains(body, "Save:") {
		t.Errorf("body %q, session %q", body, sess)
	}
}
