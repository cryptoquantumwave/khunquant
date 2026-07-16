package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/exchanges"
)

// stubReauthExchange is a minimal exchanges.Exchange + exchanges.ReauthExchange
// implementation used to unit-test WebullReconnectTool's polling state
// machine without touching the real webull package or network.
type stubReauthExchange struct {
	reconnectStatus string
	reconnectErr    error

	mu         sync.Mutex
	checkCalls int
	checkPlan  []checkStep // consumed in order; last entry repeats once exhausted
}

type checkStep struct {
	status string
	err    error
}

func (s *stubReauthExchange) Name() string { return "webull" }
func (s *stubReauthExchange) GetBalances(context.Context) ([]exchanges.Balance, error) {
	return nil, nil
}

func (s *stubReauthExchange) Reconnect(context.Context) (string, error) {
	return s.reconnectStatus, s.reconnectErr
}

func (s *stubReauthExchange) CheckReauth(context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := s.checkCalls
	if idx >= len(s.checkPlan) {
		idx = len(s.checkPlan) - 1
	}
	s.checkCalls++
	step := s.checkPlan[idx]
	return step.status, step.err
}

// stubNonReauthExchange implements exchanges.Exchange but NOT
// exchanges.ReauthExchange, to test the "does not support reconnect" branch.
type stubNonReauthExchange struct{}

func (s *stubNonReauthExchange) Name() string { return "not-webull" }
func (s *stubNonReauthExchange) GetBalances(context.Context) ([]exchanges.Balance, error) {
	return nil, nil
}

// registerStubExchange registers a stub under a unique per-test exchange
// name so exchanges.CreateExchangeForAccount's global instance cache never
// leaks a stub between subtests.
func registerStubExchange(t *testing.T, ex exchanges.Exchange) (name string) {
	t.Helper()
	name = fmt.Sprintf("stub-%s-%d", t.Name(), time.Now().UnixNano())
	exchanges.RegisterAccountFactory(name, func(*config.Config, string) (exchanges.Exchange, error) {
		return ex, nil
	})
	return name
}

// withReconnectTiming overrides the poll interval/timeout for the duration
// of a test, restoring the originals via t.Cleanup.
func withReconnectTiming(t *testing.T, interval, timeout time.Duration) {
	t.Helper()
	origInterval, origTimeout := webullReconnectPollInterval, webullReconnectTimeout
	webullReconnectPollInterval = interval
	webullReconnectTimeout = timeout
	t.Cleanup(func() {
		webullReconnectPollInterval = origInterval
		webullReconnectTimeout = origTimeout
	})
}

func TestWebullReconnectImmediateNormal(t *testing.T) {
	stub := &stubReauthExchange{reconnectStatus: reauthStatusNormal}
	name := registerStubExchange(t, stub)

	tool := NewWebullReconnectTool(&config.Config{})
	result := tool.Execute(context.Background(), map[string]any{"exchange": name})

	if result.Async {
		t.Errorf("expected a non-async result for an already-NORMAL session, got Async=true")
	}
	if result.IsError {
		t.Errorf("expected success, got error: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, "Retry") {
		t.Errorf("expected ForLLM to instruct retrying the original request, got: %s", result.ForLLM)
	}
}

func TestWebullReconnectInvalidExpiredNoPolling(t *testing.T) {
	for _, status := range []string{reauthStatusInvalid, reauthStatusExpired} {
		t.Run(status, func(t *testing.T) {
			stub := &stubReauthExchange{reconnectStatus: status}
			name := registerStubExchange(t, stub)

			tool := NewWebullReconnectTool(&config.Config{})
			called := make(chan struct{}, 1)
			result := tool.ExecuteAsync(context.Background(), map[string]any{"exchange": name}, func(context.Context, *ToolResult) {
				called <- struct{}{}
			})

			if !result.IsError {
				t.Errorf("expected an error result for status %s, got success", status)
			}
			if result.Async {
				t.Errorf("INVALID/EXPIRED must not start background polling (Async=true)")
			}
			select {
			case <-called:
				t.Errorf("callback must not fire for INVALID/EXPIRED — there is nothing to poll")
			case <-time.After(50 * time.Millisecond):
				// expected: no callback
			}
		})
	}
}

func TestWebullReconnectExchangeNotFound(t *testing.T) {
	tool := NewWebullReconnectTool(&config.Config{})
	result := tool.Execute(context.Background(), map[string]any{"exchange": "definitely-not-registered-xyz"})
	if !result.IsError {
		t.Errorf("expected error for an unregistered exchange")
	}
}

func TestWebullReconnectExchangeDoesNotSupportReauth(t *testing.T) {
	name := registerStubExchange(t, &stubNonReauthExchange{})
	tool := NewWebullReconnectTool(&config.Config{})
	result := tool.Execute(context.Background(), map[string]any{"exchange": name})
	if !result.IsError {
		t.Errorf("expected error when the exchange does not implement ReauthExchange")
	}
	if !strings.Contains(result.ForLLM, "does not support reconnect") {
		t.Errorf("expected a clear 'does not support reconnect' message, got: %s", result.ForLLM)
	}
}

func TestWebullReconnectPendingThenNormal(t *testing.T) {
	withReconnectTiming(t, time.Millisecond, 5*time.Second)

	stub := &stubReauthExchange{
		reconnectStatus: reauthStatusPending,
		checkPlan: []checkStep{
			{status: reauthStatusPending},
			{status: reauthStatusPending},
			{status: reauthStatusNormal},
		},
	}
	name := registerStubExchange(t, stub)
	tool := NewWebullReconnectTool(&config.Config{})

	resultCh := make(chan *ToolResult, 1)
	immediate := tool.ExecuteAsync(context.Background(), map[string]any{"exchange": name}, func(_ context.Context, r *ToolResult) {
		resultCh <- r
	})

	if !immediate.Async {
		t.Fatalf("expected the immediate result to be Async=true for PENDING")
	}
	if !strings.Contains(immediate.ForUser, "Webull app") {
		t.Errorf("expected immediate ForUser to tell the user to approve in the Webull app, got: %s", immediate.ForUser)
	}

	select {
	case r := <-resultCh:
		if r.IsError {
			t.Fatalf("expected eventual success, got error: %s", r.ForLLM)
		}
		if !strings.Contains(r.ForUser, "reconnected") {
			t.Errorf("expected ForUser to announce reconnection, got: %s", r.ForUser)
		}
		if !strings.Contains(r.ForLLM, "Retry") {
			t.Errorf("expected ForLLM to instruct retrying the original request, got: %s", r.ForLLM)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the background poll callback")
	}
}

func TestWebullReconnectPollTimesOut(t *testing.T) {
	withReconnectTiming(t, time.Millisecond, 20*time.Millisecond)

	stub := &stubReauthExchange{
		reconnectStatus: reauthStatusPending,
		checkPlan:       []checkStep{{status: reauthStatusPending}}, // always pending
	}
	name := registerStubExchange(t, stub)
	tool := NewWebullReconnectTool(&config.Config{})

	resultCh := make(chan *ToolResult, 1)
	tool.ExecuteAsync(context.Background(), map[string]any{"exchange": name}, func(_ context.Context, r *ToolResult) {
		resultCh <- r
	})

	select {
	case r := <-resultCh:
		if !r.IsError {
			t.Fatalf("expected a timeout result to be an error, got success: %s", r.ForUser)
		}
		if !strings.Contains(strings.ToLower(r.ForUser), "wasn't approved") && !strings.Contains(strings.ToLower(r.ForUser), "no approval") {
			t.Errorf("expected ForUser to explain the timeout, got: %s", r.ForUser)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the poll-timeout callback")
	}
}

func TestWebullReconnectPollResolvesInvalid(t *testing.T) {
	withReconnectTiming(t, time.Millisecond, 5*time.Second)

	stub := &stubReauthExchange{
		reconnectStatus: reauthStatusPending,
		checkPlan: []checkStep{
			{status: reauthStatusPending},
			{status: reauthStatusInvalid},
		},
	}
	name := registerStubExchange(t, stub)
	tool := NewWebullReconnectTool(&config.Config{})

	resultCh := make(chan *ToolResult, 1)
	tool.ExecuteAsync(context.Background(), map[string]any{"exchange": name}, func(_ context.Context, r *ToolResult) {
		resultCh <- r
	})

	select {
	case r := <-resultCh:
		if !r.IsError {
			t.Fatalf("expected INVALID to be reported as an error, got success")
		}
		if !strings.Contains(r.ForUser, "couldn't be completed") {
			t.Errorf("expected ForUser to explain the failure, got: %s", r.ForUser)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the poll-invalid callback")
	}
}

// TestWebullReconnectTransientCheckErrorDoesNotAbort verifies that a single
// CheckReauth error mid-poll doesn't end the wait — only the overall
// timeout (or a definitive status) should end it.
func TestWebullReconnectTransientCheckErrorDoesNotAbort(t *testing.T) {
	withReconnectTiming(t, time.Millisecond, 5*time.Second)

	var calls int32
	stub := &stubReauthExchange{reconnectStatus: reauthStatusPending}
	// Custom CheckReauth via checkPlan won't express a transient error easily
	// with the simple stub, so wrap manually:
	errStub := &transientErrorThenNormalStub{stubReauthExchange: stub, failFirstN: 2, calls: &calls}
	name := registerStubExchange(t, errStub)
	tool := NewWebullReconnectTool(&config.Config{})

	resultCh := make(chan *ToolResult, 1)
	tool.ExecuteAsync(context.Background(), map[string]any{"exchange": name}, func(_ context.Context, r *ToolResult) {
		resultCh <- r
	})

	select {
	case r := <-resultCh:
		if r.IsError {
			t.Fatalf("expected eventual success despite transient errors, got: %s", r.ForLLM)
		}
		if atomic.LoadInt32(&calls) < 3 {
			t.Errorf("expected at least 3 CheckReauth calls (2 errors + 1 success), got %d", calls)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the callback")
	}
}

type transientErrorThenNormalStub struct {
	*stubReauthExchange
	failFirstN int32
	calls      *int32
}

func (s *transientErrorThenNormalStub) CheckReauth(ctx context.Context) (string, error) {
	n := atomic.AddInt32(s.calls, 1)
	if n <= s.failFirstN {
		return "", errors.New("transient network blip")
	}
	return reauthStatusNormal, nil
}

// --- reauthText / reauthHint ---

func TestReauthTextWrapChain(t *testing.T) {
	// Mirrors the real wrap chain: client -> adapter -> exchange -> tool.
	wrapped := fmt.Errorf("webull: FetchBalance: %w", fmt.Errorf("webull: get token: %w", exchanges.ErrNeedsReauth))

	msg := reauthText(wrapped, "webull", "main")
	if msg == "" {
		t.Fatal("expected reauthText to detect ErrNeedsReauth through multiple %w wraps")
	}
	if !strings.Contains(msg, NameWebullReconnect) {
		t.Errorf("expected the directing message to name the reconnect tool, got: %s", msg)
	}
	if !strings.Contains(msg, "webull") || !strings.Contains(msg, "main") {
		t.Errorf("expected the message to reference the exchange/account, got: %s", msg)
	}
}

func TestReauthTextUnrelatedError(t *testing.T) {
	if msg := reauthText(errors.New("some other failure"), "webull", "main"); msg != "" {
		t.Errorf("expected no reauth hint for an unrelated error, got: %s", msg)
	}
	if msg := reauthText(nil, "webull", "main"); msg != "" {
		t.Errorf("expected no reauth hint for a nil error, got: %s", msg)
	}
}

func TestReauthHintReturnsErrorResult(t *testing.T) {
	hint := reauthHint(exchanges.ErrNeedsReauth, "webull", "main")
	if hint == nil || !hint.IsError {
		t.Fatal("expected reauthHint to return an IsError ToolResult for ErrNeedsReauth")
	}
	if reauthHint(errors.New("unrelated"), "webull", "main") != nil {
		t.Error("expected reauthHint to return nil for an unrelated error")
	}
}
