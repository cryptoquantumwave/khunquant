package agent

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/providers"
)

// fakeSection is a background section with a scripted answer.
type fakeSection struct {
	text      string
	err       error
	block     bool
	panics    bool
	sections  atomic.Int32
	completed atomic.Int32
}

func (f *fakeSection) Name() string { return "changes" }
func (f *fakeSection) Close() error { return nil }
func (f *fakeSection) Section(ctx context.Context) (string, error) {
	f.sections.Add(1)
	if f.panics {
		panic("boom")
	}
	if f.block {
		<-ctx.Done()
		return "", ctx.Err()
	}
	return f.text, f.err
}
func (f *fakeSection) TurnCompleted(context.Context) { f.completed.Add(1) }

const sectionText = "## Changes since your last look\n- Portfolio total (USD): 1,000 -> 1,100 (+10.0%)\n"

func dynamicOf(msgs []providers.Message) string {
	if len(msgs) == 0 || len(msgs[0].SystemParts) < 2 {
		return ""
	}
	return msgs[0].SystemParts[1].Text
}

func buildFor(cb *ContextBuilder, mode PromptMode) []providers.Message {
	return cb.BuildMessagesWithOptions(nil, "", "hello", nil, "telegram", "chat42", BuildOptions{Mode: mode, OfferLoadSkill: true})
}

// TestBackgroundSection_ReachesBackgroundModesOnly: the full prompt of a user turn
// is byte-identical with and without a section attached; the light and the
// proactive prompt carry it.
func TestBackgroundSection_ReachesBackgroundModesOnly(t *testing.T) {
	cb, workspace := goldenFixture(t)
	plain := map[PromptMode][]byte{}
	static := map[PromptMode]string{}
	for _, m := range []PromptMode{PromptFull, PromptLight, PromptProactive} {
		plain[m] = goldenSnapshot(t, buildFor(cb, m), workspace)
		static[m] = buildFor(cb, m)[0].SystemParts[0].Text
	}

	holder := &backgroundSections{}
	cb.WithBackgroundSections(holder)
	// Attached but empty: nothing changes anywhere.
	for m, want := range plain {
		if got := goldenSnapshot(t, buildFor(cb, m), workspace); string(got) != string(want) {
			t.Fatalf("mode %d changed with an empty holder", m)
		}
	}

	sec := &fakeSection{text: sectionText}
	holder.cur.Store(&sectionHolder{s: sec})

	if got := goldenSnapshot(t, buildFor(cb, PromptFull), workspace); string(got) != string(plain[PromptFull]) {
		t.Fatalf("the full prompt of a user turn changed:\n%s", got)
	}
	if n := sec.sections.Load(); n != 0 {
		t.Fatalf("a user turn asked the section for text (%d times)", n)
	}
	for _, m := range []PromptMode{PromptLight, PromptProactive} {
		msgs := buildFor(cb, m)
		dyn := dynamicOf(msgs)
		if !strings.Contains(dyn, sectionText) {
			t.Fatalf("mode %d: the dynamic context lacks the section:\n%s", m, dyn)
		}
		if !strings.Contains(dyn, "## Current Time") {
			t.Fatalf("mode %d: the usual dynamic context is gone", m)
		}
		// Only the dynamic block grows: the static, cached prefix is untouched.
		if msgs[0].SystemParts[0].Text != static[m] {
			t.Fatalf("mode %d: the static prompt changed", m)
		}
		if !strings.Contains(msgs[0].Content, sectionText) {
			t.Fatalf("mode %d: the concatenated content lacks the section", m)
		}
	}
}

func TestBackgroundSection_FailuresCostTheRequestNothing(t *testing.T) {
	prev := backgroundSectionTimeout
	backgroundSectionTimeout = 30 * time.Millisecond
	t.Cleanup(func() { backgroundSectionTimeout = prev })

	cb, workspace := goldenFixture(t)
	baseline := goldenSnapshot(t, buildFor(cb, PromptLight), workspace)
	holder := &backgroundSections{}
	cb.WithBackgroundSections(holder)
	for name, sec := range map[string]*fakeSection{
		"error":   {err: errors.New("db locked")},
		"panic":   {panics: true},
		"blocked": {block: true},
		"empty":   {},
	} {
		holder.cur.Store(&sectionHolder{s: sec})
		start := time.Now()
		got := goldenSnapshot(t, buildFor(cb, PromptLight), workspace)
		if string(got) != string(baseline) {
			t.Fatalf("%s: the prompt changed", name)
		}
		if time.Since(start) > 2*time.Second {
			t.Fatalf("%s: the render took %v", name, time.Since(start))
		}
	}
}

func TestBackgroundSection_BudgetEstimateOnlyChangesForBackgroundModes(t *testing.T) {
	cb, _ := goldenFixture(t)
	holder := &backgroundSections{}
	cb.WithBackgroundSections(holder)
	est := func(m PromptMode) int { return cb.EstimateSystemTokensFor(BuildOptions{Mode: m}, "") }
	full, light, proactiveEst := est(PromptFull), est(PromptLight), est(PromptProactive)

	holder.cur.Store(&sectionHolder{s: &fakeSection{text: sectionText}})
	if est(PromptFull) != full {
		t.Fatal("the user-turn estimate moved")
	}
	if got := est(PromptLight); got != light+backgroundSectionChars*2/5 {
		t.Fatalf("light estimate %d, want %d", got, light+backgroundSectionChars*2/5)
	}
	if got := est(PromptProactive); got != proactiveEst+backgroundSectionChars*2/5 {
		t.Fatalf("proactive estimate %d", got)
	}
}

// TestBackgroundSection_ProactiveTurnCarriesItAndReportsCompletion drives a real
// proactive turn and a user-prompt build through an agent loop, then reloads the
// loop's configuration and does it again: the holder belongs to the loop, so it
// outlives the registry swap.
func TestBackgroundSection_ProactiveTurnCarriesItAndReportsCompletion(t *testing.T) {
	prov := script(respondResponse(map[string]any{"decision": "silent"}))
	al := proactiveLoop(t, prov)
	sec := &fakeSection{text: sectionText}
	detach := al.SetBackgroundSection(sec)

	mustRun(t, al, proactiveReq(cand("c1", proactive.SeverityNotice, false)))
	if !strings.Contains(prov.msgs[0][0].Content, sectionText) {
		t.Fatalf("the proactive turn's system prompt lacks the section:\n%s", prov.msgs[0][0].Content)
	}
	if sec.completed.Load() != 1 {
		t.Fatalf("TurnCompleted called %d times, want once", sec.completed.Load())
	}
	// A user prompt built by the same agent does not carry it.
	agent := al.GetRegistry().GetDefaultAgent()
	user := agent.ContextBuilder.BuildMessages(nil, "", "hi", nil, "telegram", "42")
	if strings.Contains(user[0].Content, "Changes since your last look") {
		t.Fatal("a user turn carries the section")
	}

	// A reload installs a new registry with new context builders.
	cfg := *al.GetConfig()
	prov2 := script(respondResponse(map[string]any{"decision": "silent"}))
	if err := al.ReloadProviderAndConfig(context.Background(), prov2, &cfg); err != nil {
		t.Fatal(err)
	}
	mustRun(t, al, proactiveReq(cand("c2", proactive.SeverityNotice, false)))
	if !strings.Contains(prov2.msgs[0][0].Content, sectionText) {
		t.Fatal("the section did not survive a reload")
	}
	if sec.completed.Load() != 2 {
		t.Fatalf("TurnCompleted = %d", sec.completed.Load())
	}

	// A turn that fails is not a completed turn.
	failing := &scriptProvider{fn: func(context.Context, int, []providers.Message, string) (*providers.LLMResponse, error) {
		return nil, errors.New("provider down")
	}}
	if err := al.ReloadProviderAndConfig(context.Background(), failing, &cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := al.RunProactiveTurn(context.Background(), proactiveReq(cand("c3", proactive.SeverityNotice, false))); err == nil {
		t.Fatal("expected the failing provider to fail the turn")
	}
	if sec.completed.Load() != 2 {
		t.Fatalf("a failed turn was reported as completed (%d)", sec.completed.Load())
	}

	// Detaching removes it; the detach of a stale generation does not.
	other := &fakeSection{text: "## other\n"}
	detachOther := al.SetBackgroundSection(other)
	detach() // the first generation's detach must not remove the second's section
	if h := al.background.cur.Load(); h == nil || h.s != other {
		t.Fatal("a stale detach removed the current section")
	}
	detachOther()
	if al.background.attached() {
		t.Fatal("still attached")
	}
}

func TestBackgroundSection_NilAndZeroLoopsAreSafe(t *testing.T) {
	var al *AgentLoop
	al.SetBackgroundSection(&fakeSection{})() // no panic
	zero := &AgentLoop{}
	zero.SetBackgroundSection(&fakeSection{})()
	var b *backgroundSections
	if b.attached() || b.render(context.Background()) != "" {
		t.Fatal("nil holder")
	}
	b.turnCompleted(context.Background())
}
