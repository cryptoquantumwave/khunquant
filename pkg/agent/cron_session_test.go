package agent

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/cron"
	"github.com/cryptoquantumwave/khunquant/pkg/providers"
	"github.com/cryptoquantumwave/khunquant/pkg/routing"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

func historyText(msgs []providers.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		b.WriteString(m.Role + ": " + m.Content + "\n")
	}
	return b.String()
}

// TestCronTurn_UsesIsolatedSession runs a scheduled job through the real
// CronTool and agent loop and checks the three promises of the isolated cron
// session: the main conversation is never read, never written, and successive
// runs of a job do not see each other.
func TestCronTurn_UsesIsolatedSession(t *testing.T) {
	prov := &charScriptedProvider{responses: []providers.LLMResponse{
		{Content: "main reply"},
		{Content: "cron reply 1"},
		{Content: "cron reply 2"},
		{Content: "main reply 2"},
	}}
	al, cleanup := newCharLoop(t, prov)
	defer cleanup()
	agent := al.GetRegistry().GetDefaultAgent()
	mainKey := routing.BuildAgentMainSessionKey(agent.ID)

	// A conversation exists in the main session first.
	ctx := context.Background()
	if _, err := al.ProcessDirectWithChannel(ctx, "main question", "", "cli", "direct", false); err != nil {
		t.Fatalf("main turn: %v", err)
	}
	mainBefore := agent.Sessions.GetHistory(mainKey)
	if len(mainBefore) != 2 {
		t.Fatalf("setup: main session has %d messages, want 2 (the key %q must be the routed main session)",
			len(mainBefore), mainKey)
	}

	cronSvc := cron.NewCronService(filepath.Join(t.TempDir(), "jobs.json"), nil)
	tool, err := tools.NewCronTool(cronSvc, al, bus.NewMessageBus(), t.TempDir(), true, 0, al.GetConfig())
	if err != nil {
		t.Fatalf("NewCronTool: %v", err)
	}
	job := &cron.CronJob{ID: "job-iso"}
	job.Payload.Channel = "cli"
	job.Payload.To = "direct"
	job.Payload.Message = "scheduled prompt one"

	if _, err := tool.ExecuteJob(ctx, job); err != nil {
		t.Fatalf("cron run 1: %v", err)
	}
	job.Payload.Message = "scheduled prompt two"
	if _, err := tool.ExecuteJob(ctx, job); err != nil {
		t.Fatalf("cron run 2: %v", err)
	}

	// 1. The main session is untouched.
	mainAfter := agent.Sessions.GetHistory(mainKey)
	if historyText(mainAfter) != historyText(mainBefore) {
		t.Errorf("a cron turn changed the main session:\nbefore:\n%s\nafter:\n%s",
			historyText(mainBefore), historyText(mainAfter))
	}

	// 2. The cron turn went to its own job-scoped session.
	cronKey := "agent:" + agent.ID + ":cron:job-iso"
	cronHist := historyText(agent.Sessions.GetHistory(cronKey))
	if !strings.Contains(cronHist, "scheduled prompt two") || !strings.Contains(cronHist, "cron reply 2") ||
		strings.Contains(cronHist, "scheduled prompt one") {
		t.Errorf("cron session %q does not hold exactly the latest cron turn:\n%s", cronKey, cronHist)
	}

	// 3. The model never saw the main conversation, nor the previous cron run.
	if len(prov.gotMsgs) != 3 {
		t.Fatalf("provider saw %d calls, want 3", len(prov.gotMsgs))
	}
	run1 := historyText(prov.gotMsgs[1])
	run2 := historyText(prov.gotMsgs[2])
	if !strings.Contains(run1, "scheduled prompt one") || !strings.Contains(run2, "scheduled prompt two") {
		t.Fatalf("cron prompts missing from model input:\n%s\n---\n%s", run1, run2)
	}
	for i, in := range []string{run1, run2} {
		if strings.Contains(in, "main question") || strings.Contains(in, "main reply") {
			t.Errorf("cron run %d read the main conversation:\n%s", i+1, in)
		}
	}
	if strings.Contains(run2, "scheduled prompt one") || strings.Contains(run2, "cron reply 1") {
		t.Errorf("cron run 2 read run 1's history (history must not accumulate):\n%s", run2)
	}

	// The next main-session turn does not see any cron output either.
	if _, err := al.ProcessDirectWithChannel(ctx, "main again", "", "cli", "direct", false); err != nil {
		t.Fatalf("main turn 2: %v", err)
	}
	mainInput := historyText(prov.gotMsgs[3])
	if strings.Contains(mainInput, "scheduled prompt") || strings.Contains(mainInput, "cron reply") {
		t.Errorf("cron output leaked into the main conversation:\n%s", mainInput)
	}
}

// TestCronTurn_SessionHoldsOnlyLatestRun: the stable per-job session is emptied
// before every run, so after three runs it holds run 3 and nothing else, and
// the main conversation is never touched.
func TestCronTurn_SessionHoldsOnlyLatestRun(t *testing.T) {
	prov := &charScriptedProvider{responses: []providers.LLMResponse{
		{Content: "main reply"},
		{Content: "cron reply 1"},
		{Content: "cron reply 2"},
		{Content: "cron reply 3"},
	}}
	al, cleanup := newCharLoop(t, prov)
	defer cleanup()
	agent := al.GetRegistry().GetDefaultAgent()
	mainKey := routing.BuildAgentMainSessionKey(agent.ID)
	ctx := context.Background()

	if _, err := al.ProcessDirectWithChannel(ctx, "main question", "", "cli", "direct", false); err != nil {
		t.Fatalf("main turn: %v", err)
	}
	mainBefore := historyText(agent.Sessions.GetHistory(mainKey))

	cronSvc := cron.NewCronService(filepath.Join(t.TempDir(), "jobs.json"), nil)
	tool, err := tools.NewCronTool(cronSvc, al, bus.NewMessageBus(), t.TempDir(), true, 0, al.GetConfig())
	if err != nil {
		t.Fatalf("NewCronTool: %v", err)
	}
	job := &cron.CronJob{ID: "job-latest"}
	job.Payload.Channel = "cli"
	job.Payload.To = "direct"
	cronKey := "agent:" + agent.ID + ":cron:job-latest"

	for i := 1; i <= 3; i++ {
		job.Payload.Message = fmt.Sprintf("scheduled prompt %d", i)
		if _, err := tool.ExecuteJob(ctx, job); err != nil {
			t.Fatalf("cron run %d: %v", i, err)
		}
		hist := agent.Sessions.GetHistory(cronKey)
		got := historyText(hist)
		if !strings.Contains(got, fmt.Sprintf("scheduled prompt %d", i)) ||
			!strings.Contains(got, fmt.Sprintf("cron reply %d", i)) {
			t.Fatalf("after run %d the cron session lacks that run:\n%s", i, got)
		}
		if len(hist) != 2 {
			t.Errorf("after run %d the cron session holds %d messages, want only that run's 2:\n%s", i, len(hist), got)
		}
		for j := 1; j < i; j++ {
			if strings.Contains(got, fmt.Sprintf("prompt %d", j)) || strings.Contains(got, fmt.Sprintf("reply %d", j)) {
				t.Errorf("after run %d the cron session still holds run %d:\n%s", i, j, got)
			}
		}
	}

	if got := historyText(agent.Sessions.GetHistory(mainKey)); got != mainBefore {
		t.Errorf("cron runs changed the main session:\nbefore:\n%s\nafter:\n%s", mainBefore, got)
	}
}

// ResetSession must serialise with a turn through the session lock and give up
// when its context ends while waiting.
func TestResetSession_WaitsForTheSessionLock(t *testing.T) {
	al, cleanup := newCharLoop(t, &charScriptedProvider{})
	defer cleanup()
	agent := al.GetRegistry().GetDefaultAgent()
	key := "agent:main:cron:locked"
	agent.Sessions.AddMessage(key, "user", "old")

	held := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- al.WithSessionLock(key, func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := al.ResetSession(ctx, key); err == nil {
		t.Error("ResetSession must give up when its context ends while the lock is held")
	}
	if len(agent.Sessions.GetHistory(key)) != 1 {
		t.Error("a reset that could not take the lock must not touch the session")
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := al.ResetSession(context.Background(), key); err != nil {
		t.Fatalf("ResetSession: %v", err)
	}
	if n := len(agent.Sessions.GetHistory(key)); n != 0 {
		t.Errorf("history has %d messages after reset, want 0", n)
	}
}
