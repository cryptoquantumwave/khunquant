package tools

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/cron"
)

// TestExecuteJob_AgentErrorRecordedAsError drives a real cron.CronService with a
// CronTool as its handler, the way the gateway wires them: an agent failure must
// land in the job's state as an error, and nothing may be pushed to the chat.
func TestExecuteJob_AgentErrorRecordedAsError(t *testing.T) {
	executor := &stubJobExecutor{
		response: "this response must not be published",
		err:      errors.New("agent failure"),
	}
	tool := newTestCronToolWithExecutorAndConfig(t, executor, config.DefaultConfig())
	svc := tool.cronService
	svc.SetOnJobCtx(func(ctx context.Context, job *cron.CronJob) (string, error) {
		return tool.ExecuteJob(ctx, job)
	})

	job, err := svc.AddJob("agent-job", cron.CronSchedule{Kind: "every", EveryMS: int64Ptr(3_600_000)},
		"do something", false, "telegram", "chat-1")
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	if err := svc.RunJobNow(job.ID); err != nil {
		t.Fatalf("RunJobNow: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		st := svc.GetJob(job.ID).State
		if !st.Running && st.LastStatus != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the job to finish")
		}
		time.Sleep(time.Millisecond)
	}

	st := svc.GetJob(job.ID).State
	if st.LastStatus != "error" {
		t.Errorf("LastStatus = %q, want error", st.LastStatus)
	}
	if !strings.Contains(st.LastError, "agent failure") {
		t.Errorf("LastError = %q, want it to carry the agent failure", st.LastError)
	}
	if executor.publishedResp != "" {
		t.Errorf("agent failure must not publish a response, got %q", executor.publishedResp)
	}
	select {
	case msg := <-tool.msgBus.OutboundChan():
		t.Errorf("nothing may be pushed to the chat on failure, got %+v", msg)
	default:
	}
}

func TestCronTool_ExecuteJobPublishFailureIsAnError(t *testing.T) {
	tool := newTestCronTool(t)
	// A bus that is already closed cannot accept the direct-delivery message.
	tool.msgBus = bus.NewMessageBus()
	tool.msgBus.Close()

	job := &cron.CronJob{ID: "deliver"}
	job.Payload.Channel = "telegram"
	job.Payload.To = "chat-1"
	job.Payload.Message = "static reminder"
	job.Payload.Deliver = true

	if got, err := tool.ExecuteJob(context.Background(), job); err == nil {
		t.Fatalf("ExecuteJob() = %q, nil; a failed delivery must be reported as an error", got)
	}
}

func TestCronTool_SessionKeyUsesConfiguredDefaultAgent(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Agents.List = []config.AgentConfig{{ID: "alpha"}, {ID: "Beta", Default: true}}
	executor := &stubJobExecutor{response: "ok"}
	tool := newTestCronToolWithExecutorAndConfig(t, executor, cfg)

	job := &cron.CronJob{ID: "j9"}
	job.Payload.Message = "hi"
	if _, err := tool.ExecuteJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if executor.lastKey != "agent:beta:cron:j9" {
		t.Errorf("sessionKey = %q, want agent:beta:cron:j9", executor.lastKey)
	}
	if !strings.HasPrefix(executor.lastKey, "agent:") {
		t.Error("the agent: prefix is what makes the agent loop honour the key")
	}
}

func TestCronTool_NoHistoryIsForcedForCronTurns(t *testing.T) {
	for _, payloadNoHistory := range []bool{false, true} {
		executor := &stubJobExecutor{response: "ok"}
		tool := newTestCronToolWithExecutorAndConfig(t, executor, config.DefaultConfig())
		job := &cron.CronJob{ID: "j"}
		job.Payload.Message = "hi"
		job.Payload.NoHistory = payloadNoHistory
		if _, err := tool.ExecuteJob(context.Background(), job); err != nil {
			t.Fatal(err)
		}
		if !executor.lastNoHistory {
			t.Errorf("Payload.NoHistory=%v: the stable per-job session must never load history", payloadNoHistory)
		}
	}
}

// gateExecutor lets a test hold agent turns open and observe their concurrency.
type gateExecutor struct {
	stubJobExecutor
	current atomic.Int32
	peak    atomic.Int32
	calls   atomic.Int32
	entered chan string
	gates   map[string]chan struct{}
	mu      sync.Mutex
}

func (g *gateExecutor) ProcessDirectWithChannel(
	_ context.Context,
	content, _, _, _ string,
	_ bool,
) (string, error) {
	g.calls.Add(1)
	n := g.current.Add(1)
	defer g.current.Add(-1)
	for {
		p := g.peak.Load()
		if n <= p || g.peak.CompareAndSwap(p, n) {
			break
		}
	}
	g.entered <- content
	g.mu.Lock()
	gate := g.gates[content]
	g.mu.Unlock()
	<-gate
	return "", nil
}

func TestOnlyOneLLMJobAtATime(t *testing.T) {
	exec := &gateExecutor{
		entered: make(chan string, 4),
		gates:   map[string]chan struct{}{"A": make(chan struct{}), "B": make(chan struct{})},
	}
	tool := newTestCronToolWithExecutorAndConfig(t, exec, config.DefaultConfig())

	run := func(ctx context.Context, msg string) <-chan error {
		done := make(chan error, 1)
		job := &cron.CronJob{ID: "job-" + msg}
		job.Payload.Message = msg
		go func() {
			_, err := tool.ExecuteJob(ctx, job)
			done <- err
		}()
		return done
	}
	recv := func(ch <-chan string, what string) string {
		t.Helper()
		select {
		case v := <-ch:
			return v
		case <-time.After(10 * time.Second):
			t.Fatalf("timed out waiting for %s", what)
			return ""
		}
	}

	doneA := run(context.Background(), "A")
	if got := recv(exec.entered, "job A to reach the agent"); got != "A" {
		t.Fatalf("first agent turn = %q, want A", got)
	}

	doneB := run(context.Background(), "B")
	// B must be parked on the semaphore, not in the agent.
	select {
	case got := <-exec.entered:
		t.Fatalf("job %q reached the agent while another LLM job was running", got)
	case <-time.After(50 * time.Millisecond):
	}

	// A third job gives up when its context is cancelled while waiting, without
	// ever reaching the agent.
	ctxC, cancelC := context.WithCancel(context.Background())
	doneC := run(ctxC, "C")
	cancelC()
	select {
	case err := <-doneC:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("waiting job error = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a cancelled waiter must return promptly")
	}

	close(exec.gates["A"])
	if err := <-doneA; err != nil {
		t.Errorf("job A: %v", err)
	}
	if got := recv(exec.entered, "job B to reach the agent after A finished"); got != "B" {
		t.Fatalf("second agent turn = %q, want B", got)
	}
	close(exec.gates["B"])
	if err := <-doneB; err != nil {
		t.Errorf("job B: %v", err)
	}

	if peak := exec.peak.Load(); peak != 1 {
		t.Errorf("peak concurrent agent turns = %d, want 1", peak)
	}
	if calls := exec.calls.Load(); calls != 2 {
		t.Errorf("agent called %d times, want 2 (the cancelled job must not run)", calls)
	}
	if len(llmJobSem) != 0 {
		t.Error("the LLM slot must be released after every job")
	}
}

// resettingExecutor adds the optional ResetSession hook to the stub.
type resettingExecutor struct {
	stubJobExecutor
	resets   []string
	resetErr error
}

func (r *resettingExecutor) ResetSession(_ context.Context, sessionKey string) error {
	r.resets = append(r.resets, sessionKey)
	return r.resetErr
}

func TestCronTool_ResetsSessionOncePerAgentRun(t *testing.T) {
	exec := &resettingExecutor{stubJobExecutor: stubJobExecutor{response: "reply"}}
	tool := newTestCronToolWithExecutorAndConfig(t, exec, config.DefaultConfig())

	agentJob := &cron.CronJob{ID: "agentic"}
	agentJob.Payload.Channel = "telegram"
	agentJob.Payload.To = "chat-1"
	agentJob.Payload.Message = "report"
	for i := 0; i < 2; i++ {
		if _, err := tool.ExecuteJob(context.Background(), agentJob); err != nil {
			t.Fatal(err)
		}
	}
	if len(exec.resets) != 2 || exec.resets[0] != "agent:main:cron:agentic" || exec.resets[1] != exec.resets[0] {
		t.Fatalf("resets = %v, want one reset of agent:main:cron:agentic per agent run", exec.resets)
	}
	if exec.lastKey != exec.resets[0] {
		t.Errorf("the turn ran in %q but %q was reset", exec.lastKey, exec.resets[0])
	}

	// Deliver and command jobs never reach the agent, so they never reset.
	before := len(exec.resets)
	deliver := &cron.CronJob{ID: "deliver"}
	deliver.Payload.Channel, deliver.Payload.To = "cli", "direct"
	deliver.Payload.Message, deliver.Payload.Deliver = "static", true
	command := &cron.CronJob{ID: "command"}
	command.Payload.Channel, command.Payload.To = "cli", "direct"
	command.Payload.Command = "echo hi"
	for _, j := range []*cron.CronJob{deliver, command} {
		if _, err := tool.ExecuteJob(context.Background(), j); err != nil {
			t.Fatalf("%s: %v", j.ID, err)
		}
	}
	if len(exec.resets) != before {
		t.Errorf("deliver/command jobs reset a session: %v", exec.resets[before:])
	}
}

func TestCronTool_ResetFailureDoesNotFailTheJob(t *testing.T) {
	exec := &resettingExecutor{stubJobExecutor: stubJobExecutor{response: "reply"}, resetErr: errors.New("disk full")}
	tool := newTestCronToolWithExecutorAndConfig(t, exec, config.DefaultConfig())
	job := &cron.CronJob{ID: "j"}
	job.Payload.Message = "report"
	if got, err := tool.ExecuteJob(context.Background(), job); err != nil || got != "ok" {
		t.Fatalf("ExecuteJob() = %q, %v; a reset failure must only be logged", got, err)
	}
	if exec.publishedResp != "reply" {
		t.Error("the run must continue and publish after a failed reset")
	}
}
