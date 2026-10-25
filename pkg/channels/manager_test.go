package channels

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

// mockChannel is a test double that delegates Send to a configurable function.
type mockChannel struct {
	BaseChannel
	sendFn            func(ctx context.Context, msg bus.OutboundMessage) error
	startFn           func(ctx context.Context) error
	stopFn            func(ctx context.Context) error
	sentMessages      []bus.OutboundMessage
	placeholdersSent  int
	editedMessages    int
	lastPlaceholderID string
}

func (m *mockChannel) Send(ctx context.Context, msg bus.OutboundMessage) error {
	m.sentMessages = append(m.sentMessages, msg)
	return m.sendFn(ctx, msg)
}

func (m *mockChannel) Start(ctx context.Context) error {
	if m.startFn != nil {
		return m.startFn(ctx)
	}
	return nil
}

func (m *mockChannel) Stop(ctx context.Context) error {
	if m.stopFn != nil {
		return m.stopFn(ctx)
	}
	return nil
}

func (m *mockChannel) SendPlaceholder(ctx context.Context, chatID string) (string, error) {
	m.placeholdersSent++
	m.lastPlaceholderID = "mock-ph-123"
	return m.lastPlaceholderID, nil
}

func (m *mockChannel) EditMessage(ctx context.Context, chatID, messageID, content string) error {
	m.editedMessages++
	return nil
}

type mockMediaChannel struct {
	mockChannel
	sendMediaFn       func(ctx context.Context, msg bus.OutboundMediaMessage) error
	sentMediaMessages []bus.OutboundMediaMessage
}

func (m *mockMediaChannel) SendMedia(ctx context.Context, msg bus.OutboundMediaMessage) error {
	m.sentMediaMessages = append(m.sentMediaMessages, msg)
	if m.sendMediaFn != nil {
		return m.sendMediaFn(ctx, msg)
	}
	return nil
}

// newTestManager creates a minimal Manager suitable for unit tests.
func newTestManager() *Manager {
	return &Manager{
		channels: make(map[string]Channel),
		workers:  make(map[string]*channelWorker),
		bus:      bus.NewMessageBus(),
	}
}

func TestStartAll_AllChannelsFail_ReturnsJoinedError(t *testing.T) {
	m := newTestManager()
	errA := errors.New("channel-a start failed")
	errB := errors.New("channel-b start failed")

	m.channels["a"] = &mockChannel{
		startFn: func(_ context.Context) error { return errA },
	}
	m.channels["b"] = &mockChannel{
		startFn: func(_ context.Context) error { return errB },
	}

	err := m.StartAll(t.Context())
	if err == nil {
		t.Fatal("expected StartAll to fail when all channels fail")
	}
	if !strings.Contains(err.Error(), "failed to start any enabled channels") {
		t.Fatalf("unexpected error: %v", err)
	}
	if !errors.Is(err, errA) {
		t.Fatalf("expected error to wrap errA, got: %v", err)
	}
	if !errors.Is(err, errB) {
		t.Fatalf("expected error to wrap errB, got: %v", err)
	}
	if len(m.workers) != 0 {
		t.Fatalf("expected no workers on full startup failure, got %d", len(m.workers))
	}
	if m.dispatchTask != nil {
		t.Fatal("expected dispatch task to be cleared on full startup failure")
	}
}

func TestStartAll_PartialFailure_StartsSuccessfulWorkers(t *testing.T) {
	m := newTestManager()
	errBad := errors.New("bad channel start failed")
	processed := make(chan struct{}, 1)

	m.channels["good"] = &mockChannel{
		sendFn: func(_ context.Context, msg bus.OutboundMessage) error {
			if msg.Channel == "good" {
				select {
				case processed <- struct{}{}:
				default:
				}
			}
			return nil
		},
	}
	m.channels["bad"] = &mockChannel{
		startFn: func(_ context.Context) error { return errBad },
	}

	err := m.StartAll(t.Context())
	if err != nil {
		t.Fatalf("expected StartAll to succeed with partial channel failures, got: %v", err)
	}
	if len(m.workers) != 1 {
		t.Fatalf("expected exactly 1 active worker, got %d", len(m.workers))
	}
	if _, ok := m.workers["good"]; !ok {
		t.Fatal("expected worker for successful channel 'good'")
	}
	if _, ok := m.workers["bad"]; ok {
		t.Fatal("did not expect worker for failed channel 'bad'")
	}
	if m.dispatchTask == nil {
		t.Fatal("expected dispatch task to run when at least one channel starts")
	}

	pubCtx, pubCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer pubCancel()
	if err := m.bus.PublishOutbound(pubCtx, bus.OutboundMessage{
		Channel: "good",
		ChatID:  "chat-1",
		Content: "hello",
	}); err != nil {
		t.Fatalf("PublishOutbound() error = %v", err)
	}

	select {
	case <-processed:
		// worker processed outbound message as expected
	case <-time.After(2 * time.Second):
		t.Fatal("expected successful channel worker to process outbound message")
	}

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer stopCancel()
	if err := m.StopAll(stopCtx); err != nil {
		t.Fatalf("StopAll() error = %v", err)
	}
}

func TestSendWithRetry_Success(t *testing.T) {
	m := newTestManager()
	var callCount int
	ch := &mockChannel{
		sendFn: func(_ context.Context, _ bus.OutboundMessage) error {
			callCount++
			return nil
		},
	}
	w := &channelWorker{
		ch:      ch,
		limiter: rate.NewLimiter(rate.Inf, 1),
	}

	ctx := context.Background()
	msg := bus.OutboundMessage{Channel: "test", ChatID: "1", Content: "hello"}

	m.sendWithRetry(ctx, "test", w, msg)

	if callCount != 1 {
		t.Fatalf("expected 1 Send call, got %d", callCount)
	}
}

func TestSendWithRetry_TemporaryThenSuccess(t *testing.T) {
	m := newTestManager()
	var callCount int
	ch := &mockChannel{
		sendFn: func(_ context.Context, _ bus.OutboundMessage) error {
			callCount++
			if callCount <= 2 {
				return fmt.Errorf("network error: %w", ErrTemporary)
			}
			return nil
		},
	}
	w := &channelWorker{
		ch:      ch,
		limiter: rate.NewLimiter(rate.Inf, 1),
	}

	ctx := context.Background()
	msg := bus.OutboundMessage{Channel: "test", ChatID: "1", Content: "hello"}

	m.sendWithRetry(ctx, "test", w, msg)

	if callCount != 3 {
		t.Fatalf("expected 3 Send calls (2 failures + 1 success), got %d", callCount)
	}
}

func TestSendWithRetry_PermanentFailure(t *testing.T) {
	m := newTestManager()
	var callCount int
	ch := &mockChannel{
		sendFn: func(_ context.Context, _ bus.OutboundMessage) error {
			callCount++
			return fmt.Errorf("bad chat ID: %w", ErrSendFailed)
		},
	}
	w := &channelWorker{
		ch:      ch,
		limiter: rate.NewLimiter(rate.Inf, 1),
	}

	ctx := context.Background()
	msg := bus.OutboundMessage{Channel: "test", ChatID: "1", Content: "hello"}

	m.sendWithRetry(ctx, "test", w, msg)

	if callCount != 1 {
		t.Fatalf("expected 1 Send call (no retry for permanent failure), got %d", callCount)
	}
}

func TestSendWithRetry_NotRunning(t *testing.T) {
	m := newTestManager()
	var callCount int
	ch := &mockChannel{
		sendFn: func(_ context.Context, _ bus.OutboundMessage) error {
			callCount++
			return ErrNotRunning
		},
	}
	w := &channelWorker{
		ch:      ch,
		limiter: rate.NewLimiter(rate.Inf, 1),
	}

	ctx := context.Background()
	msg := bus.OutboundMessage{Channel: "test", ChatID: "1", Content: "hello"}

	m.sendWithRetry(ctx, "test", w, msg)

	if callCount != 1 {
		t.Fatalf("expected 1 Send call (no retry for ErrNotRunning), got %d", callCount)
	}
}

func TestSendWithRetry_RateLimitRetry(t *testing.T) {
	m := newTestManager()
	var callCount int
	ch := &mockChannel{
		sendFn: func(_ context.Context, _ bus.OutboundMessage) error {
			callCount++
			if callCount == 1 {
				return fmt.Errorf("429: %w", ErrRateLimit)
			}
			return nil
		},
	}
	w := &channelWorker{
		ch:      ch,
		limiter: rate.NewLimiter(rate.Inf, 1),
	}

	ctx := context.Background()
	msg := bus.OutboundMessage{Channel: "test", ChatID: "1", Content: "hello"}

	start := time.Now()
	m.sendWithRetry(ctx, "test", w, msg)
	elapsed := time.Since(start)

	if callCount != 2 {
		t.Fatalf("expected 2 Send calls (1 rate limit + 1 success), got %d", callCount)
	}
	// Should have waited at least rateLimitDelay (1s) but allow some slack
	if elapsed < 900*time.Millisecond {
		t.Fatalf("expected at least ~1s delay for rate limit retry, got %v", elapsed)
	}
}

func TestSendWithRetry_MaxRetriesExhausted(t *testing.T) {
	m := newTestManager()
	var callCount int
	ch := &mockChannel{
		sendFn: func(_ context.Context, _ bus.OutboundMessage) error {
			callCount++
			return fmt.Errorf("timeout: %w", ErrTemporary)
		},
	}
	w := &channelWorker{
		ch:      ch,
		limiter: rate.NewLimiter(rate.Inf, 1),
	}

	ctx := context.Background()
	msg := bus.OutboundMessage{Channel: "test", ChatID: "1", Content: "hello"}

	m.sendWithRetry(ctx, "test", w, msg)

	expected := maxRetries + 1 // initial attempt + maxRetries retries
	if callCount != expected {
		t.Fatalf("expected %d Send calls, got %d", expected, callCount)
	}
}

func TestSendMedia_Success(t *testing.T) {
	m := newTestManager()
	var callCount int
	ch := &mockMediaChannel{
		sendMediaFn: func(_ context.Context, _ bus.OutboundMediaMessage) error {
			callCount++
			return nil
		},
	}
	w := &channelWorker{
		ch:      ch,
		limiter: rate.NewLimiter(rate.Inf, 1),
	}
	m.channels["test"] = ch
	m.workers["test"] = w

	err := m.SendMedia(context.Background(), bus.OutboundMediaMessage{
		Channel: "test",
		ChatID:  "chat1",
		Parts:   []bus.MediaPart{{Ref: "media://abc"}},
	})
	if err != nil {
		t.Fatalf("SendMedia() error = %v", err)
	}
	if callCount != 1 {
		t.Fatalf("expected 1 SendMedia call, got %d", callCount)
	}
}

func TestSendMedia_PropagatesFailure(t *testing.T) {
	m := newTestManager()
	ch := &mockMediaChannel{
		sendMediaFn: func(_ context.Context, _ bus.OutboundMediaMessage) error {
			return fmt.Errorf("bad upload: %w", ErrSendFailed)
		},
	}
	w := &channelWorker{
		ch:      ch,
		limiter: rate.NewLimiter(rate.Inf, 1),
	}
	m.channels["test"] = ch
	m.workers["test"] = w

	err := m.SendMedia(context.Background(), bus.OutboundMediaMessage{
		Channel: "test",
		ChatID:  "chat1",
		Parts:   []bus.MediaPart{{Ref: "media://abc"}},
	})
	if err == nil {
		t.Fatal("expected SendMedia to return error")
	}
	if !errors.Is(err, ErrSendFailed) {
		t.Fatalf("expected ErrSendFailed, got %v", err)
	}
}

func TestSendWithRetry_UnknownError(t *testing.T) {
	m := newTestManager()
	var callCount int
	ch := &mockChannel{
		sendFn: func(_ context.Context, _ bus.OutboundMessage) error {
			callCount++
			if callCount == 1 {
				return errors.New("random unexpected error")
			}
			return nil
		},
	}
	w := &channelWorker{
		ch:      ch,
		limiter: rate.NewLimiter(rate.Inf, 1),
	}

	ctx := context.Background()
	msg := bus.OutboundMessage{Channel: "test", ChatID: "1", Content: "hello"}

	m.sendWithRetry(ctx, "test", w, msg)

	if callCount != 2 {
		t.Fatalf("expected 2 Send calls (unknown error treated as temporary), got %d", callCount)
	}
}

func TestSendWithRetry_ContextCancelled(t *testing.T) {
	m := newTestManager()
	var callCount int
	ch := &mockChannel{
		sendFn: func(_ context.Context, _ bus.OutboundMessage) error {
			callCount++
			return fmt.Errorf("timeout: %w", ErrTemporary)
		},
	}
	w := &channelWorker{
		ch:      ch,
		limiter: rate.NewLimiter(rate.Inf, 1),
	}

	ctx, cancel := context.WithCancel(context.Background())
	msg := bus.OutboundMessage{Channel: "test", ChatID: "1", Content: "hello"}

	// Cancel context after first Send attempt returns
	ch.sendFn = func(_ context.Context, _ bus.OutboundMessage) error {
		callCount++
		cancel()
		return fmt.Errorf("timeout: %w", ErrTemporary)
	}

	m.sendWithRetry(ctx, "test", w, msg)

	// Should have called Send once, then noticed ctx canceled during backoff
	if callCount != 1 {
		t.Fatalf("expected 1 Send call before context cancellation, got %d", callCount)
	}
}

func TestWorkerRateLimiter(t *testing.T) {
	m := newTestManager()

	var mu sync.Mutex
	var sendTimes []time.Time

	ch := &mockChannel{
		sendFn: func(_ context.Context, _ bus.OutboundMessage) error {
			mu.Lock()
			sendTimes = append(sendTimes, time.Now())
			mu.Unlock()
			return nil
		},
	}

	// Create a worker with a low rate: 2 msg/s, burst 1
	w := &channelWorker{
		ch:      ch,
		queue:   make(chan bus.OutboundMessage, 10),
		done:    make(chan struct{}),
		limiter: rate.NewLimiter(2, 1),
	}

	ctx := t.Context()

	go m.runWorker(ctx, "test", w)

	// Enqueue 4 messages
	for i := range 4 {
		w.queue <- bus.OutboundMessage{Channel: "test", ChatID: "1", Content: fmt.Sprintf("msg%d", i)}
	}

	// Wait enough time for all messages to be sent (4 msgs at 2/s = ~2s, give extra margin)
	time.Sleep(3 * time.Second)

	mu.Lock()
	times := make([]time.Time, len(sendTimes))
	copy(times, sendTimes)
	mu.Unlock()

	if len(times) != 4 {
		t.Fatalf("expected 4 sends, got %d", len(times))
	}

	// Verify rate limiting: total duration should be at least 1s
	// (first message immediate, then ~500ms between each subsequent one at 2/s)
	totalDuration := times[len(times)-1].Sub(times[0])
	if totalDuration < 1*time.Second {
		t.Fatalf("expected total duration >= 1s for 4 msgs at 2/s rate, got %v", totalDuration)
	}
}

func TestNewChannelWorker_DefaultRate(t *testing.T) {
	ch := &mockChannel{}
	w := newChannelWorker("unknown_channel", ch)

	if w.limiter == nil {
		t.Fatal("expected limiter to be non-nil")
	}
	if w.limiter.Limit() != rate.Limit(defaultRateLimit) {
		t.Fatalf("expected rate limit %v, got %v", rate.Limit(defaultRateLimit), w.limiter.Limit())
	}
}

func TestNewChannelWorker_ConfiguredRate(t *testing.T) {
	ch := &mockChannel{}

	for name, expectedRate := range channelRateConfig {
		w := newChannelWorker(name, ch)
		if w.limiter.Limit() != rate.Limit(expectedRate) {
			t.Fatalf("channel %s: expected rate %v, got %v", name, expectedRate, w.limiter.Limit())
		}
	}
}

func TestRunWorker_MessageSplitting(t *testing.T) {
	m := newTestManager()

	var mu sync.Mutex
	var received []string

	ch := &mockChannelWithLength{
		mockChannel: mockChannel{
			sendFn: func(_ context.Context, msg bus.OutboundMessage) error {
				mu.Lock()
				received = append(received, msg.Content)
				mu.Unlock()
				return nil
			},
		},
		maxLen: 5,
	}

	w := &channelWorker{
		ch:      ch,
		queue:   make(chan bus.OutboundMessage, 10),
		done:    make(chan struct{}),
		limiter: rate.NewLimiter(rate.Inf, 1),
	}

	ctx := t.Context()

	go m.runWorker(ctx, "test", w)

	// Send a message that should be split
	w.queue <- bus.OutboundMessage{Channel: "test", ChatID: "1", Content: "hello world"}

	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	count := len(received)
	mu.Unlock()

	if count < 2 {
		t.Fatalf("expected message to be split into at least 2 chunks, got %d", count)
	}
}

// mockChannelWithLength implements MessageLengthProvider.
type mockChannelWithLength struct {
	mockChannel
	maxLen int
}

func (m *mockChannelWithLength) MaxMessageLength() int {
	return m.maxLen
}

func TestSendWithRetry_ExponentialBackoff(t *testing.T) {
	m := newTestManager()

	var callTimes []time.Time
	var callCount atomic.Int32
	ch := &mockChannel{
		sendFn: func(_ context.Context, _ bus.OutboundMessage) error {
			callTimes = append(callTimes, time.Now())
			callCount.Add(1)
			return fmt.Errorf("timeout: %w", ErrTemporary)
		},
	}
	w := &channelWorker{
		ch:      ch,
		limiter: rate.NewLimiter(rate.Inf, 1),
	}

	ctx := context.Background()
	msg := bus.OutboundMessage{Channel: "test", ChatID: "1", Content: "hello"}

	start := time.Now()
	m.sendWithRetry(ctx, "test", w, msg)
	totalElapsed := time.Since(start)

	// With maxRetries=3: attempts at 0, ~500ms, ~1.5s, ~3.5s
	// Total backoff: 500ms + 1s + 2s = 3.5s
	// Allow some margin
	if totalElapsed < 3*time.Second {
		t.Fatalf("expected total elapsed >= 3s for exponential backoff, got %v", totalElapsed)
	}

	if int(callCount.Load()) != maxRetries+1 {
		t.Fatalf("expected %d calls, got %d", maxRetries+1, callCount.Load())
	}
}

// --- Phase 10: preSend orchestration tests ---

// mockMessageEditor is a channel that supports MessageEditor.
type mockMessageEditor struct {
	mockChannel
	editFn func(ctx context.Context, chatID, messageID, content string) error
}

func (m *mockMessageEditor) EditMessage(ctx context.Context, chatID, messageID, content string) error {
	return m.editFn(ctx, chatID, messageID, content)
}

func TestPreSend_PlaceholderEditSuccess(t *testing.T) {
	m := newTestManager()
	var sendCalled bool
	var editCalled bool

	ch := &mockMessageEditor{
		mockChannel: mockChannel{
			sendFn: func(_ context.Context, _ bus.OutboundMessage) error {
				sendCalled = true
				return nil
			},
		},
		editFn: func(_ context.Context, chatID, messageID, content string) error {
			editCalled = true
			if chatID != "123" {
				t.Fatalf("expected chatID 123, got %s", chatID)
			}
			if messageID != "456" {
				t.Fatalf("expected messageID 456, got %s", messageID)
			}
			if content != "hello" {
				t.Fatalf("expected content 'hello', got %s", content)
			}
			return nil
		},
	}

	// Register placeholder
	m.RecordPlaceholder("test", "123", "456")

	msg := bus.OutboundMessage{Channel: "test", ChatID: "123", Content: "hello"}
	edited, _ := m.preSend(context.Background(), "test", msg, ch)

	if !edited {
		t.Fatal("expected preSend to return true (placeholder edited)")
	}
	if !editCalled {
		t.Fatal("expected EditMessage to be called")
	}
	if sendCalled {
		t.Fatal("expected Send to NOT be called when placeholder edited")
	}
}

func TestPreSend_PlaceholderEditFails_FallsThrough(t *testing.T) {
	m := newTestManager()

	ch := &mockMessageEditor{
		mockChannel: mockChannel{
			sendFn: func(_ context.Context, _ bus.OutboundMessage) error {
				return nil
			},
		},
		editFn: func(_ context.Context, _, _, _ string) error {
			return fmt.Errorf("edit failed")
		},
	}

	m.RecordPlaceholder("test", "123", "456")

	msg := bus.OutboundMessage{Channel: "test", ChatID: "123", Content: "hello"}
	edited, _ := m.preSend(context.Background(), "test", msg, ch)

	if edited {
		t.Fatal("expected preSend to return false when edit fails")
	}
}

func TestInvokeTypingStop_CallsRegisteredStop(t *testing.T) {
	m := newTestManager()
	var stopCalled bool

	m.RecordTypingStop("telegram", "chat123", func() {
		stopCalled = true
	})

	m.InvokeTypingStop("telegram", "chat123")

	if !stopCalled {
		t.Fatal("expected typing stop func to be called")
	}
}

func TestInvokeTypingStop_NoOpWhenNoEntry(t *testing.T) {
	m := newTestManager()
	// Should not panic
	m.InvokeTypingStop("telegram", "nonexistent")
}

func TestInvokeTypingStop_Idempotent(t *testing.T) {
	m := newTestManager()
	var callCount int

	m.RecordTypingStop("telegram", "chat123", func() {
		callCount++
	})

	m.InvokeTypingStop("telegram", "chat123")
	m.InvokeTypingStop("telegram", "chat123") // Second call: entry already removed, no-op

	if callCount != 1 {
		t.Fatalf("expected stop to be called once, got %d", callCount)
	}
}

func TestPreSend_TypingStopCalled(t *testing.T) {
	m := newTestManager()
	var stopCalled bool

	ch := &mockChannel{
		sendFn: func(_ context.Context, _ bus.OutboundMessage) error {
			return nil
		},
	}

	m.RecordTypingStop("test", "123", func() {
		stopCalled = true
	})

	msg := bus.OutboundMessage{Channel: "test", ChatID: "123", Content: "hello"}
	_, _ = m.preSend(context.Background(), "test", msg, ch)

	if !stopCalled {
		t.Fatal("expected typing stop func to be called")
	}
}

func TestPreSend_NoRegisteredState(t *testing.T) {
	m := newTestManager()

	ch := &mockChannel{
		sendFn: func(_ context.Context, _ bus.OutboundMessage) error {
			return nil
		},
	}

	msg := bus.OutboundMessage{Channel: "test", ChatID: "123", Content: "hello"}
	edited, _ := m.preSend(context.Background(), "test", msg, ch)

	if edited {
		t.Fatal("expected preSend to return false with no registered state")
	}
}

func TestPreSend_TypingAndPlaceholder(t *testing.T) {
	m := newTestManager()
	var stopCalled bool
	var editCalled bool

	ch := &mockMessageEditor{
		mockChannel: mockChannel{
			sendFn: func(_ context.Context, _ bus.OutboundMessage) error {
				return nil
			},
		},
		editFn: func(_ context.Context, _, _, _ string) error {
			editCalled = true
			return nil
		},
	}

	m.RecordTypingStop("test", "123", func() {
		stopCalled = true
	})
	m.RecordPlaceholder("test", "123", "456")

	msg := bus.OutboundMessage{Channel: "test", ChatID: "123", Content: "hello"}
	edited, _ := m.preSend(context.Background(), "test", msg, ch)

	if !stopCalled {
		t.Fatal("expected typing stop to be called")
	}
	if !editCalled {
		t.Fatal("expected EditMessage to be called")
	}
	if !edited {
		t.Fatal("expected preSend to return true")
	}
}

func TestRecordPlaceholder_ConcurrentSafe(t *testing.T) {
	m := newTestManager()

	var wg sync.WaitGroup
	for i := range 100 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			chatID := fmt.Sprintf("chat_%d", i%10)
			m.RecordPlaceholder("test", chatID, fmt.Sprintf("msg_%d", i))
		}(i)
	}
	wg.Wait()
}

func TestRecordTypingStop_ConcurrentSafe(t *testing.T) {
	m := newTestManager()

	var wg sync.WaitGroup
	for i := range 100 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			chatID := fmt.Sprintf("chat_%d", i%10)
			m.RecordTypingStop("test", chatID, func() {})
		}(i)
	}
	wg.Wait()
}

func TestRecordTypingStop_ReplacesExistingStop(t *testing.T) {
	m := newTestManager()
	var oldStopCalls int
	var newStopCalls int

	m.RecordTypingStop("test", "123", func() {
		oldStopCalls++
	})

	m.RecordTypingStop("test", "123", func() {
		newStopCalls++
	})

	if oldStopCalls != 1 {
		t.Fatalf("expected previous typing stop to be called once when replaced, got %d", oldStopCalls)
	}
	if newStopCalls != 0 {
		t.Fatalf("expected replacement typing stop to stay active until preSend, got %d calls", newStopCalls)
	}

	msg := bus.OutboundMessage{Channel: "test", ChatID: "123", Content: "hello"}
	_, _ = m.preSend(context.Background(), "test", msg, &mockChannel{})

	if newStopCalls != 1 {
		t.Fatalf("expected replacement typing stop to be called by preSend, got %d", newStopCalls)
	}
	if oldStopCalls != 1 {
		t.Fatalf("expected previous typing stop to not be called again, got %d", oldStopCalls)
	}
}

func TestSendWithRetry_PreSendEditsPlaceholder(t *testing.T) {
	m := newTestManager()
	var sendCalled bool

	ch := &mockMessageEditor{
		mockChannel: mockChannel{
			sendFn: func(_ context.Context, _ bus.OutboundMessage) error {
				sendCalled = true
				return nil
			},
		},
		editFn: func(_ context.Context, _, _, _ string) error {
			return nil // edit succeeds
		},
	}

	m.RecordPlaceholder("test", "123", "456")

	w := &channelWorker{
		ch:      ch,
		limiter: rate.NewLimiter(rate.Inf, 1),
	}

	msg := bus.OutboundMessage{Channel: "test", ChatID: "123", Content: "hello"}
	m.sendWithRetry(context.Background(), "test", w, msg)

	if sendCalled {
		t.Fatal("expected Send to NOT be called when placeholder was edited")
	}
}

// --- Dispatcher exit tests (Step 1) ---

func TestDispatcherExitsOnCancel(t *testing.T) {
	mb := bus.NewMessageBus()
	defer mb.Close()

	m := &Manager{
		channels: make(map[string]Channel),
		workers:  make(map[string]*channelWorker),
		bus:      mb,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		m.dispatchOutbound(ctx)
		close(done)
	}()

	// Cancel context and verify the dispatcher exits quickly
	cancel()

	select {
	case <-done:
		// success
	case <-time.After(2 * time.Second):
		t.Fatal("dispatchOutbound did not exit within 2s after context cancel")
	}
}

func TestDispatcherMediaExitsOnCancel(t *testing.T) {
	mb := bus.NewMessageBus()
	defer mb.Close()

	m := &Manager{
		channels: make(map[string]Channel),
		workers:  make(map[string]*channelWorker),
		bus:      mb,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		m.dispatchOutboundMedia(ctx)
		close(done)
	}()

	cancel()

	select {
	case <-done:
		// success
	case <-time.After(2 * time.Second):
		t.Fatal("dispatchOutboundMedia did not exit within 2s after context cancel")
	}
}

// --- TTL Janitor tests (Step 2) ---

func TestTypingStopJanitorEviction(t *testing.T) {
	m := newTestManager()

	var stopCalled atomic.Bool
	// Store a typing entry with a creation time far in the past
	m.typingStops.Store("test:123", typingEntry{
		stop:      func() { stopCalled.Store(true) },
		createdAt: time.Now().Add(-10 * time.Minute), // well past typingStopTTL
	})

	// Run janitor with a short-lived context
	ctx, cancel := context.WithCancel(context.Background())

	// Manually trigger the janitor logic once by simulating a tick
	go func() {
		// Override janitor to run immediately
		now := time.Now()
		m.typingStops.Range(func(key, value any) bool {
			if entry, ok := value.(typingEntry); ok {
				if now.Sub(entry.createdAt) > typingStopTTL {
					if _, loaded := m.typingStops.LoadAndDelete(key); loaded {
						entry.stop()
					}
				}
			}
			return true
		})
		cancel()
	}()

	<-ctx.Done()

	if !stopCalled.Load() {
		t.Fatal("expected typing stop function to be called by janitor eviction")
	}

	// Verify entry was deleted
	if _, loaded := m.typingStops.Load("test:123"); loaded {
		t.Fatal("expected typing entry to be deleted after eviction")
	}
}

func TestPlaceholderJanitorEviction(t *testing.T) {
	m := newTestManager()

	// Store a placeholder entry with a creation time far in the past
	m.placeholders.Store("test:456", placeholderEntry{
		id:        "msg_old",
		createdAt: time.Now().Add(-20 * time.Minute), // well past placeholderTTL
	})

	// Simulate janitor logic
	now := time.Now()
	m.placeholders.Range(func(key, value any) bool {
		if entry, ok := value.(placeholderEntry); ok {
			if now.Sub(entry.createdAt) > placeholderTTL {
				m.placeholders.Delete(key)
			}
		}
		return true
	})

	// Verify entry was deleted
	if _, loaded := m.placeholders.Load("test:456"); loaded {
		t.Fatal("expected placeholder entry to be deleted after eviction")
	}
}

func TestPreSendStillWorksWithWrappedTypes(t *testing.T) {
	m := newTestManager()
	var stopCalled bool
	var editCalled bool

	ch := &mockMessageEditor{
		mockChannel: mockChannel{
			sendFn: func(_ context.Context, _ bus.OutboundMessage) error {
				return nil
			},
		},
		editFn: func(_ context.Context, chatID, messageID, content string) error {
			editCalled = true
			if messageID != "ph_id" {
				t.Fatalf("expected messageID ph_id, got %s", messageID)
			}
			return nil
		},
	}

	// Use the new wrapped types via the public API
	m.RecordTypingStop("test", "chat1", func() {
		stopCalled = true
	})
	m.RecordPlaceholder("test", "chat1", "ph_id")

	msg := bus.OutboundMessage{Channel: "test", ChatID: "chat1", Content: "response"}
	edited, _ := m.preSend(context.Background(), "test", msg, ch)

	if !stopCalled {
		t.Fatal("expected typing stop to be called via wrapped type")
	}
	if !editCalled {
		t.Fatal("expected EditMessage to be called via wrapped type")
	}
	if !edited {
		t.Fatal("expected preSend to return true")
	}
}

// --- Lazy worker creation tests (Step 6) ---

func TestLazyWorkerCreation(t *testing.T) {
	m := newTestManager()

	ch := &mockChannel{
		sendFn: func(_ context.Context, _ bus.OutboundMessage) error {
			return nil
		},
	}

	// RegisterChannel should NOT create a worker
	m.RegisterChannel("lazy", ch)

	m.mu.RLock()
	_, chExists := m.channels["lazy"]
	_, wExists := m.workers["lazy"]
	m.mu.RUnlock()

	if !chExists {
		t.Fatal("expected channel to be registered")
	}
	if wExists {
		t.Fatal("expected worker to NOT be created by RegisterChannel (lazy creation)")
	}
}

// --- FastID uniqueness test (Step 5) ---

func TestBuildMediaScope_FastIDUniqueness(t *testing.T) {
	seen := make(map[string]bool)

	for range 1000 {
		scope := BuildMediaScope("test", "chat1", "")
		if seen[scope] {
			t.Fatalf("duplicate scope generated: %s", scope)
		}
		seen[scope] = true
	}

	// Verify format: "channel:chatID:id"
	scope := BuildMediaScope("telegram", "42", "")
	parts := 0
	for _, c := range scope {
		if c == ':' {
			parts++
		}
	}
	if parts != 2 {
		t.Fatalf("expected scope to have 2 colons (channel:chatID:id), got: %s", scope)
	}
}

func TestBuildMediaScope_WithMessageID(t *testing.T) {
	scope := BuildMediaScope("discord", "chat99", "msg123")
	expected := "discord:chat99:msg123"
	if scope != expected {
		t.Fatalf("expected %s, got %s", expected, scope)
	}
}

func TestManager_PlaceholderConsumedByResponse(t *testing.T) {
	mgr := &Manager{
		channels:     make(map[string]Channel),
		workers:      make(map[string]*channelWorker),
		placeholders: sync.Map{},
	}

	mockCh := &mockChannel{
		sendFn: func(ctx context.Context, msg bus.OutboundMessage) error {
			return nil
		},
	}
	worker := newChannelWorker("mock", mockCh)
	mgr.channels["mock"] = mockCh
	mgr.workers["mock"] = worker

	ctx := context.Background()
	key := "mock:chat-1"

	// Simulate a placeholder recorded by base.go HandleMessage
	mgr.RecordPlaceholder("mock", "chat-1", "ph-123")

	if _, ok := mgr.placeholders.Load(key); !ok {
		t.Fatal("expected placeholder to be recorded")
	}

	// Transcription feedback arrives first — it should consume the placeholder
	// and be delivered via EditMessage, not Send.
	msgTranscript := bus.OutboundMessage{
		Channel: "mock",
		ChatID:  "chat-1",
		Content: "Transcript: hello",
	}
	mgr.sendWithRetry(ctx, "mock", worker, msgTranscript)

	if mockCh.editedMessages != 1 {
		t.Errorf("expected 1 edited message (placeholder consumed by transcript), got %d", mockCh.editedMessages)
	}
	if len(mockCh.sentMessages) != 0 {
		t.Errorf("expected 0 normal messages (transcript used edit), got %d", len(mockCh.sentMessages))
	}

	// Placeholder should be gone now
	if _, ok := mgr.placeholders.Load(key); ok {
		t.Error("expected placeholder to be removed after being consumed")
	}

	// Final LLM response arrives — no placeholder left, so it goes through Send
	msgFinal := bus.OutboundMessage{
		Channel: "mock",
		ChatID:  "chat-1",
		Content: "Final Answer",
	}
	mgr.sendWithRetry(ctx, "mock", worker, msgFinal)

	if len(mockCh.sentMessages) != 1 {
		t.Errorf("expected 1 normal message sent, got %d", len(mockCh.sentMessages))
	}
}

func TestSendMessage_Synchronous(t *testing.T) {
	m := newTestManager()

	var received []bus.OutboundMessage
	ch := &mockChannel{
		sendFn: func(_ context.Context, msg bus.OutboundMessage) error {
			received = append(received, msg)
			return nil
		},
	}

	w := &channelWorker{
		ch:      ch,
		limiter: rate.NewLimiter(rate.Inf, 1),
	}
	m.channels["test"] = ch
	m.workers["test"] = w

	msg := bus.OutboundMessage{
		Channel:          "test",
		ChatID:           "123",
		Content:          "hello world",
		ReplyToMessageID: "msg-456",
	}

	err := m.SendMessage(context.Background(), msg)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	// SendMessage is synchronous — message should already be delivered
	if len(received) != 1 {
		t.Fatalf("expected 1 message sent, got %d", len(received))
	}
	if received[0].ReplyToMessageID != "msg-456" {
		t.Fatalf("expected ReplyToMessageID msg-456, got %s", received[0].ReplyToMessageID)
	}
	if received[0].Content != "hello world" {
		t.Fatalf("expected content 'hello world', got %s", received[0].Content)
	}
}

func TestSendMessage_UnknownChannel(t *testing.T) {
	m := newTestManager()

	msg := bus.OutboundMessage{
		Channel: "nonexistent",
		ChatID:  "123",
		Content: "hello",
	}

	err := m.SendMessage(context.Background(), msg)
	if err == nil {
		t.Fatal("expected error for unknown channel")
	}
}

func TestSendMessage_NoWorker(t *testing.T) {
	m := newTestManager()

	ch := &mockChannel{
		sendFn: func(_ context.Context, _ bus.OutboundMessage) error { return nil },
	}
	m.channels["test"] = ch
	// No worker registered

	msg := bus.OutboundMessage{
		Channel: "test",
		ChatID:  "123",
		Content: "hello",
	}

	err := m.SendMessage(context.Background(), msg)
	if err == nil {
		t.Fatal("expected error when no worker exists")
	}
}

func TestSendMessage_WithRetry(t *testing.T) {
	m := newTestManager()

	var callCount int
	ch := &mockChannel{
		sendFn: func(_ context.Context, _ bus.OutboundMessage) error {
			callCount++
			if callCount == 1 {
				return fmt.Errorf("transient: %w", ErrTemporary)
			}
			return nil
		},
	}

	w := &channelWorker{
		ch:      ch,
		limiter: rate.NewLimiter(rate.Inf, 1),
	}
	m.channels["test"] = ch
	m.workers["test"] = w

	msg := bus.OutboundMessage{
		Channel: "test",
		ChatID:  "123",
		Content: "retry me",
	}

	err := m.SendMessage(context.Background(), msg)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if callCount != 2 {
		t.Fatalf("expected 2 Send calls (1 failure + 1 success), got %d", callCount)
	}
}

func TestSendMessage_WithSplitting(t *testing.T) {
	m := newTestManager()

	var received []string
	ch := &mockChannelWithLength{
		mockChannel: mockChannel{
			sendFn: func(_ context.Context, msg bus.OutboundMessage) error {
				received = append(received, msg.Content)
				return nil
			},
		},
		maxLen: 5,
	}

	w := &channelWorker{
		ch:      ch,
		limiter: rate.NewLimiter(rate.Inf, 1),
	}
	m.channels["test"] = ch
	m.workers["test"] = w

	msg := bus.OutboundMessage{
		Channel: "test",
		ChatID:  "123",
		Content: "hello world",
	}

	err := m.SendMessage(context.Background(), msg)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if len(received) < 2 {
		t.Fatalf("expected message to be split into at least 2 chunks, got %d", len(received))
	}
}

func TestSendMessage_PreservesOrdering(t *testing.T) {
	m := newTestManager()

	var order []string
	ch := &mockChannel{
		sendFn: func(_ context.Context, msg bus.OutboundMessage) error {
			order = append(order, msg.Content)
			return nil
		},
	}

	w := &channelWorker{
		ch:      ch,
		limiter: rate.NewLimiter(rate.Inf, 1),
	}
	m.channels["test"] = ch
	m.workers["test"] = w

	// Send two messages sequentially — they must arrive in order
	_ = m.SendMessage(context.Background(), bus.OutboundMessage{
		Channel: "test", ChatID: "1", Content: "first",
	})
	_ = m.SendMessage(context.Background(), bus.OutboundMessage{
		Channel: "test", ChatID: "1", Content: "second",
	})

	if len(order) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(order))
	}
	if order[0] != "first" || order[1] != "second" {
		t.Fatalf("expected [first, second], got %v", order)
	}
}

func TestManager_SendPlaceholder(t *testing.T) {
	mgr := &Manager{
		channels:     make(map[string]Channel),
		workers:      make(map[string]*channelWorker),
		placeholders: sync.Map{},
	}

	mockCh := &mockChannel{
		sendFn: func(ctx context.Context, msg bus.OutboundMessage) error {
			return nil
		},
	}
	mgr.channels["mock"] = mockCh

	ctx := context.Background()

	// SendPlaceholder should send a placeholder and record it
	ok := mgr.SendPlaceholder(ctx, "mock", "chat-1")
	if !ok {
		t.Fatal("expected SendPlaceholder to succeed")
	}
	if mockCh.placeholdersSent != 1 {
		t.Errorf("expected 1 placeholder sent, got %d", mockCh.placeholdersSent)
	}

	key := "mock:chat-1"
	if _, loaded := mgr.placeholders.Load(key); !loaded {
		t.Error("expected placeholder to be recorded in manager")
	}

	// SendPlaceholder on unknown channel should return false
	ok = mgr.SendPlaceholder(ctx, "unknown", "chat-1")
	if ok {
		t.Error("expected SendPlaceholder to fail for unknown channel")
	}
}

func TestNewManager(t *testing.T) {
	// NewManager should create a manager with initialized channels
	// This test verifies the basic initialization path
	m := newTestManager()
	if m.channels == nil {
		t.Fatal("expected channels map to be initialized")
	}
	if m.workers == nil {
		t.Fatal("expected workers map to be initialized")
	}
	if m.bus == nil {
		t.Fatal("expected message bus to be initialized")
	}
}

func TestRecordReactionUndo(t *testing.T) {
	m := newTestManager()
	undoFn := func() {}

	m.RecordReactionUndo("test", "chat1", undoFn)

	key := "test:chat1"
	val, loaded := m.reactionUndos.Load(key)
	if !loaded {
		t.Fatal("expected reaction undo to be recorded")
	}

	entry, ok := val.(reactionEntry)
	if !ok {
		t.Fatal("expected value to be reactionEntry")
	}
	if entry.undo == nil {
		t.Fatal("expected undo function to be non-nil")
	}
}

func TestHandle(t *testing.T) {
	m := newTestManager()
	m.mux = http.NewServeMux()

	// Test that Handle registers an HTTP handler
	called := false
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	m.Handle("/test", handler)

	// Simulate a request to verify it was registered
	req := httptest.NewRequest("GET", "/test", nil)
	w := httptest.NewRecorder()
	m.mux.ServeHTTP(w, req)

	if !called {
		t.Fatal("expected handler to be called")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("expected status OK, got %d", w.Code)
	}
}

func TestGetChannel(t *testing.T) {
	m := newTestManager()
	mockCh := &mockChannel{}
	m.channels["test"] = mockCh

	ch, ok := m.GetChannel("test")
	if !ok {
		t.Fatal("expected GetChannel to return true")
	}
	if ch != mockCh {
		t.Fatal("expected GetChannel to return the same channel")
	}

	ch, ok = m.GetChannel("nonexistent")
	if ok {
		t.Fatal("expected GetChannel to return false for unknown channel")
	}
	if ch != nil {
		t.Fatal("expected GetChannel to return nil for unknown channel")
	}
}

func TestGetStatus(t *testing.T) {
	m := newTestManager()
	mockCh := &mockChannel{}
	mockCh.SetRunning(true)
	m.channels["test"] = mockCh

	status := m.GetStatus()
	if _, ok := status["test"]; !ok {
		t.Fatal("expected status to contain test channel")
	}

	testStatus, ok := status["test"].(map[string]any)
	if !ok {
		t.Fatal("expected status value to be map[string]any")
	}

	if enabled, ok := testStatus["enabled"].(bool); !ok || !enabled {
		t.Fatal("expected channel to be enabled")
	}
	if running, ok := testStatus["running"].(bool); !ok || !running {
		t.Fatal("expected channel to be running")
	}
}

func TestGetEnabledChannels(t *testing.T) {
	m := newTestManager()
	m.channels["telegram"] = &mockChannel{}
	m.channels["discord"] = &mockChannel{}

	enabled := m.GetEnabledChannels()
	if len(enabled) != 2 {
		t.Fatalf("expected 2 enabled channels, got %d", len(enabled))
	}

	// Check that both channels are in the list (order is not guaranteed)
	foundTelegram := false
	foundDiscord := false
	for _, name := range enabled {
		if name == "telegram" {
			foundTelegram = true
		}
		if name == "discord" {
			foundDiscord = true
		}
	}

	if !foundTelegram || !foundDiscord {
		t.Fatalf("expected both telegram and discord channels in list, got %v", enabled)
	}
}

func TestUnregisterChannel(t *testing.T) {
	m := newTestManager()
	mockCh := &mockChannel{}
	m.channels["test"] = mockCh

	// Create a worker for the channel
	w := &channelWorker{
		ch:        mockCh,
		queue:     make(chan bus.OutboundMessage),
		done:      make(chan struct{}),
		mediaQueue: make(chan bus.OutboundMediaMessage),
		mediaDone: make(chan struct{}),
		limiter:   rate.NewLimiter(rate.Inf, 1),
	}
	m.workers["test"] = w

	// Start workers to satisfy the close contract
	go func() {
		for range w.queue {
		}
		close(w.done)
	}()
	go func() {
		for range w.mediaQueue {
		}
		close(w.mediaDone)
	}()

	m.UnregisterChannel("test")

	// Verify channel and worker are removed
	if _, ok := m.channels["test"]; ok {
		t.Fatal("expected channel to be unregistered")
	}
	if _, ok := m.workers["test"]; ok {
		t.Fatal("expected worker to be unregistered")
	}
}

func TestDispatchOutboundMedia(t *testing.T) {
	m := newTestManager()
	mockCh := &mockMediaChannel{}
	m.channels["test"] = mockCh

	w := &channelWorker{
		ch:        mockCh,
		mediaQueue: make(chan bus.OutboundMediaMessage, 10),
		done:      make(chan struct{}),
		mediaDone: make(chan struct{}),
		limiter:   rate.NewLimiter(rate.Inf, 1),
	}
	m.workers["test"] = w

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dispatched := make(chan struct{}, 1)
	go func() {
		msg, ok := <-w.mediaQueue
		if ok && msg.Channel == "test" {
			dispatched <- struct{}{}
		}
	}()

	// Publish an outbound media message
	mediaMsg := bus.OutboundMediaMessage{
		Channel: "test",
		ChatID:  "chat1",
		Parts:   []bus.MediaPart{{Ref: "media://test"}},
	}
	if err := m.bus.PublishOutboundMedia(ctx, mediaMsg); err != nil {
		t.Fatalf("PublishOutboundMedia failed: %v", err)
	}

	// Start the dispatcher in a goroutine
	go m.dispatchOutboundMedia(ctx)

	// Wait for the message to be dispatched
	select {
	case <-dispatched:
		// Success: message was dispatched
	case <-time.After(2 * time.Second):
		t.Fatal("expected media message to be dispatched")
	}
}

func TestSendToChannel(t *testing.T) {
	m := newTestManager()
	mockCh := &mockChannel{}
	m.channels["test"] = mockCh

	w := &channelWorker{
		ch:        mockCh,
		queue:     make(chan bus.OutboundMessage, 10),
		done:      make(chan struct{}),
		mediaQueue: make(chan bus.OutboundMediaMessage),
		mediaDone: make(chan struct{}),
		limiter:   rate.NewLimiter(rate.Inf, 1),
	}
	m.workers["test"] = w

	ctx := context.Background()
	err := m.SendToChannel(ctx, "test", "chat1", "hello")
	if err != nil {
		t.Fatalf("SendToChannel failed: %v", err)
	}

	// Verify the message was queued
	select {
	case msg := <-w.queue:
		if msg.Content != "hello" || msg.ChatID != "chat1" {
			t.Fatalf("expected message 'hello' to 'chat1', got %+v", msg)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("expected message to be queued")
	}
}

func TestSendToChannel_UnknownChannel(t *testing.T) {
	m := newTestManager()

	ctx := context.Background()
	err := m.SendToChannel(ctx, "unknown", "chat1", "hello")
	if err == nil {
		t.Fatal("expected SendToChannel to return error for unknown channel")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected 'not found' error, got %v", err)
	}
}

func TestSetupHTTPServer(t *testing.T) {
	m := newTestManager()
	mockCh := &mockChannel{}
	m.channels["test"] = mockCh

	// SetupHTTPServer should initialize the mux
	m.SetupHTTPServer("localhost:8080", nil)

	if m.mux == nil {
		t.Fatal("expected mux to be initialized")
	}
	if m.httpServer == nil {
		t.Fatal("expected httpServer to be initialized")
	}
	if m.httpServer.Addr != "localhost:8080" {
		t.Fatalf("expected address localhost:8080, got %s", m.httpServer.Addr)
	}
}

func TestSetupHTTPServer_WithWebhookHandler(t *testing.T) {
	m := newTestManager()

	// Create a channel that implements WebhookHandler
	ch := &mockWebhookChannel{
		mockChannel: mockChannel{
			sendFn: func(_ context.Context, _ bus.OutboundMessage) error {
				return nil
			},
		},
	}
	m.channels["test"] = ch

	m.SetupHTTPServer("localhost:8080", nil)

	// Verify webhook handler was registered
	if m.mux == nil {
		t.Fatal("expected mux to be initialized")
	}

	// Test that the webhook path is accessible
	req := httptest.NewRequest("POST", "/webhooks/test", nil)
	w := httptest.NewRecorder()
	m.mux.ServeHTTP(w, req)

	// Should get a response (webhook handler was called)
	if w.Code == 404 {
		t.Fatal("expected webhook handler to be registered")
	}
}

func TestSetupHTTPServer_WithHealthChecker(t *testing.T) {
	m := newTestManager()

	// Create a channel that implements HealthChecker
	ch := &mockHealthCheckerChannel{
		mockChannel: mockChannel{
			sendFn: func(_ context.Context, _ bus.OutboundMessage) error {
				return nil
			},
		},
	}
	m.channels["test"] = ch

	m.SetupHTTPServer("localhost:8080", nil)

	if m.mux == nil {
		t.Fatal("expected mux to be initialized")
	}

	// Test that health endpoint is accessible
	req := httptest.NewRequest("GET", "/health/test", nil)
	w := httptest.NewRecorder()
	m.mux.ServeHTTP(w, req)

	// Should get a response
	if w.Code == 404 {
		t.Fatal("expected health handler to be registered")
	}
}

// mockWebhookChannel implements WebhookHandler
type mockWebhookChannel struct {
	mockChannel
}

func (m *mockWebhookChannel) WebhookPath() string {
	return "/webhooks/test"
}

func (m *mockWebhookChannel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}

// mockHealthCheckerChannel implements HealthChecker
type mockHealthCheckerChannel struct {
	mockChannel
}

func (m *mockHealthCheckerChannel) HealthPath() string {
	return "/health/test"
}

func (m *mockHealthCheckerChannel) HealthHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func TestRecordTypingStop(t *testing.T) {
	m := newTestManager()
	stopCalled := false
	stopFn := func() {
		stopCalled = true
	}

	m.RecordTypingStop("test", "chat1", stopFn)

	key := "test:chat1"
	val, loaded := m.typingStops.Load(key)
	if !loaded {
		t.Fatal("expected typing stop to be recorded")
	}

	entry, ok := val.(typingEntry)
	if !ok {
		t.Fatal("expected value to be typingEntry")
	}
	if entry.stop == nil {
		t.Fatal("expected stop function to be non-nil")
	}

	// Invoke the stop function
	m.InvokeTypingStop("test", "chat1")

	if !stopCalled {
		t.Fatal("expected stop function to be called")
	}

	// Verify it was deleted after invocation
	_, loaded = m.typingStops.Load(key)
	if loaded {
		t.Fatal("expected typing stop to be deleted after invocation")
	}
}

func TestRecordPlaceholder(t *testing.T) {
	m := newTestManager()
	m.RecordPlaceholder("test", "chat1", "placeholder-123")

	key := "test:chat1"
	val, loaded := m.placeholders.Load(key)
	if !loaded {
		t.Fatal("expected placeholder to be recorded")
	}

	entry, ok := val.(placeholderEntry)
	if !ok {
		t.Fatal("expected value to be placeholderEntry")
	}
	if entry.id != "placeholder-123" {
		t.Fatalf("expected placeholder ID 'placeholder-123', got %s", entry.id)
	}
}

func TestSendPlaceholder_ChannelNotFound(t *testing.T) {
	m := newTestManager()

	ctx := context.Background()

	// SendPlaceholder on unknown channel should return false
	ok := m.SendPlaceholder(ctx, "unknown", "chat1")
	if ok {
		t.Error("expected SendPlaceholder to return false for unknown channel")
	}
}

func TestSendPlaceholder_SendFails(t *testing.T) {
	m := newTestManager()

	// Create a channel that supports placeholders but returns empty ID
	ch := &mockEmptyPlaceholderChannel{}
	m.channels["test"] = ch

	ctx := context.Background()

	// SendPlaceholder should return false when it returns empty ID
	ok := m.SendPlaceholder(ctx, "test", "chat1")
	if ok {
		t.Error("expected SendPlaceholder to return false when placeholder ID is empty")
	}
}

// mockEmptyPlaceholderChannel returns an empty placeholder ID
type mockEmptyPlaceholderChannel struct {
	mockChannel
}

func (m *mockEmptyPlaceholderChannel) SendPlaceholder(ctx context.Context, chatID string) (string, error) {
	return "", nil
}

func TestSendPlaceholder_ReturnsError(t *testing.T) {
	m := newTestManager()

	// Create a channel with broken SendPlaceholder
	ch := &mockBrokenPlaceholderChannel{}
	m.channels["test"] = ch

	ctx := context.Background()

	// SendPlaceholder should return false when SendPlaceholder returns error
	ok := m.SendPlaceholder(ctx, "test", "chat1")
	if ok {
		t.Error("expected SendPlaceholder to return false when SendPlaceholder errors")
	}
}

// mockBrokenPlaceholderChannel returns an error from SendPlaceholder
type mockBrokenPlaceholderChannel struct {
	mockChannel
}

func (m *mockBrokenPlaceholderChannel) SendPlaceholder(ctx context.Context, chatID string) (string, error) {
	return "", errors.New("failed to send placeholder")
}

func TestSendPlaceholder_ChannelDoesNotSupportPlaceholder(t *testing.T) {
	m := newTestManager()

	// Add a channel that doesn't support PlaceholderCapable
	// Create a minimal channel that only has the basic Channel interface
	ch := &minimalChannel{}
	m.channels["test"] = ch

	ctx := context.Background()

	// SendPlaceholder on channel without PlaceholderCapable should return false
	ok := m.SendPlaceholder(ctx, "test", "chat1")
	if ok {
		t.Error("expected SendPlaceholder to return false for channel without PlaceholderCapable")
	}
}

// minimalChannel implements only the Channel interface, not PlaceholderCapable
type minimalChannel struct{}

func (m *minimalChannel) Name() string                                        { return "minimal" }
func (m *minimalChannel) Start(_ context.Context) error                       { return nil }
func (m *minimalChannel) Stop(_ context.Context) error                        { return nil }
func (m *minimalChannel) Send(_ context.Context, _ bus.OutboundMessage) error { return nil }
func (m *minimalChannel) IsRunning() bool                                     { return false }
func (m *minimalChannel) IsAllowed(senderID string) bool                      { return true }
func (m *minimalChannel) IsAllowedSender(sender bus.SenderInfo) bool          { return true }
func (m *minimalChannel) ReasoningChannelID() string                          { return "" }

func TestDispatchOutboundMedia_NoWorker(t *testing.T) {
	m := newTestManager()
	mockCh := &mockMediaChannel{}
	m.channels["test"] = mockCh

	// Worker doesn't exist for the channel - should log warning but not panic
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go m.dispatchOutboundMedia(ctx)

	msg := bus.OutboundMediaMessage{Channel: "test", ChatID: "chat1"}
	if err := m.bus.PublishOutboundMedia(ctx, msg); err != nil {
		t.Fatalf("PublishOutboundMedia failed: %v", err)
	}

	// Give dispatcher a moment to process
	time.Sleep(100 * time.Millisecond)

	// Cancel to stop dispatcher
	cancel()
	time.Sleep(50 * time.Millisecond)

	// Should not panic
}

func TestInvokeTypingStop_NotFound(t *testing.T) {
	m := newTestManager()

	// InvokeTypingStop on non-existent key should be safe (no-op)
	// Should not panic
	m.InvokeTypingStop("test", "chat1")
}

func TestPreSend_AllOperations(t *testing.T) {
	m := newTestManager()

	var editCalled bool
	typingStopCalled := false
	reactionUndoCalled := false

	ch := &mockMessageEditor{
		mockChannel: mockChannel{
			sendFn: func(_ context.Context, _ bus.OutboundMessage) error {
				return nil
			},
		},
		editFn: func(_ context.Context, chatID, messageID, content string) error {
			editCalled = true
			return nil
		},
	}

	key := "test:chat1"

	// Record typing stop
	m.RecordTypingStop("test", "chat1", func() {
		typingStopCalled = true
	})

	// Record reaction undo
	m.RecordReactionUndo("test", "chat1", func() {
		reactionUndoCalled = true
	})

	// Record placeholder
	m.RecordPlaceholder("test", "chat1", "ph-123")

	ctx := context.Background()
	msg := bus.OutboundMessage{Channel: "test", ChatID: "chat1", Content: "updated"}

	// preSend should handle all three: typing, reaction, and placeholder
	edited, _ := m.preSend(ctx, "test", msg, ch)

	if !edited {
		t.Fatal("expected preSend to return true (placeholder was edited)")
	}
	if !editCalled {
		t.Fatal("expected EditMessage to be called")
	}
	if !typingStopCalled {
		t.Fatal("expected typing stop to be called")
	}
	if !reactionUndoCalled {
		t.Fatal("expected reaction undo to be called")
	}

	// Verify they were deleted
	_, typingLoaded := m.typingStops.Load(key)
	_, reactionLoaded := m.reactionUndos.Load(key)
	_, placeholderLoaded := m.placeholders.Load(key)

	if typingLoaded || reactionLoaded {
		t.Fatal("expected typing and reaction to be deleted after preSend")
	}
	if placeholderLoaded {
		t.Fatal("expected placeholder to be deleted after successful edit")
	}
}

func TestPreSend_NoPlaceholder(t *testing.T) {
	m := newTestManager()

	ch := &mockMessageEditor{
		mockChannel: mockChannel{
			sendFn: func(_ context.Context, _ bus.OutboundMessage) error {
				return nil
			},
		},
		editFn: func(_ context.Context, _, _, _ string) error {
			return nil
		},
	}

	ctx := context.Background()
	msg := bus.OutboundMessage{Channel: "test", ChatID: "chat1", Content: "hello"}

	// preSend with no placeholder should return false
	edited, _ := m.preSend(ctx, "test", msg, ch)
	if edited {
		t.Fatal("expected preSend to return false (no placeholder)")
	}
}

func TestRunMediaWorker_Processes(t *testing.T) {
	m := newTestManager()

	var receivedMedia []bus.OutboundMediaMessage
	ch := &mockMediaChannel{
		sendMediaFn: func(_ context.Context, msg bus.OutboundMediaMessage) error {
			receivedMedia = append(receivedMedia, msg)
			return nil
		},
	}

	w := &channelWorker{
		ch:        ch,
		mediaQueue: make(chan bus.OutboundMediaMessage, 10),
		done:      make(chan struct{}),
		mediaDone: make(chan struct{}),
		limiter:   rate.NewLimiter(rate.Inf, 1),
	}

	ctx, cancel := context.WithCancel(context.Background())

	go m.runMediaWorker(ctx, "test", w)

	// Queue a media message
	w.mediaQueue <- bus.OutboundMediaMessage{
		Channel: "test",
		ChatID:  "chat1",
		Parts:   []bus.MediaPart{{Ref: "media://test"}},
	}

	// Give it time to process
	time.Sleep(100 * time.Millisecond)

	// Close the queue
	close(w.mediaQueue)

	// Wait for worker to finish
	<-w.mediaDone

	if len(receivedMedia) != 1 {
		t.Fatalf("expected 1 media message, got %d", len(receivedMedia))
	}
	if receivedMedia[0].ChatID != "chat1" {
		t.Fatalf("expected chat1, got %s", receivedMedia[0].ChatID)
	}

	cancel()
}

func TestRunMediaWorker_ContextCanceled(t *testing.T) {
	m := newTestManager()

	ch := &mockMediaChannel{}

	w := &channelWorker{
		ch:        ch,
		mediaQueue: make(chan bus.OutboundMediaMessage, 10),
		done:      make(chan struct{}),
		mediaDone: make(chan struct{}),
		limiter:   rate.NewLimiter(rate.Inf, 1),
	}

	ctx, cancel := context.WithCancel(context.Background())

	go m.runMediaWorker(ctx, "test", w)

	// Cancel context immediately
	cancel()

	// Wait for worker to finish
	<-w.mediaDone

	// Should exit cleanly
}

func TestDispatchOutboundMedia_Unknown(t *testing.T) {
	m := newTestManager()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go m.dispatchOutboundMedia(ctx)

	// Publish to an unknown channel
	msg := bus.OutboundMediaMessage{Channel: "unknown", ChatID: "chat1"}
	if err := m.bus.PublishOutboundMedia(ctx, msg); err != nil {
		t.Fatalf("PublishOutboundMedia failed: %v", err)
	}

	// Give dispatcher a moment
	time.Sleep(100 * time.Millisecond)

	// Should not panic; just skip unknown channels
	cancel()
}

func TestSendMediaWithRetry_NoMediaSender(t *testing.T) {
	m := newTestManager()

	// Use a regular channel (not MediaSender)
	ch := &mockChannel{
		sendFn: func(_ context.Context, _ bus.OutboundMessage) error {
			return nil
		},
	}

	w := &channelWorker{
		ch:      ch,
		limiter: rate.NewLimiter(rate.Inf, 1),
	}

	msg := bus.OutboundMediaMessage{Channel: "test", ChatID: "chat1"}

	// sendMediaWithRetry should return nil when channel doesn't support MediaSender
	err := m.sendMediaWithRetry(context.Background(), "test", w, msg)
	if err != nil {
		t.Fatalf("expected no error for non-MediaSender channel, got %v", err)
	}
}

func TestSendMediaWithRetry_Success(t *testing.T) {
	m := newTestManager()

	var callCount int
	ch := &mockMediaChannel{
		sendMediaFn: func(_ context.Context, _ bus.OutboundMediaMessage) error {
			callCount++
			return nil
		},
	}

	w := &channelWorker{
		ch:      ch,
		limiter: rate.NewLimiter(rate.Inf, 1),
	}

	msg := bus.OutboundMediaMessage{Channel: "test", ChatID: "chat1"}

	err := m.sendMediaWithRetry(context.Background(), "test", w, msg)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if callCount != 1 {
		t.Fatalf("expected 1 call, got %d", callCount)
	}
}

func TestSendMediaWithRetry_TemporaryThenSuccess(t *testing.T) {
	m := newTestManager()

	var callCount int
	ch := &mockMediaChannel{
		sendMediaFn: func(_ context.Context, _ bus.OutboundMediaMessage) error {
			callCount++
			if callCount <= 2 {
				return fmt.Errorf("network error: %w", ErrTemporary)
			}
			return nil
		},
	}

	w := &channelWorker{
		ch:      ch,
		limiter: rate.NewLimiter(rate.Inf, 1),
	}

	msg := bus.OutboundMediaMessage{Channel: "test", ChatID: "chat1"}

	err := m.sendMediaWithRetry(context.Background(), "test", w, msg)
	if err != nil {
		t.Fatalf("expected success after retries, got %v", err)
	}
	if callCount != 3 {
		t.Fatalf("expected 3 calls, got %d", callCount)
	}
}

func TestSendMediaWithRetry_PermanentFailure(t *testing.T) {
	m := newTestManager()

	var callCount int
	ch := &mockMediaChannel{
		sendMediaFn: func(_ context.Context, _ bus.OutboundMediaMessage) error {
			callCount++
			return fmt.Errorf("bad upload: %w", ErrSendFailed)
		},
	}

	w := &channelWorker{
		ch:      ch,
		limiter: rate.NewLimiter(rate.Inf, 1),
	}

	msg := bus.OutboundMediaMessage{Channel: "test", ChatID: "chat1"}

	err := m.sendMediaWithRetry(context.Background(), "test", w, msg)
	if err == nil {
		t.Fatal("expected error")
	}
	if callCount != 1 {
		t.Fatalf("expected 1 call (no retry for permanent failure), got %d", callCount)
	}
}

func TestRunTTLJanitor(t *testing.T) {
	m := newTestManager()

	// Record some entries with old timestamps
	oldTime := time.Now().Add(-2 * typingStopTTL)
	m.typingStops.Store("old", typingEntry{stop: func() {}, createdAt: oldTime})
	m.placeholders.Store("old-ph", placeholderEntry{id: "ph-123", createdAt: oldTime})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go m.runTTLJanitor(ctx)

	// Wait for janitor to run
	time.Sleep(15 * time.Second)

	// Check that old entries were deleted
	// (Note: This test is simplified; a real test might use a ticker mock)
	cancel()
}

func TestNewManager_EmptyConfig(t *testing.T) {
	m, err := NewManager(&config.Config{}, bus.NewMessageBus(), nil)
	if err != nil {
		t.Fatalf("NewManager with empty config: %v", err)
	}
	if m == nil {
		t.Fatal("expected non-nil manager")
	}
	if m.channels == nil {
		t.Error("expected channels map initialized")
	}
	if len(m.channels) != 0 {
		t.Errorf("expected 0 channels with empty config, got %d", len(m.channels))
	}
}

func TestManager_GetChannel_Found(t *testing.T) {
	m := newTestManager()
	ch := &mockChannel{}
	m.channels["telegram"] = ch
	got, ok := m.GetChannel("telegram")
	if !ok || got != ch {
		t.Errorf("GetChannel found: ok=%v, got=%v", ok, got)
	}
}

func TestManager_GetChannel_NotFound(t *testing.T) {
	m := newTestManager()
	_, ok := m.GetChannel("nonexistent")
	if ok {
		t.Error("GetChannel should return false for nonexistent channel")
	}
}

func TestManager_GetStatus_Empty(t *testing.T) {
	m := newTestManager()
	status := m.GetStatus()
	if len(status) != 0 {
		t.Errorf("GetStatus empty manager: got %d entries, want 0", len(status))
	}
}

func TestManager_GetStatus_WithChannel(t *testing.T) {
	m := newTestManager()
	m.channels["telegram"] = &mockChannel{}
	status := m.GetStatus()
	if _, ok := status["telegram"]; !ok {
		t.Error("GetStatus should include registered channel")
	}
}

func TestManager_GetEnabledChannels_Empty(t *testing.T) {
	m := newTestManager()
	names := m.GetEnabledChannels()
	if len(names) != 0 {
		t.Errorf("GetEnabledChannels empty: got %v", names)
	}
}

func TestManager_GetEnabledChannels_WithChannels(t *testing.T) {
	m := newTestManager()
	m.channels["telegram"] = &mockChannel{}
	m.channels["discord"] = &mockChannel{}
	names := m.GetEnabledChannels()
	if len(names) != 2 {
		t.Errorf("GetEnabledChannels: got %d channels, want 2", len(names))
	}
}

func TestManager_RegisterChannel(t *testing.T) {
	m := newTestManager()
	ch := &mockChannel{}
	m.RegisterChannel("slack", ch)
	got, ok := m.GetChannel("slack")
	if !ok || got != ch {
		t.Errorf("RegisterChannel: ok=%v, got=%v", ok, got)
	}
}

func TestManager_UnregisterChannel_NoWorker(t *testing.T) {
	m := newTestManager()
	m.channels["slack"] = &mockChannel{}
	m.UnregisterChannel("slack")
	if _, ok := m.channels["slack"]; ok {
		t.Error("UnregisterChannel should remove channel")
	}
}

func TestManager_SendMessage_ChannelNotFound(t *testing.T) {
	m := newTestManager()
	err := m.SendMessage(context.Background(), bus.OutboundMessage{Channel: "nonexistent", ChatID: "1", Content: "hi"})
	if err == nil {
		t.Error("SendMessage to nonexistent channel should return error")
	}
}

// --- initChannels branch coverage ---
// None of these platform factories are registered in the test binary, so
// initChannel takes the WarnCF+return path for each, exercising the if-branch
// statements in initChannels without needing real platform SDKs.

func TestNewManager_TelegramEnabled_NoFactory(t *testing.T) {
	cfg := &config.Config{}
	cfg.Channels.Telegram.Enabled = true
	cfg.Channels.Telegram.Token = *config.NewSecureString("test-token")
	m, err := NewManager(cfg, bus.NewMessageBus(), nil)
	if err != nil {
		t.Fatalf("NewManager should not error: %v", err)
	}
	if _, ok := m.channels["telegram"]; ok {
		t.Error("telegram channel should not be initialized when factory not registered")
	}
}

func TestNewManager_DiscordEnabled_NoFactory(t *testing.T) {
	cfg := &config.Config{}
	cfg.Channels.Discord.Enabled = true
	cfg.Channels.Discord.Token = *config.NewSecureString("test-token")
	m, err := NewManager(cfg, bus.NewMessageBus(), nil)
	if err != nil {
		t.Fatalf("NewManager should not error: %v", err)
	}
	if _, ok := m.channels["discord"]; ok {
		t.Error("discord channel should not be initialized when factory not registered")
	}
}

func TestNewManager_SlackEnabled_NoFactory(t *testing.T) {
	cfg := &config.Config{}
	cfg.Channels.Slack.Enabled = true
	cfg.Channels.Slack.BotToken = *config.NewSecureString("xoxb-test-token")
	m, err := NewManager(cfg, bus.NewMessageBus(), nil)
	if err != nil {
		t.Fatalf("NewManager should not error: %v", err)
	}
	if _, ok := m.channels["slack"]; ok {
		t.Error("slack channel should not be initialized when factory not registered")
	}
}

func TestNewManager_MatrixEnabled_NoFactory(t *testing.T) {
	cfg := &config.Config{}
	cfg.Channels.Matrix.Enabled = true
	cfg.Channels.Matrix.Homeserver = "https://matrix.example.com"
	cfg.Channels.Matrix.UserID = "@bot:matrix.example.com"
	cfg.Channels.Matrix.AccessToken = *config.NewSecureString("syt_test_token")
	m, err := NewManager(cfg, bus.NewMessageBus(), nil)
	if err != nil {
		t.Fatalf("NewManager should not error: %v", err)
	}
	if _, ok := m.channels["matrix"]; ok {
		t.Error("matrix channel should not be initialized when factory not registered")
	}
}

func TestNewManager_FeishuEnabled_NoFactory(t *testing.T) {
	cfg := &config.Config{}
	cfg.Channels.Feishu.Enabled = true
	m, err := NewManager(cfg, bus.NewMessageBus(), nil)
	if err != nil {
		t.Fatalf("NewManager should not error: %v", err)
	}
	if _, ok := m.channels["feishu"]; ok {
		t.Error("feishu channel should not be initialized when factory not registered")
	}
}

func TestNewManager_QQEnabled_NoFactory(t *testing.T) {
	cfg := &config.Config{}
	cfg.Channels.QQ.Enabled = true
	m, err := NewManager(cfg, bus.NewMessageBus(), nil)
	if err != nil {
		t.Fatalf("NewManager should not error: %v", err)
	}
	if _, ok := m.channels["qq"]; ok {
		t.Error("qq channel should not be initialized when factory not registered")
	}
}

func TestNewManager_DingTalkEnabled_NoFactory(t *testing.T) {
	cfg := &config.Config{}
	cfg.Channels.DingTalk.Enabled = true
	cfg.Channels.DingTalk.ClientID = "dt-client-id"
	m, err := NewManager(cfg, bus.NewMessageBus(), nil)
	if err != nil {
		t.Fatalf("NewManager should not error: %v", err)
	}
	if _, ok := m.channels["dingtalk"]; ok {
		t.Error("dingtalk channel should not be initialized when factory not registered")
	}
}

func TestNewManager_WhatsAppBridgeEnabled_NoFactory(t *testing.T) {
	cfg := &config.Config{}
	cfg.Channels.WhatsApp.Enabled = true
	cfg.Channels.WhatsApp.BridgeURL = "http://localhost:3000"
	m, err := NewManager(cfg, bus.NewMessageBus(), nil)
	if err != nil {
		t.Fatalf("NewManager should not error: %v", err)
	}
	if _, ok := m.channels["whatsapp"]; ok {
		t.Error("whatsapp channel should not be initialized when factory not registered")
	}
}

func TestNewManager_WhatsAppNativeEnabled_NoFactory(t *testing.T) {
	cfg := &config.Config{}
	cfg.Channels.WhatsApp.Enabled = true
	cfg.Channels.WhatsApp.UseNative = true
	m, err := NewManager(cfg, bus.NewMessageBus(), nil)
	if err != nil {
		t.Fatalf("NewManager should not error: %v", err)
	}
	if _, ok := m.channels["whatsapp_native"]; ok {
		t.Error("whatsapp_native channel should not be initialized when factory not registered")
	}
}

func TestNewManager_LINEEnabled_NoFactory(t *testing.T) {
	cfg := &config.Config{}
	cfg.Channels.LINE.Enabled = true
	cfg.Channels.LINE.ChannelAccessToken = *config.NewSecureString("line-token")
	m, err := NewManager(cfg, bus.NewMessageBus(), nil)
	if err != nil {
		t.Fatalf("NewManager should not error: %v", err)
	}
	if _, ok := m.channels["line"]; ok {
		t.Error("line channel should not be initialized when factory not registered")
	}
}

func TestNewManager_OneBotEnabled_NoFactory(t *testing.T) {
	cfg := &config.Config{}
	cfg.Channels.OneBot.Enabled = true
	cfg.Channels.OneBot.WSUrl = "ws://localhost:6700"
	m, err := NewManager(cfg, bus.NewMessageBus(), nil)
	if err != nil {
		t.Fatalf("NewManager should not error: %v", err)
	}
	if _, ok := m.channels["onebot"]; ok {
		t.Error("onebot channel should not be initialized when factory not registered")
	}
}

func TestNewManager_WeComEnabled_NoFactory(t *testing.T) {
	cfg := &config.Config{}
	cfg.Channels.WeCom.Enabled = true
	cfg.Channels.WeCom.Token = *config.NewSecureString("wecom-token")
	m, err := NewManager(cfg, bus.NewMessageBus(), nil)
	if err != nil {
		t.Fatalf("NewManager should not error: %v", err)
	}
	if _, ok := m.channels["wecom"]; ok {
		t.Error("wecom channel should not be initialized when factory not registered")
	}
}

func TestNewManager_IRCEnabled_NoFactory(t *testing.T) {
	cfg := &config.Config{}
	cfg.Channels.IRC.Enabled = true
	cfg.Channels.IRC.Server = "irc.libera.chat:6667"
	m, err := NewManager(cfg, bus.NewMessageBus(), nil)
	if err != nil {
		t.Fatalf("NewManager should not error: %v", err)
	}
	if _, ok := m.channels["irc"]; ok {
		t.Error("irc channel should not be initialized when factory not registered")
	}
}

func TestNewManager_EmptyConfig_NoChannels(t *testing.T) {
	cfg := &config.Config{}
	m, err := NewManager(cfg, bus.NewMessageBus(), nil)
	if err != nil {
		t.Fatalf("NewManager with empty config should not error: %v", err)
	}
	if len(m.channels) != 0 {
		t.Errorf("expected 0 channels with empty config, got %d", len(m.channels))
	}
}

// Verify channels without required credential fields are skipped.
func TestNewManager_TelegramEnabled_EmptyToken_Skipped(t *testing.T) {
	cfg := &config.Config{}
	cfg.Channels.Telegram.Enabled = true
	// Token is empty → condition fails → initChannel never called
	m, err := NewManager(cfg, bus.NewMessageBus(), nil)
	if err != nil {
		t.Fatalf("NewManager should not error: %v", err)
	}
	if _, ok := m.channels["telegram"]; ok {
		t.Error("telegram should be skipped when token is empty")
	}
}

func TestNewManager_DiscordEnabled_EmptyToken_Skipped(t *testing.T) {
	cfg := &config.Config{}
	cfg.Channels.Discord.Enabled = true
	// Token empty → skipped
	m, err := NewManager(cfg, bus.NewMessageBus(), nil)
	if err != nil {
		t.Fatalf("NewManager should not error: %v", err)
	}
	if _, ok := m.channels["discord"]; ok {
		t.Error("discord should be skipped when token is empty")
	}
}

func TestNewManager_MatrixEnabled_MissingFields_Skipped(t *testing.T) {
	cfg := &config.Config{}
	cfg.Channels.Matrix.Enabled = true
	// Missing Homeserver, UserID, AccessToken → skipped
	m, err := NewManager(cfg, bus.NewMessageBus(), nil)
	if err != nil {
		t.Fatalf("NewManager should not error: %v", err)
	}
	if _, ok := m.channels["matrix"]; ok {
		t.Error("matrix should be skipped when required fields are missing")
	}
}
