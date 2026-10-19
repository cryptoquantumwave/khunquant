package agent

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/cognition"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/providers"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex/sqlitextest"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

// countingScheduler wraps a scheduler and records every job submitted to it.
type countingScheduler struct {
	inner turnScheduler
	mu    sync.Mutex
	jobs  []cognition.Job
}

func (c *countingScheduler) Submit(ctx context.Context, j cognition.Job) (<-chan cognition.Result, error) {
	c.mu.Lock()
	c.jobs = append(c.jobs, j)
	c.mu.Unlock()
	return c.inner.Submit(ctx, j)
}

func (c *countingScheduler) submitted() []cognition.Job {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]cognition.Job(nil), c.jobs...)
}

func cogCfg(workers int) config.CognitionConfig {
	return config.CognitionConfig{Enabled: true, MaxConcurrentLLM: workers, WakesPerHour: 1000, TokensPerDay: 10_000_000}
}

// startScheduler builds a running scheduler on a fresh database, stopped at cleanup.
func startScheduler(t *testing.T, cfg config.CognitionConfig) (*cognition.Scheduler, context.CancelFunc) {
	t.Helper()
	s, err := cognition.New(cfg, sqlitextest.Open(t), nil, nil)
	if err != nil {
		t.Fatalf("cognition.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = s.Run(ctx) }()
	stop := func() {
		cancel()
		select {
		case <-done:
		case <-time.After(testWait):
			t.Error("scheduler did not stop")
		}
	}
	t.Cleanup(stop)
	return s, stop
}

// scheduledOwnerLoop is an owner-configured loop with a scheduler installed
// behind a counting wrapper.
func scheduledOwnerLoop(
	t *testing.T, prov providers.LLMProvider, cfg config.CognitionConfig,
) (*AgentLoop, *cognition.Scheduler, *countingScheduler) {
	t.Helper()
	al, _ := ownerLoop(t, prov)
	sched, _ := startScheduler(t, cfg)
	cs := &countingScheduler{inner: sched}
	al.sched.Store(&schedulerHolder{s: cs})
	return al, sched, cs
}

func runOwnerLoop(t *testing.T, al *AgentLoop) (context.Context, func()) {
	t.Helper()
	return runLoop(t, al)
}

func inboundTo(chatID, content string) bus.InboundMessage {
	return bus.InboundMessage{Channel: "telegram", ChatID: chatID, SenderID: "user-1", Content: content}
}

func publishMsg(t *testing.T, al *AgentLoop, msg bus.InboundMessage) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	if err := al.bus.PublishInbound(ctx, msg); err != nil {
		t.Fatalf("publish inbound: %v", err)
	}
}

// ====================== routing ======================

// A fake scheduler that records the job and finishes it without running it: the
// message has been routed and its session key resolved by the time it is
// submitted, and the model has not been asked anything.
type recordingScheduler struct {
	jobs chan cognition.Job
}

func (r *recordingScheduler) Submit(_ context.Context, j cognition.Job) (<-chan cognition.Result, error) {
	r.jobs <- j
	ch := make(chan cognition.Result, 1)
	ch <- cognition.Result{}
	close(ch)
	return ch, nil
}

func TestRun_ResolvesSessionBeforeSubmit(t *testing.T) {
	prov := newRoutedProvider().on("hello", func(context.Context, int, []providers.Message) (*providers.LLMResponse, error) {
		return textResponse("hi")
	})
	al, _ := ownerLoop(t, prov)
	rec := &recordingScheduler{jobs: make(chan cognition.Job, 4)}
	al.sched.Store(&schedulerHolder{s: rec})
	_, stop := runOwnerLoop(t, al)
	defer stop()

	msg := dmFrom("424242", "hello")
	publishMsg(t, al, msg)

	var job cognition.Job
	select {
	case job = <-rec.jobs:
	case <-time.After(testWait):
		t.Fatal("the message was never submitted")
	}
	route, _, err := al.resolveMessageRoute(msg)
	if err != nil {
		t.Fatal(err)
	}
	if want := resolveScopeKey(route, ""); job.SessionKey == "" || job.SessionKey != want {
		t.Errorf("job session key = %q, want the routed %q", job.SessionKey, want)
	}
	if job.Kind != proactive.TurnUser || job.Priority != cognition.PriUser || job.Budgeted {
		t.Errorf("job = kind %s priority %s budgeted %v, want an unbudgeted user job", job.Kind, job.Priority, job.Budgeted)
	}
	if n := prov.callCount("hello"); n != 0 {
		t.Errorf("the model was called %d time(s) before the job ran", n)
	}
	// The job never ran, so the reply never came; the loop is free to read on.
}

// The key the job is serialised on is the key the turn locks.
func TestRun_JobSessionKeyIsTheTurnsLockKey(t *testing.T) {
	inLLM := newSignal()
	release := newSignal()
	prov := newRoutedProvider().on("hello", func(ctx context.Context, _ int, _ []providers.Message) (*providers.LLMResponse, error) {
		inLLM.fire()
		select {
		case <-release.fired():
			return textResponse("hi")
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	al, _, cs := scheduledOwnerLoop(t, prov, cogCfg(1))
	_, stop := runOwnerLoop(t, al)
	defer stop()
	defer release.fire()

	publishMsg(t, al, dmFrom("424242", "hello"))
	inLLM.wait(t, "the turn to reach the model")
	jobs := cs.submitted()
	if len(jobs) != 1 {
		t.Fatalf("%d job(s) submitted, want 1", len(jobs))
	}
	if n := sessionLockRefs(al, jobs[0].SessionKey); n == 0 {
		t.Errorf("no session lock is held on the job's session key %q", jobs[0].SessionKey)
	}
}

// ====================== ordering ======================

func TestScheduler_UserBeforeProactive(t *testing.T) {
	p1InLLM := newSignal()
	releaseP1 := newSignal()
	var mu sync.Mutex
	var order []string
	note := func(s string) { mu.Lock(); order = append(order, s); mu.Unlock() }

	prov := newRoutedProvider().
		on("FIRST_PROACTIVE", func(ctx context.Context, _ int, _ []providers.Message) (*providers.LLMResponse, error) {
			note("p1")
			p1InLLM.fire()
			select {
			case <-releaseP1.fired():
				return textResponse("HEARTBEAT_OK")
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}).
		on("SECOND_PROACTIVE", func(context.Context, int, []providers.Message) (*providers.LLMResponse, error) {
			note("p2")
			return textResponse("HEARTBEAT_OK")
		}).
		on("USER_MESSAGE", func(context.Context, int, []providers.Message) (*providers.LLMResponse, error) {
			note("user")
			return textResponse("user reply")
		})
	al, sched, _ := scheduledOwnerLoop(t, prov, cogCfg(1))
	ctx, stop := runOwnerLoop(t, al)
	defer stop()
	out := collectOutbound(ctx, al.bus)

	run := func(token string) <-chan error {
		ch := make(chan error, 1)
		go func() {
			_, err := al.RunProactiveTurn(context.Background(), ProactiveTurnRequest{
				Kind: proactive.TurnProactive, Prompt: token, Deadline: 20 * time.Second,
			})
			ch <- err
		}()
		return ch
	}
	d1 := run("FIRST_PROACTIVE")
	p1InLLM.wait(t, "the first proactive turn to hold the worker")
	d2 := run("SECOND_PROACTIVE")
	waitUntil(t, "the second proactive job to queue", func() bool { return sched.Pending() == 2 })
	publishMsg(t, al, dmFrom("424242", "USER_MESSAGE"))
	waitUntil(t, "the user job to queue", func() bool { return sched.Pending() == 3 })

	releaseP1.fire()
	out.waitFor(t, "the user's reply", outboundContent("user reply"))
	for _, d := range []<-chan error{d1, d2} {
		select {
		case err := <-d:
			if err != nil {
				t.Errorf("proactive turn: %v", err)
			}
		case <-time.After(testWait):
			t.Fatal("a proactive turn never finished")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(order, []string{"p1", "user", "p2"}) {
		t.Errorf("model calls ran in order %v, want [p1 user p2]: the user waited for more than the one running job", order)
	}
}

// The MR-009 race test, through the scheduler: a heartbeat runs while a user
// turn is parked in the model, and its message must not suppress the user's reply.
func TestRun_ConcurrentHeartbeatMessage_DoesNotSuppressUserReply_Scheduled(t *testing.T) {
	userInLLM := newSignal()
	releaseUser := newSignal()
	prov := newRoutedProvider().
		on("USER_TURN", func(ctx context.Context, _ int, _ []providers.Message) (*providers.LLMResponse, error) {
			userInLLM.fire()
			select {
			case <-releaseUser.fired():
				return textResponse("user reply")
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}).
		on("HEARTBEAT_TURN", func(_ context.Context, call int, _ []providers.Message) (*providers.LLMResponse, error) {
			if call == 1 {
				return toolCallResponse(tools.NameMessage, map[string]any{"content": "heartbeat ping"})
			}
			return textResponse("HEARTBEAT_OK")
		})
	// Two workers: the heartbeat and the user turn are different sessions.
	al, _, cs := scheduledOwnerLoop(t, prov, cogCfg(2))
	al.cfg.Tools.Message.Enabled = true
	ctx, stop := runOwnerLoop(t, al)
	defer stop()
	out := collectOutbound(ctx, al.bus)

	publishMsg(t, al, inboundTo("chat-1", "USER_TURN hello"))
	userInLLM.wait(t, "the user turn to reach the model")
	if _, err := al.ProcessHeartbeat(ctx, "HEARTBEAT_TURN check tasks", "telegram", "chat-1"); err != nil {
		t.Fatalf("ProcessHeartbeat: %v", err)
	}
	releaseUser.fire()
	out.waitFor(t, "the user's reply (suppressed as already sent?)", outboundTo("chat-1", "user reply"))
	if n := out.count(outboundContent("user reply")); n != 1 {
		t.Errorf("user reply published %d times, want 1", n)
	}
	if got := len(cs.submitted()); got != 2 {
		t.Errorf("%d jobs submitted, want 2 (the user turn and the heartbeat)", got)
	}
}

// ====================== budget and deadline ======================

func TestHeartbeat_OverBudgetIsSkippedWithoutAModelCall(t *testing.T) {
	prov := newRoutedProvider().on("HB", func(context.Context, int, []providers.Message) (*providers.LLMResponse, error) {
		return textResponse("HEARTBEAT_OK")
	})
	cfg := cogCfg(1)
	cfg.TokensPerDay = 0 // no budgeted work at all
	al, _, _ := scheduledOwnerLoop(t, prov, cfg)

	if _, err := al.ProcessHeartbeat(context.Background(), "HB check", "telegram", "chat-1"); !errors.Is(err, proactive.ErrTurnSkipped) {
		t.Errorf("legacy heartbeat over budget: err = %v, want ErrTurnSkipped", err)
	}
	_, err := al.RunProactiveTurn(context.Background(), ProactiveTurnRequest{
		Kind: proactive.TurnHeartbeatLegacy, Prompt: "HB governed", Deadline: 5 * time.Second,
	})
	if !errors.Is(err, proactive.ErrTurnSkipped) {
		t.Errorf("governed heartbeat over budget: err = %v, want ErrTurnSkipped", err)
	}
	_, err = al.RunProactiveTurn(context.Background(), ProactiveTurnRequest{
		Kind: proactive.TurnProactive, Prompt: "HB compose", Deadline: 5 * time.Second,
		Candidates: []proactive.Candidate{cand("c1", proactive.SeverityNotice, false)},
	})
	if !errors.Is(err, proactive.ErrTurnSkipped) {
		t.Errorf("composer turn over budget: err = %v, want ErrTurnSkipped (the composer sends its template on any error)", err)
	}
	if n := prov.callCount("HB"); n != 0 {
		t.Errorf("the model was called %d time(s) for skipped turns", n)
	}
}

func TestRunProactiveTurn_DeadlineThroughScheduler(t *testing.T) {
	prov := newRoutedProvider().on("SLOW", func(ctx context.Context, _ int, _ []providers.Message) (*providers.LLMResponse, error) {
		return blockUntilCancelled(ctx)
	})
	al, sched, _ := scheduledOwnerLoop(t, prov, cogCfg(1))
	start := time.Now()
	_, err := al.RunProactiveTurn(context.Background(), ProactiveTurnRequest{
		Kind: proactive.TurnProactive, Prompt: "SLOW", Deadline: 150 * time.Millisecond,
	})
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, proactive.ErrTurnSkipped) {
		t.Errorf("err = %v, want a deadline error (not a budget skip)", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("the turn took %s to hit a 150ms deadline", time.Since(start))
	}
	waitUntil(t, "the worker to be free", func() bool { return sched.Pending() == 0 })
}

func TestInboundSpec_UserDeadlineFallbackTellsTheUser(t *testing.T) {
	al, _ := ownerLoop(t, newRoutedProvider())
	out := collectOutbound(context.Background(), al.bus)
	spec := al.inboundSpec(dmFrom("424242", "hi"))
	if spec.Budgeted || spec.Fallback == nil {
		t.Fatalf("user spec = %+v, want unbudgeted with a deadline fallback", spec)
	}
	if err := spec.Fallback(context.Background(), cognition.ReasonDeadline); err != nil {
		t.Fatal(err)
	}
	out.waitFor(t, "the timeout notice", outboundContent(deadlineNotice))
}

// ====================== usage ======================

func TestScheduler_UsageReachesTheBudget(t *testing.T) {
	prov := newRoutedProvider().on("USAGE", func(context.Context, int, []providers.Message) (*providers.LLMResponse, error) {
		return &providers.LLMResponse{Content: "HEARTBEAT_OK", Usage: &providers.UsageInfo{PromptTokens: 120, CompletionTokens: 15}}, nil
	})
	al, sched, _ := scheduledOwnerLoop(t, prov, cogCfg(1))
	if _, err := al.RunProactiveTurn(context.Background(), ProactiveTurnRequest{
		Kind: proactive.TurnProactive, Prompt: "USAGE", Deadline: 5 * time.Second,
	}); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the usage to be recorded", func() bool { return sched.Stats().Budget["proactive"].InputTokens == 120 })
	if got := sched.Stats().Budget["proactive"]; got.OutputTokens != 15 || got.Wakes != 1 {
		t.Errorf("budget = %+v, want 1 wake, 120 in, 15 out", got)
	}
}

// ====================== what a turn carries ======================

type ctxInfo struct {
	Origin, Sender string
	SubTurn        bool
	Scope          tools.AutomationScope
	ScopeOK        bool
	Kind           proactive.TurnKind
	HasTaint       bool
	Taint          proactive.Trust
	HasProposal    bool
	HasCollector   bool
}

func infoOf(ctx context.Context) ctxInfo {
	var i ctxInfo
	i.Origin, i.Sender, i.SubTurn = tools.ToolOrigin(ctx), tools.ToolSender(ctx), tools.IsSubTurn(ctx)
	i.Scope, i.ScopeOK = tools.AutomationScopeFrom(ctx)
	i.Kind = tools.ProactiveTurnKind(ctx)
	if tn := proactive.TaintFrom(ctx); tn != nil {
		i.HasTaint, i.Taint = true, tn.Trust()
	}
	i.HasProposal = tools.ProposalScopeFrom(ctx) != nil
	i.HasCollector = proactive.CollectorFrom(ctx) != nil
	return i
}

// Every entry point carries the origin, sender, scope, turn kind, taint and
// proposal scope the direct call carries, scheduled or not.
func TestScheduler_EntryPointsCarryTheDirectCallsContext(t *testing.T) {
	type entry struct {
		name string
		kind proactive.TurnKind // the job's kind when scheduled
		run  func(t *testing.T, al *AgentLoop, token string)
	}
	// What the scheduler is told about each entry point: its priority and
	// whether it counts against the background budget.
	type queueing struct {
		pri      cognition.Priority
		budgeted bool
	}
	wantQueueing := map[string]queueing{
		"user message via Run":               {cognition.PriUser, false},
		"cron ProcessDirect":                 {cognition.PriScheduled, false},
		"CLI ProcessDirect":                  {cognition.PriUser, false},
		"legacy heartbeat":                   {cognition.PriLegacy, true},
		"proactive turn with candidates":     {cognition.PriProactive, true},
		"proactive turn with a forced skill": {cognition.PriProactive, true},
		"governed heartbeat":                 {cognition.PriProactive, true},
		"system follow-up of a cron turn":    {cognition.PriApprovalFollowUp, false},
	}
	cronCtx := func() context.Context {
		ctx := tools.WithToolOrigin(context.Background(), tools.OriginCron)
		ctx = tools.WithAutomationScope(ctx, tools.AutomationScope{JobName: "nightly"})
		return tools.WithTurnID(ctx, tools.NewTurnID())
	}
	entries := []entry{
		{"user message via Run", proactive.TurnUser, func(t *testing.T, al *AgentLoop, token string) {
			ctx, stop := runOwnerLoop(t, al)
			defer stop()
			out := collectOutbound(ctx, al.bus)
			publishMsg(t, al, dmFrom("424242", token))
			out.waitFor(t, "the reply", outboundContent("ok "+token))
		}},
		{"cron ProcessDirect", proactive.TurnCronLegacy, func(t *testing.T, al *AgentLoop, token string) {
			if _, err := al.ProcessDirectWithChannel(cronCtx(), token, "agent:main:cron:x", "telegram", "424242", true); err != nil {
				t.Fatal(err)
			}
		}},
		{"CLI ProcessDirect", proactive.TurnUser, func(t *testing.T, al *AgentLoop, token string) {
			if _, err := al.ProcessDirect(context.Background(), token, "cli:test"); err != nil {
				t.Fatal(err)
			}
		}},
		{"legacy heartbeat", proactive.TurnHeartbeatLegacy, func(t *testing.T, al *AgentLoop, token string) {
			if _, err := al.ProcessHeartbeat(context.Background(), token, "telegram", "424242"); err != nil {
				t.Fatal(err)
			}
		}},
		{"proactive turn with candidates", proactive.TurnProactive, func(t *testing.T, al *AgentLoop, token string) {
			if _, err := al.RunProactiveTurn(context.Background(), ProactiveTurnRequest{
				Kind: proactive.TurnProactive, Prompt: token, Deadline: 5 * time.Second, DeliverNow: true,
				Candidates: []proactive.Candidate{cand("c1", proactive.SeverityNotice, false)},
			}); err != nil {
				t.Fatal(err)
			}
		}},
		{"proactive turn with a forced skill", proactive.TurnProactive, func(t *testing.T, al *AgentLoop, token string) {
			if _, err := al.RunProactiveTurn(context.Background(), ProactiveTurnRequest{
				Kind: proactive.TurnProactive, Prompt: token, Deadline: 5 * time.Second,
				Candidates: []proactive.Candidate{cand("c1", proactive.SeverityNotice, false)}, ForcedSkills: []string{"x"},
			}); err != nil {
				t.Fatal(err)
			}
		}},
		{"governed heartbeat", proactive.TurnHeartbeatLegacy, func(t *testing.T, al *AgentLoop, token string) {
			if _, err := al.RunProactiveTurn(context.Background(), ProactiveTurnRequest{
				Kind: proactive.TurnHeartbeatLegacy, Prompt: token, Deadline: 5 * time.Second,
			}); err != nil {
				t.Fatal(err)
			}
		}},
		{"system follow-up of a cron turn", proactive.TurnApprovalFollowUp, func(t *testing.T, al *AgentLoop, token string) {
			ctx, stop := runOwnerLoop(t, al)
			defer stop()
			out := collectOutbound(ctx, al.bus)
			publishMsg(t, al, bus.InboundMessage{
				Channel: "system", SenderID: "subagent", ChatID: "telegram:424242", Content: token,
				Metadata: map[string]string{
					metadataKeyTurnOrigin: tools.OriginCron, metadataKeyAutomationJob: "nightly",
				},
			})
			out.waitFor(t, "the follow-up", outboundContent("ok "+token))
		}},
	}

	for i, e := range entries {
		t.Run(e.name, func(t *testing.T) {
			token := "TOKEN" + string(rune('A'+i))
			run := func(scheduled bool) (ctxInfo, *countingScheduler) {
				var mu sync.Mutex
				var got ctxInfo
				seen := false
				prov := newRoutedProvider().on(token, func(ctx context.Context, _ int, _ []providers.Message) (*providers.LLMResponse, error) {
					mu.Lock()
					defer mu.Unlock()
					if !seen {
						got, seen = infoOf(ctx), true
					}
					return textResponse("ok " + token)
				})
				var al *AgentLoop
				var cs *countingScheduler
				if scheduled {
					al, _, cs = scheduledOwnerLoop(t, prov, cogCfg(1))
				} else {
					al, _ = ownerLoop(t, prov)
				}
				e.run(t, al, token)
				mu.Lock()
				defer mu.Unlock()
				if !seen {
					t.Fatal("the model was never called")
				}
				return got, cs
			}
			direct, _ := run(false)
			viaJob, cs := run(true)
			if !reflect.DeepEqual(direct, viaJob) {
				t.Errorf("scheduled context differs from the direct call's:\n direct %+v\n job    %+v", direct, viaJob)
			}
			jobs := cs.submitted()
			if len(jobs) != 1 || jobs[0].Kind != e.kind {
				t.Errorf("jobs = %+v, want exactly one of kind %s", jobs, e.kind)
			}
			if w, ok := wantQueueing[e.name]; !ok {
				t.Errorf("no expected queueing for entry %q", e.name)
			} else if len(jobs) == 1 && (jobs[0].Priority != w.pri || jobs[0].Budgeted != w.budgeted) {
				t.Errorf("job priority %s budgeted %v, want %s budgeted %v",
					jobs[0].Priority, jobs[0].Budgeted, w.pri, w.budgeted)
			}
		})
	}
}

// With no scheduler, no turn is wrapped: no job marker, no usage aggregate.
func TestCognitionOff_TurnsTakeTheDirectPath(t *testing.T) {
	var mu sync.Mutex
	var marked, aggregated bool
	prov := newRoutedProvider().on("OFF", func(ctx context.Context, _ int, _ []providers.Message) (*providers.LLMResponse, error) {
		mu.Lock()
		marked = marked || inScheduledJob(ctx)
		aggregated = aggregated || ctx.Value(usageAggKey{}) != nil
		mu.Unlock()
		return textResponse("HEARTBEAT_OK")
	})
	al, _ := ownerLoop(t, prov)
	if al.scheduler() != nil {
		t.Fatal("a scheduler is installed on a fresh loop")
	}
	ctx, stop := runOwnerLoop(t, al)
	defer stop()
	out := collectOutbound(ctx, al.bus)
	publishMsg(t, al, dmFrom("424242", "OFF user"))
	out.waitFor(t, "the reply", outboundContent("HEARTBEAT_OK"))
	if _, err := al.ProcessHeartbeat(ctx, "OFF hb", "telegram", "424242"); err != nil {
		t.Fatal(err)
	}
	if _, err := al.RunProactiveTurn(ctx, ProactiveTurnRequest{Kind: proactive.TurnProactive, Prompt: "OFF p", Deadline: 5 * time.Second}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if marked || aggregated {
		t.Errorf("a turn ran as a job with cognition off (marker %v, usage aggregate %v)", marked, aggregated)
	}
	// Installing and removing a scheduler restores the same state.
	s, _ := startScheduler(t, cogCfg(1))
	al.SetScheduler(s)
	al.SetScheduler(nil)
	if al.scheduler() != nil {
		t.Error("SetScheduler(nil) left a scheduler installed")
	}
}

// ====================== sub-turns, /stop, stopping ======================

func TestScheduler_SubTurnRunsInsideItsParentJob(t *testing.T) {
	childInLLM := newSignal()
	childErr := make(chan error, 1)
	prov := newRoutedProvider().
		on("PARENT_TURN", func(_ context.Context, call int, _ []providers.Message) (*providers.LLMResponse, error) {
			if call == 1 {
				return toolCallResponse("spawn_sync_child", map[string]any{})
			}
			return textResponse("parent done")
		}).
		on("CHILD_TURN", func(ctx context.Context, _ int, _ []providers.Message) (*providers.LLMResponse, error) {
			childInLLM.fire()
			<-ctx.Done()
			childErr <- ctx.Err()
			return nil, ctx.Err()
		})
	al, sched, cs := scheduledOwnerLoop(t, prov, cogCfg(1))
	al.RegisterTool(&funcTool{name: "spawn_sync_child", fn: func(ctx context.Context, _ map[string]any) *tools.ToolResult {
		res, err := SpawnSubTurn(ctx, SubTurnConfig{Model: "char-model", SystemPrompt: "CHILD_TURN work"})
		if err != nil {
			return tools.ErrorResult(err.Error())
		}
		return res
	}})

	done := startTurn(context.Background(), al, "PARENT_TURN", "agent:main:sub-in-job")
	childInLLM.wait(t, "the sub-turn to block in the model")
	if n := len(cs.submitted()); n != 1 {
		t.Fatalf("%d jobs submitted while a sub-turn runs, want 1: the sub-turn is not a job", n)
	}
	// The job is cancelled (what /stop does); the synchronous child must go with it.
	sched.CancelAll("test")
	select {
	case err := <-childErr:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("sub-turn ended with %v, want context.Canceled", err)
		}
	case <-time.After(testWait):
		t.Fatal("cancelling the job did not reach its synchronous sub-turn")
	}
	awaitTurn(t, "the cancelled parent", done)
}

// A message the scheduler drops because it stops is not lost: it is handled as
// it would be without a scheduler.
func TestRun_MessageDroppedByAStoppingSchedulerIsStillHandled(t *testing.T) {
	blockerInLLM := newSignal()
	prov := newRoutedProvider().
		on("BLOCKER", func(ctx context.Context, _ int, _ []providers.Message) (*providers.LLMResponse, error) {
			blockerInLLM.fire()
			return blockUntilCancelled(ctx)
		}).
		on("QUEUED_USER", func(context.Context, int, []providers.Message) (*providers.LLMResponse, error) {
			return textResponse("handled anyway")
		})
	al, _ := ownerLoop(t, prov)
	sched, stopSched := startScheduler(t, cogCfg(1))
	al.SetScheduler(sched)
	ctx, stop := runOwnerLoop(t, al)
	defer stop()
	out := collectOutbound(ctx, al.bus)

	publishMsg(t, al, inboundTo("chat-1", "BLOCKER"))
	blockerInLLM.wait(t, "the blocking turn to hold the worker")
	publishMsg(t, al, inboundTo("chat-2", "QUEUED_USER"))
	waitUntil(t, "the second message to queue", func() bool { return sched.Pending() == 2 })

	stopSched() // the stopped scheduler stays installed, as after cognitionRuntime.stop
	out.waitFor(t, "the dropped message's reply", outboundContent("handled anyway"))
}

// After the scheduler has stopped (a reload, a shutdown) it stays installed and
// refuses work: budgeted turns are skipped, never run as unbudgeted model calls,
// and unbudgeted ones run inline as they would without a scheduler.
func TestStoppedScheduler_BudgetedTurnsAreSkippedAndOthersRunInline(t *testing.T) {
	prov := newRoutedProvider().
		on("BUDGETED", func(context.Context, int, []providers.Message) (*providers.LLMResponse, error) {
			return textResponse("HEARTBEAT_OK")
		}).
		on("DIRECT", func(context.Context, int, []providers.Message) (*providers.LLMResponse, error) {
			return textResponse("direct reply")
		})
	al, _ := ownerLoop(t, prov)
	sched, stopSched := startScheduler(t, cogCfg(1))
	al.SetScheduler(sched)
	stopSched()
	if !al.HasScheduler() {
		t.Fatal("the stopped scheduler was removed from the loop")
	}

	_, err := al.RunProactiveTurn(context.Background(), ProactiveTurnRequest{
		Kind: proactive.TurnProactive, Prompt: "BUDGETED", Deadline: 5 * time.Second,
	})
	if !errors.Is(err, proactive.ErrTurnSkipped) {
		t.Errorf("composer turn after stop: err = %v, want ErrTurnSkipped", err)
	}
	if _, err := al.ProcessHeartbeat(context.Background(), "BUDGETED hb", "telegram", "424242"); !errors.Is(err, proactive.ErrTurnSkipped) {
		t.Errorf("legacy heartbeat after stop: err = %v, want ErrTurnSkipped", err)
	}
	if n := prov.callCount("BUDGETED"); n != 0 {
		t.Errorf("%d model call(s) for budgeted turns after the scheduler stopped", n)
	}
	resp, err := al.ProcessDirect(context.Background(), "DIRECT hello", "cli:test")
	if err != nil || resp != "direct reply" {
		t.Errorf("ProcessDirect after stop = (%q, %v), want it to run inline", resp, err)
	}
}

// The deadline of a proactive turn covers its wait in the queue: behind a busy
// worker it is given up at its deadline, unrun, and the caller falls back to its
// template.
func TestRunProactiveTurn_DeadlineCoversTheQueueWait(t *testing.T) {
	holding := newSignal()
	release := newSignal()
	prov := newRoutedProvider().
		on("HOLD", func(ctx context.Context, _ int, _ []providers.Message) (*providers.LLMResponse, error) {
			holding.fire()
			select {
			case <-release.fired():
				return textResponse("held")
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}).
		on("WAITING", func(context.Context, int, []providers.Message) (*providers.LLMResponse, error) {
			return textResponse("HEARTBEAT_OK")
		})
	al, sched, _ := scheduledOwnerLoop(t, prov, cogCfg(1))
	defer release.fire()
	go al.ProcessDirect(context.Background(), "HOLD", "cli:hold") //nolint:errcheck // released at cleanup
	holding.wait(t, "the user turn to hold the worker")

	start := time.Now()
	_, err := al.RunProactiveTurn(context.Background(), ProactiveTurnRequest{
		Kind: proactive.TurnProactive, Prompt: "WAITING", Deadline: 150 * time.Millisecond,
	})
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want a deadline error", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("the turn waited %s for a 150ms deadline", d)
	}
	if n := prov.callCount("WAITING"); n != 0 {
		t.Errorf("the model was called %d time(s) for a turn that never got a worker", n)
	}
	release.fire()
	waitUntil(t, "the worker to be free", func() bool { return sched.Pending() == 0 })
}
