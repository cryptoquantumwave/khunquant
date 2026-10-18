package attention

import (
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

func reflCand(code, change, why string) proactive.Candidate {
	return cand("reflection:"+code, info, func(c *proactive.Candidate) {
		c.TriggerID = proactive.ReflectionTriggerPrefix + code
		c.Subject = "reflection"
		c.Title = "Suggestion " + code + " (trigger_param)"
		c.Quote = change
		c.Evidence = map[string]any{"why": why}
		c.Trust = proactive.TrustUntrusted
	})
}

func TestReflectionDigest_ChangeAndWhyForTheOwnerNothingForTheSession(t *testing.T) {
	e := newPrimed(t)
	e.queue("i1")
	e.submit(reflCand("ZX9Q", "Change floor_th_pct of trigger pf-move_vs_vol (move_vs_vol) from 3 to 5", "You dismissed 4 of 4 SET alerts."))
	e.submit(memCand("AB3K", "I rebalance monthly"))
	e.tickAt(bkkAt(1, 8, 0))
	m := e.digests()[0]
	for _, want := range []string{"Suggested changes - made only if you approve them:", "> Change floor_th_pct of trigger pf-move_vs_vol (move_vs_vol) from 3 to 5",
		"Why: You dismissed 4 of 4 SET alerts.", "/approve ZX9Q", "/reject ZX9Q", "Memory suggestions"} {
		if !strings.Contains(m.Body, want) {
			t.Errorf("the body lacks %q:\n%s", want, m.Body)
		}
	}
	if got := ReflectionSuggestions(m.Decisions); len(got) != 1 || got[0].Code != "ZX9Q" || !strings.HasPrefix(got[0].Change, "Change floor_th_pct") {
		t.Errorf("ReflectionSuggestions = %+v", got)
	}
	if !m.SessionRestricted {
		t.Fatal("the session copy is not restricted")
	}
	for _, leak := range []string{"floor_th_pct", "dismissed", "rebalance"} {
		if strings.Contains(m.SessionBody, leak) || strings.Contains(m.Title, leak) {
			t.Errorf("%q reached the session copy or the title", leak)
		}
	}
	if !strings.Contains(m.SessionBody, "title i1") || !strings.Contains(m.SessionBody, SessionWithheld) {
		t.Errorf("session body = %q", m.SessionBody)
	}
}

func TestReflectionDigest_ShowsEveryPendingSuggestionAndIgnoresAnItemWithNoChange(t *testing.T) {
	e := newPrimed(t)
	for _, code := range []string{"AAAA", "BBBB", "CCCC", "DDDD", "EEEE", "FFFF"} {
		e.submit(reflCand(code, "Mute alerts starting \"x:\" for 7 days", "noisy"))
	}
	e.tickAt(bkkAt(1, 8, 0))
	if got := ReflectionSuggestions(e.digests()[0].Decisions); len(got) != maxDigestReflection {
		t.Errorf("%d suggestions shown, want %d", len(got), maxDigestReflection)
	}
	d := Decision{Candidate: proactive.Candidate{TriggerID: proactive.ReflectionTriggerPrefix + "AB3K", Title: "t"}}
	if isReflectionItem(d) {
		t.Error("an item with no change is a suggestion")
	}
	if got := renderReflectionBlock([]Decision{d}, true); got != "" {
		t.Errorf("block = %q", got)
	}
}
