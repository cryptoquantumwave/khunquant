package sim

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/providers/protocoltypes"
)

// slowInner stands in for a model server that takes a while (or never answers): it answers
// after delay, or when the call's context ends.
type slowInner struct {
	delay   time.Duration
	started chan struct{}
	once    sync.Once
}

func (s *slowInner) Chat(ctx context.Context, _ []protocoltypes.Message, _ []protocoltypes.ToolDefinition, _ string, _ map[string]any) (*protocoltypes.LLMResponse, error) {
	if s.started != nil {
		s.once.Do(func() { close(s.started) })
	}
	select {
	case <-time.After(s.delay):
		return &protocoltypes.LLMResponse{Content: "slow but fine", FinishReason: "stop",
			Usage: &protocoltypes.UsageInfo{PromptTokens: 10, CompletionTokens: 5}}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// A call that runs over its wall limit fails with ErrCallTimeout, and the journal entry says
// timeout:true so the scenario runner reads the run as infrastructure.
func TestLiveCallTimeoutIsMarkedInTheJournal(t *testing.T) {
	e, j, lo, _ := newLiveEngine(t, ModeLive, 0, 1_000_000, "")
	lo.Inner = &slowInner{delay: time.Minute}
	lo.Timeout = 50 * time.Millisecond
	start := time.Now()
	_, err := ask(e, lo, "hi")
	if !errors.Is(err, ErrCallTimeout) {
		t.Fatalf("err = %v, want ErrCallTimeout", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("the timeout took %v: it did not end the call", time.Since(start))
	}
	calls := kinds(j, "llm.call")
	if len(calls) != 1 || calls[0]["timeout"] != true || calls[0]["error"] == nil {
		t.Fatalf("journal = %v, want one llm.call with timeout:true and an error", calls)
	}
	// An ordinary failure is not a timeout.
	lo.Inner = &fakeInner{resp: func(int) (*protocoltypes.LLMResponse, error) { return nil, errors.New("HTTP 500") }}
	if _, err := ask(e, lo, "again"); err == nil || errors.Is(err, ErrCallTimeout) {
		t.Fatalf("err = %v: a 500 is an error, not a timeout", err)
	}
	if calls = kinds(j, "llm.call"); len(calls) != 2 || calls[1]["timeout"] == true {
		t.Fatalf("an ordinary error was marked as a timeout: %v", calls)
	}
}

// The caller's own cancellation is not the model's timeout.
func TestLiveCancelledCallIsNotATimeout(t *testing.T) {
	e, j, lo, _ := newLiveEngine(t, ModeLive, 0, 1_000_000, "")
	lo.Inner = &slowInner{delay: time.Minute}
	lo.Timeout = time.Minute
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.ChatWith(ctx, lo, user("x"), nil, "m", map[string]any{"prompt_cache_key": "main", "max_tokens": 100}); err == nil || errors.Is(err, ErrCallTimeout) {
		t.Fatalf("err = %v, want a plain cancellation", err)
	}
	for _, c := range kinds(j, "llm.call") {
		if c["timeout"] == true {
			t.Errorf("a cancelled call was journaled as a timeout: %v", c)
		}
	}
}

// InFlight counts a call from the moment it starts until it returns, so a settle can wait
// for a model that takes a minute.
func TestLiveInFlightCountsTheCallInProgress(t *testing.T) {
	e, _, lo, _ := newLiveEngine(t, ModeLive, 0, 1_000_000, "")
	in := &slowInner{delay: 300 * time.Millisecond, started: make(chan struct{})}
	lo.Inner = in
	if e.InFlight() != 0 {
		t.Fatalf("InFlight = %d before any call", e.InFlight())
	}
	done := make(chan error, 1)
	go func() { _, err := ask(e, lo, "hi"); done <- err }()
	<-in.started
	if e.InFlight() != 1 {
		t.Errorf("InFlight = %d during a call, want 1", e.InFlight())
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if e.InFlight() != 0 {
		t.Errorf("InFlight = %d after the call, want 0", e.InFlight())
	}
}

// The timeout does not count the wait for the budget lock: a call queued behind a slow one
// has its own full limit.
func TestLiveTimeoutStartsWhenTheCallIsMade(t *testing.T) {
	e, _, lo, _ := newLiveEngine(t, ModeLive, 0, 1_000_000, "")
	first := &slowInner{delay: 400 * time.Millisecond, started: make(chan struct{})}
	lo.Inner = first
	lo.Timeout = 2 * time.Second
	errs := make(chan error, 2)
	go func() { _, err := ask(e, lo, "one"); errs <- err }()
	<-first.started
	lo2 := *lo
	lo2.Inner = &slowInner{delay: 300 * time.Millisecond}
	lo2.Timeout = 600 * time.Millisecond // shorter than the wait in the queue plus the call, longer than the call
	go func() { _, err := ask(e, &lo2, "two"); errs <- err }()
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("a queued call timed out while it waited: %v", err)
		}
	}
}

func TestLiveEnvCallTimeout(t *testing.T) {
	get := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	le, err := LiveEnvFrom(get(map[string]string{}))
	if err != nil || le.CallTimeout != 180*time.Second {
		t.Fatalf("default = %v, %v; want 180s", le.CallTimeout, err)
	}
	le, err = LiveEnvFrom(get(map[string]string{"KQ_SANDBOX_LLM_CALL_TIMEOUT": "45s"}))
	if err != nil || le.CallTimeout != 45*time.Second {
		t.Fatalf("45s = %v, %v", le.CallTimeout, err)
	}
	if _, err = LiveEnvFrom(get(map[string]string{"KQ_SANDBOX_LLM_CALL_TIMEOUT": "never"})); err == nil {
		t.Error("a bad timeout must be refused")
	}
}
