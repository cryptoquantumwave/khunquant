package agent

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// simNow is a settable clock for the prompt tests.
type simNow struct{ t time.Time }

func (s *simNow) now() time.Time { return s.t }

func TestContextBuilder_PromptTimeAndCacheDayUseClock(t *testing.T) {
	tmpDir := setupWorkspace(t, map[string]string{"IDENTITY.md": "# Identity\nTest agent."})
	defer os.RemoveAll(tmpDir)

	clk := &simNow{t: time.Date(2026, 10, 20, 23, 50, 0, 0, time.Local)}
	cb := NewContextBuilder(tmpDir)
	cb.SetClock(clk.now)

	prompt := func() string { return cb.BuildMessages(nil, "", "hi", nil, "telegram", "42")[0].Content }
	if p := prompt(); !strings.Contains(p, "2026-10-20 23:50 (Tuesday)") {
		t.Fatalf("the prompt does not show sim time:\n%s", p)
	}
	builds := func() uint64 {
		cb.systemPromptMutex.RLock()
		defer cb.systemPromptMutex.RUnlock()
		return cb.promptCacheSeq
	}
	cb.BuildSystemPromptWithCache()
	base := builds()
	cb.BuildSystemPromptWithCache()
	if builds() != base {
		t.Fatal("the cached prompt was rebuilt with no change")
	}

	clk.t = clk.t.Add(20 * time.Minute) // sim midnight passes; the wall day does not
	if p := prompt(); !strings.Contains(p, "2026-10-21 00:10 (Wednesday)") {
		t.Fatalf("the prompt time did not roll over:\n%s", p)
	}
	cb.BuildSystemPromptWithCache()
	if builds() != base+1 {
		t.Fatalf("crossing sim midnight rebuilt %d times, want once", builds()-base)
	}
	cb.BuildSystemPromptWithCache()
	if builds() != base+1 {
		t.Fatal("the cache day was invalidated twice")
	}

	cb.SetClock(nil)
	if p := prompt(); strings.Contains(p, "2026-10-21 00:10") {
		t.Fatal("SetClock(nil) did not restore wall time")
	}
}

// A reload builds new agent instances; the loop gives them its clock.
func TestAgentLoop_ClockSurvivesReload(t *testing.T) {
	prov := script(respondResponse(map[string]any{"decision": "silent"}))
	al := proactiveLoop(t, prov)
	clk := &simNow{t: time.Date(2026, 3, 10, 9, 0, 0, 0, time.Local)}
	al.SetClock(clk.now)

	prompt := func() string {
		cb := al.GetRegistry().GetDefaultAgent().ContextBuilder
		return cb.BuildMessages(nil, "", "hi", nil, "telegram", "42")[0].Content
	}
	if p := prompt(); !strings.Contains(p, "2026-03-10 09:00 (Tuesday)") {
		t.Fatalf("the prompt lacks sim time before a reload:\n%s", p)
	}
	cfg := *al.GetConfig()
	if err := al.ReloadProviderAndConfig(context.Background(), script(respondResponse(map[string]any{"decision": "silent"})), &cfg); err != nil {
		t.Fatal(err)
	}
	if p := prompt(); !strings.Contains(p, "2026-03-10 09:00 (Tuesday)") {
		t.Fatalf("the new instances' prompt lacks sim time after a reload:\n%s", p)
	}
}
