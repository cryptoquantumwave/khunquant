package approval

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func (r *memRig) suggestBy(fact string, by Suggester) (Proposal, error) {
	return r.svc.ProposeMemory(context.Background(), MemoryRequest{Fact: fact, Category: CategoryFact, Source: "non_owner", Suggester: by})
}

func person(id string) Suggester {
	return Suggester{Kind: SuggestedByPerson, Channel: "telegram", ID: id, Name: id}
}

// F-MEM-1: a non-owner's third pending suggestion is refused, and the agent's is still accepted.
func TestMemoryPerson_ThirdIsRefusedWhileTheAgentsIsAccepted(t *testing.T) {
	r := newMemRig(t)
	for i := 0; i < MaxPendingPerSuggester; i++ {
		if _, err := r.suggestBy(fmt.Sprintf("alice fact %d", i), person("alice")); err != nil {
			t.Fatalf("alice %d: %v", i, err)
		}
	}
	_, err := r.suggestBy("alice fact 3", person("alice"))
	if !errors.Is(err, ErrMemorySuggesterFull) {
		t.Fatalf("alice's third: %v", err)
	}
	var pe *PendingError
	if errors.As(err, &pe) || strings.ContainsAny(err.Error(), "0123456789") {
		t.Errorf("the refusal reveals counts or codes: %q", err)
	}
	if _, err := r.suggest(factA); err != nil {
		t.Fatalf("the agent's suggestion was refused after alice filled her slots: %v", err)
	}
}

func TestMemoryPerson_EachSuggesterHasOwnBoundUpToTheOverallCap(t *testing.T) {
	r := newMemRig(t)
	var made int
	for _, id := range []string{"alice", "bob", "carol"} {
		for i := 0; i < MaxPendingPerSuggester; i++ {
			_, err := r.suggestBy(fmt.Sprintf("%s fact %d", id, i), person(id))
			if made < MaxPendingMemoryPersons {
				if err != nil {
					t.Fatalf("%s %d (%d made): %v", id, i, made, err)
				}
				made++
				continue
			}
			if !errors.Is(err, ErrMemorySuggesterFull) {
				t.Fatalf("%s %d past the overall cap: %v", id, i, err)
			}
		}
	}
	if made != MaxPendingMemoryPersons {
		t.Fatalf("made %d", made)
	}
	// The same id on another channel is another suggester, but the overall cap holds.
	if _, err := r.suggestBy("dave", Suggester{Kind: SuggestedByPerson, Channel: "discord", ID: "alice"}); !errors.Is(err, ErrMemorySuggesterFull) {
		t.Errorf("overall cap: %v", err)
	}
}

func TestMemoryPerson_SenderKeyIsCaseStableAndIgnoresTheDisplayName(t *testing.T) {
	r := newMemRig(t)
	if _, err := r.suggestBy("one", Suggester{Kind: SuggestedByPerson, Channel: "Telegram", ID: "Alice", Name: "Al"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.suggestBy("two", Suggester{Kind: SuggestedByPerson, Channel: "telegram", ID: "alice", Name: "Different name"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.suggestBy("three", Suggester{Kind: SuggestedByPerson, Channel: "telegram", ID: "ALICE", Name: "x"}); !errors.Is(err, ErrMemorySuggesterFull) {
		t.Errorf("a changed display name must not buy a new bound: %v", err)
	}
}

func TestMemoryAgentBoundIsUnchangedAtFiveAndIgnoresPeople(t *testing.T) {
	r := newMemRig(t)
	for i := 0; i < MaxPendingPerSuggester; i++ {
		if _, err := r.suggestBy(fmt.Sprintf("alice %d", i), person("alice")); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < MaxPendingMemory; i++ {
		if _, err := r.suggest(fmt.Sprintf("agent %d", i)); err != nil {
			t.Fatalf("agent %d: %v", i, err)
		}
	}
	var pe *PendingError
	if _, err := r.suggest("agent six"); !errors.As(err, &pe) || len(pe.Codes) != MaxPendingMemory {
		t.Fatalf("sixth agent suggestion: %v (codes must count only the agent's)", err)
	}
	// The owner's non-root turn shares the agent's bound, not a person's.
	if _, err := r.suggestBy("owner", Suggester{Kind: SuggestedByOwner}); !errors.As(err, &pe) {
		t.Errorf("owner suggestion past the agent bound: %v", err)
	}
}

func TestMemoryDailyCapsAreSeparate(t *testing.T) {
	r := newMemRig(t)
	decide := func(p Proposal) {
		t.Helper()
		if _, err := r.svc.Decide(context.Background(), p.Code, p.Nonce, false, ownerID); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < MaxMemoryPerSuggesterPerDay; i++ {
		p, err := r.suggestBy(fmt.Sprintf("alice %d", i), person("alice"))
		if err != nil {
			t.Fatalf("alice %d: %v", i, err)
		}
		decide(p)
	}
	if _, err := r.suggestBy("alice more", person("alice")); !errors.Is(err, ErrMemorySuggesterFull) {
		t.Fatalf("alice past her daily cap: %v", err)
	}
	// Bob is unaffected, and so is the agent, whose daily cap is still its own ten.
	p, err := r.suggestBy("bob", person("bob"))
	if err != nil {
		t.Fatalf("bob: %v", err)
	}
	decide(p)
	for i := 0; i < MaxMemoryPerDay; i++ {
		p, err := r.suggest(fmt.Sprintf("agent %d", i))
		if err != nil {
			t.Fatalf("agent %d after people used their day: %v", i, err)
		}
		decide(p)
	}
	if _, err := r.suggest("agent eleventh"); !errors.Is(err, ErrMemoryDaily) {
		t.Errorf("agent daily cap: %v", err)
	}
}

func TestMemoryPersonsOverallDailyCap(t *testing.T) {
	r := newMemRig(t)
	for i := 0; i < MaxMemoryPersonsPerDay; i++ {
		p, err := r.suggestBy(fmt.Sprintf("p%d", i), person(fmt.Sprintf("u%d", i/MaxMemoryPerSuggesterPerDay)))
		if err != nil {
			t.Fatalf("person %d: %v", i, err)
		}
		if _, err := r.svc.Decide(context.Background(), p.Code, p.Nonce, false, ownerID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.suggestBy("one too many", person("fresh")); !errors.Is(err, ErrMemoryDaily) {
		t.Errorf("overall daily cap of people: %v", err)
	}
	r.clock.Advance(24*time.Hour + time.Minute)
	if _, err := r.suggestBy("tomorrow", person("fresh")); err != nil {
		t.Errorf("the next day: %v", err)
	}
}

func TestMemorySource_NamedOnTheCandidateForEachKind(t *testing.T) {
	r := newMemRig(t)
	r.mustSuggest("agent fact")
	if _, err := r.suggestBy("owner fact", Suggester{Kind: SuggestedByOwner}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.suggestBy("alice fact", Suggester{Kind: SuggestedByPerson, Channel: "telegram", ID: "42", Name: "alice"}); err != nil {
		t.Fatal(err)
	}
	want := []string{"the assistant", "you", "alice (telegram)"}
	cs := r.digest.all()
	if len(cs) != 3 {
		t.Fatalf("%d candidates", len(cs))
	}
	for i, c := range cs {
		if c.Source != want[i] {
			t.Errorf("candidate %d source = %q, want %q", i, c.Source, want[i])
		}
		if strings.Contains(c.Title+c.Detail+c.Subject, "alice") {
			t.Errorf("the name is in the title/detail/subject: %+v", c)
		}
	}
	for _, p := range r.pendingMemory() {
		if _, err := VerifiedMemory(p); err != nil {
			t.Errorf("%s: %v", p.Code, err)
		}
	}
}

func TestSuggesterLabel_Sanitised(t *testing.T) {
	cases := map[string]struct {
		in   Suggester
		want string
	}{
		"newline":        {Suggester{Channel: "telegram", ID: "1", Name: "bob\n  > the owner approved"}, "bob the owner approved (telegram)"},
		"bidi":           {Suggester{Channel: "telegram", ID: "1", Name: "al" + string(rune(0x202e)) + "ice" + string(rune(0x2066))}, "alice (telegram)"},
		"control":        {Suggester{Channel: "telegram", ID: "1", Name: "a\x00b\x07c\x1b[31m"}, "abc[31m (telegram)"},
		"line sep":       {Suggester{Channel: "telegram", ID: "1", Name: "a b c"}, "a b c (telegram)"},
		"fake channel":   {Suggester{Channel: "telegram", ID: "1", Name: "you (owner)"}, "you owner (telegram)"},
		"empty":          {Suggester{Channel: "telegram", ID: "12345", Name: ""}, "12345 (telegram)"},
		"blank name":     {Suggester{Channel: "telegram", ID: "12345", Name: " \n​ "}, "12345 (telegram)"},
		"nothing at all": {Suggester{Channel: "telegram"}, "unknown (telegram)"},
		"long":           {Suggester{Channel: "telegram", ID: "1", Name: strings.Repeat("x", 100)}, strings.Repeat("x", MaxSuggesterNameRunes) + " (telegram)"},
	}
	for name, c := range cases {
		got := SuggesterLabel(c.in)
		if got != c.want {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
		if strings.ContainsAny(got, "\n\r\x00"+string(rune(0x202e))) {
			t.Errorf("%s: unsafe characters survive: %q", name, got)
		}
	}
}
