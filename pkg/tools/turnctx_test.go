package tools

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/cron"
)

func TestTurnID_RoundTrip(t *testing.T) {
	if got := TurnID(context.Background()); got != "" {
		t.Errorf("TurnID of a bare context = %q, want empty", got)
	}

	ctx := WithTurnID(context.Background(), "turn-1")
	if got := TurnID(ctx); got != "turn-1" {
		t.Errorf("TurnID = %q, want turn-1", got)
	}

	// An empty ID is not a turn: it must not erase the one already there.
	if got := TurnID(WithTurnID(ctx, "")); got != "turn-1" {
		t.Errorf("WithTurnID(ctx, \"\") changed the turn ID to %q", got)
	}

	// The turn ID travels alongside the other turn tags without disturbing them.
	ctx = WithToolSender(WithToolOrigin(ctx, OriginCron), "user-1")
	if TurnID(ctx) != "turn-1" || ToolOrigin(ctx) != OriginCron || ToolSender(ctx) != "user-1" {
		t.Errorf("turn tags interfere: turn=%q origin=%q sender=%q", TurnID(ctx), ToolOrigin(ctx), ToolSender(ctx))
	}
}

func TestNewTurnID_Unique(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 1000; i++ {
		id := NewTurnID()
		if id == "" {
			t.Fatal("NewTurnID returned an empty ID")
		}
		if seen[id] {
			t.Fatalf("NewTurnID returned %q twice", id)
		}
		seen[id] = true
	}
}

// turnRecordingExecutor records the turn ID the cron tool hands to each of the
// two calls it makes for a job.
type turnRecordingExecutor struct {
	processTurnID string
	publishTurnID string
}

func (e *turnRecordingExecutor) ProcessDirectWithChannel(
	ctx context.Context,
	_, _, _, _ string,
	_ bool,
) (string, error) {
	e.processTurnID = TurnID(ctx)
	return "response", nil
}

func (e *turnRecordingExecutor) PublishResponseIfNeeded(ctx context.Context, _, _, _, _ string) {
	e.publishTurnID = TurnID(ctx)
}

// The cron tool decides whether to publish a job's response after the turn has
// returned, by asking what the message tool did during that turn. Both calls
// therefore have to carry the same turn ID, and each job its own - otherwise
// one job's message would suppress another job's response.
func TestCronTool_ExecuteJobRunsEachJobAsItsOwnTurn(t *testing.T) {
	executor := &turnRecordingExecutor{}
	tool := newTestCronToolWithExecutorAndConfig(t, executor, config.DefaultConfig())

	run := func(id string) (string, string) {
		job := &cron.CronJob{ID: id}
		job.Payload.Channel = "telegram"
		job.Payload.To = "chat-1"
		job.Payload.Message = "report"
		if got := tool.ExecuteJob(context.Background(), job); got != "ok" {
			t.Fatalf("ExecuteJob() = %q, want ok", got)
		}
		return executor.processTurnID, executor.publishTurnID
	}

	turn1, publish1 := run("job-1")
	if turn1 == "" {
		t.Fatal("the cron turn was started without a turn ID")
	}
	if publish1 != turn1 {
		t.Errorf("PublishResponseIfNeeded was asked about turn %q, but the job ran as turn %q", publish1, turn1)
	}

	turn2, _ := run("job-2")
	if turn2 == "" || turn2 == turn1 {
		t.Errorf("second job ran as turn %q, first as %q: want distinct, non-empty IDs", turn2, turn1)
	}

	// A caller's own turn ID is replaced, not inherited: a job is a turn of
	// its own even when something that is itself a turn triggers it.
	job := &cron.CronJob{ID: "job-3"}
	job.Payload.Channel = "telegram"
	job.Payload.To = "chat-1"
	job.Payload.Message = "report"
	tool.ExecuteJob(WithTurnID(context.Background(), "outer-turn"), job)
	if executor.processTurnID == "outer-turn" || executor.processTurnID == "" {
		t.Errorf("job ran as turn %q, want a fresh ID of its own", executor.processTurnID)
	}
}

// blockingSpawner holds the spawn until the test lets it go, then reports the
// state of the context it was given.
type blockingSpawner struct {
	release chan struct{}
	ctxErr  chan error
	ctx     chan context.Context
}

func (s *blockingSpawner) SpawnSubTurn(ctx context.Context, _ SubTurnConfig) (*ToolResult, error) {
	<-s.release
	s.ctxErr <- ctx.Err()
	s.ctx <- ctx
	return &ToolResult{ForLLM: "child result"}, nil
}

// An async spawn outlives the tool call that started it, and normally the turn
// too. The agent loop cancels a turn's context when the turn returns; the
// spawn must run on a context that survives that, yet still knows which turn,
// origin and sender it belongs to.
func TestSpawnTool_AsyncSpawnContextSurvivesParentCancel(t *testing.T) {
	spawner := &blockingSpawner{
		release: make(chan struct{}),
		ctxErr:  make(chan error, 1),
		ctx:     make(chan context.Context, 1),
	}
	tool := NewSpawnTool(NewSubagentManager(nil, "test-model", t.TempDir()))
	tool.SetSpawner(spawner)

	parent, cancelParent := context.WithCancel(context.Background())
	parent = WithToolOrigin(parent, OriginCron)
	parent = WithToolSender(parent, "user-1")
	parent = WithTurnID(parent, "turn-1")

	var (
		mu       sync.Mutex
		cbErr    error
		cbResult *ToolResult
	)
	cbDone := make(chan struct{})
	result := tool.ExecuteAsync(parent, map[string]any{"task": "background job"}, func(ctx context.Context, r *ToolResult) {
		mu.Lock()
		cbErr, cbResult = ctx.Err(), r
		mu.Unlock()
		close(cbDone)
	})
	if result == nil || !result.Async {
		t.Fatalf("ExecuteAsync returned %+v, want an async acknowledgement", result)
	}

	// The turn that made the call ends. Only now does the spawn get going.
	cancelParent()
	close(spawner.release)

	select {
	case err := <-spawner.ctxErr:
		if err != nil {
			t.Fatalf("the spawn's context is %v after its parent turn ended; it must outlive the parent", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the spawner was never called")
	}
	spawnCtx := <-spawner.ctx
	if ToolOrigin(spawnCtx) != OriginCron || ToolSender(spawnCtx) != "user-1" || TurnID(spawnCtx) != "turn-1" {
		t.Errorf("the spawn's context lost the turn's tags: origin=%q sender=%q turn=%q",
			ToolOrigin(spawnCtx), ToolSender(spawnCtx), TurnID(spawnCtx))
	}

	select {
	case <-cbDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the completion callback was never called")
	}
	mu.Lock()
	defer mu.Unlock()
	if cbErr != nil {
		t.Errorf("the callback's context is %v; it must outlive the parent turn too", cbErr)
	}
	if cbResult == nil || cbResult.ForLLM != "child result" {
		t.Errorf("callback got %+v, want the child's result", cbResult)
	}
}
