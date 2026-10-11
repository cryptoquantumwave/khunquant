package channels

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// recChannel is a thread-safe plain channel (Send only) that records what it
// was asked to send. maxLen is advertised through MessageLengthProvider.
type recChannel struct {
	BaseChannel
	mu        sync.Mutex
	sent      []bus.OutboundMessage
	sendFn    func(ctx context.Context, msg bus.OutboundMessage) error
	maxLen    int
	edited    []string
	editedErr error
}

func (c *recChannel) Send(ctx context.Context, msg bus.OutboundMessage) error {
	c.mu.Lock()
	c.sent = append(c.sent, msg)
	fn := c.sendFn
	c.mu.Unlock()
	if fn != nil {
		return fn(ctx, msg)
	}
	return nil
}

func (c *recChannel) Start(context.Context) error { return nil }
func (c *recChannel) Stop(context.Context) error  { return nil }
func (c *recChannel) MaxMessageLength() int       { return c.maxLen }

func (c *recChannel) EditMessage(_ context.Context, _, messageID, _ string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.edited = append(c.edited, messageID)
	return c.editedErr
}

func (c *recChannel) snapshot() []bus.OutboundMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]bus.OutboundMessage(nil), c.sent...)
}

// idChannel also implements IDReportingSender. Every platform message it
// creates gets the ID "id-N". It counts Send separately so a test can prove the
// manager never calls both.
type idChannel struct {
	recChannel
	idMu        sync.Mutex
	next        int
	sendWithIDs []bus.OutboundMessage
	idFn        func(ctx context.Context, msg bus.OutboundMessage) error
}

func (c *idChannel) SendWithIDs(ctx context.Context, msg bus.OutboundMessage) ([]string, error) {
	c.idMu.Lock()
	c.next++
	id := fmt.Sprintf("id-%d", c.next)
	c.sendWithIDs = append(c.sendWithIDs, msg)
	fn := c.idFn
	c.idMu.Unlock()
	if fn != nil {
		if err := fn(ctx, msg); err != nil {
			return nil, err
		}
	}
	return []string{id}, nil
}

func (c *idChannel) idSnapshot() []bus.OutboundMessage {
	c.idMu.Lock()
	defer c.idMu.Unlock()
	return append([]bus.OutboundMessage(nil), c.sendWithIDs...)
}

// rendererChannel can show real buttons.
type rendererChannel struct {
	idChannel
	supports bool
}

func (c *rendererChannel) SupportsActions() bool { return c.supports }

// newDeliveryManager wires ch into a Manager with a started-looking worker and
// an unlimited rate limiter.
func newDeliveryManager(ch Channel) (*Manager, *channelWorker) {
	m := newTestManager()
	w := &channelWorker{
		ch:      ch,
		queue:   make(chan bus.OutboundMessage, 16),
		done:    make(chan struct{}),
		limiter: rate.NewLimiter(rate.Inf, 1),
	}
	m.channels["test"] = ch
	m.workers["test"] = w
	return m, w
}

// observeInto registers an observer that forwards receipts to a buffered chan.
func observeInto(m *Manager) <-chan DeliveryReceipt {
	got := make(chan DeliveryReceipt, 16)
	m.SetDeliveryObserver(func(r DeliveryReceipt) { got <- r })
	return got
}

func waitReceipt(t *testing.T, got <-chan DeliveryReceipt) DeliveryReceipt {
	t.Helper()
	select {
	case r := <-got:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a delivery receipt")
		return DeliveryReceipt{}
	}
}

func approveReject() []bus.Action {
	return []bus.Action{
		{ID: "approve", Label: "Approve", Style: "primary", Payload: "a:7F3K", TextCommand: "/approve 7F3K"},
		{ID: "reject", Label: "Reject", Style: "danger", Payload: "r:7F3K", TextCommand: "/reject 7F3K"},
	}
}

func proactiveMsg(content string) bus.OutboundMessage {
	return bus.OutboundMessage{
		Channel:             "test",
		ChatID:              "1",
		Content:             content,
		Severity:            proactive.SeverityCritical,
		DedupKey:            "pnl:BTC",
		InterventionID:      "iv-1",
		Actions:             approveReject(),
		DisableNotification: true,
	}
}

func TestRunWorker_ActionsOnlyOnLastChunk(t *testing.T) {
	t.Run("length split", func(t *testing.T) {
		ch := &rendererChannel{supports: true}
		ch.maxLen = 12
		m, w := newDeliveryManager(ch)
		m.SetControlEnabled(true)
		got := observeInto(m)
		go m.runWorker(t.Context(), "test", w)

		w.queue <- proactiveMsg("alpha beta gamma delta epsilon zeta eta")
		waitReceipt(t, got)

		sent := ch.idSnapshot()
		if len(sent) < 3 {
			t.Fatalf("expected at least 3 chunks, got %d", len(sent))
		}
		for i, msg := range sent {
			last := i == len(sent)-1
			if last && (len(msg.Actions) != 2 || msg.InterventionID != "iv-1") {
				t.Errorf("last chunk lost its actions/intervention: %+v", msg)
			}
			if !last && (msg.Actions != nil || msg.InterventionID != "") {
				t.Errorf("chunk %d carries actions or intervention ID: %+v", i, msg)
			}
			// Metadata that is not about the buttons applies to every chunk.
			if msg.Severity != proactive.SeverityCritical || msg.DedupKey != "pnl:BTC" || !msg.DisableNotification {
				t.Errorf("chunk %d dropped severity/dedup/silent: %+v", i, msg)
			}
		}
	})

	t.Run("split marker", func(t *testing.T) {
		ch := &rendererChannel{supports: true}
		m, w := newDeliveryManager(ch)
		m.config = &config.Config{}
		m.config.Agents.Defaults.SplitOnMarker = true
		m.SetControlEnabled(true)
		got := observeInto(m)
		go m.runWorker(t.Context(), "test", w)

		w.queue <- proactiveMsg("one" + MessageSplitMarker + "two" + MessageSplitMarker + "three")
		waitReceipt(t, got)

		sent := ch.idSnapshot()
		if len(sent) != 3 {
			t.Fatalf("expected 3 marker parts, got %d", len(sent))
		}
		for i, msg := range sent {
			last := i == 2
			if last != (len(msg.Actions) == 2) || last != (msg.InterventionID == "iv-1") {
				t.Errorf("part %d: actions=%d intervention=%q", i, len(msg.Actions), msg.InterventionID)
			}
		}
	})
}

func TestRunWorker_TextFallbackFooter(t *testing.T) {
	const footer = "\n\n↳ /approve 7F3K · /reject 7F3K"

	run := func(t *testing.T, ch *recChannel, msg bus.OutboundMessage) []bus.OutboundMessage {
		t.Helper()
		m, w := newDeliveryManager(ch)
		m.SetControlEnabled(true)
		got := observeInto(m)
		go m.runWorker(t.Context(), "test", w)
		w.queue <- msg
		waitReceipt(t, got)
		return ch.snapshot()
	}

	t.Run("single message", func(t *testing.T) {
		sent := run(t, &recChannel{}, proactiveMsg("hello"))
		if len(sent) != 1 || sent[0].Content != "hello"+footer {
			t.Fatalf("got %+v", sent)
		}
	})

	t.Run("two chunks put the footer only on chunk 2", func(t *testing.T) {
		sent := run(t, &recChannel{maxLen: 80}, proactiveMsg(strings.Repeat("word ", 30)))
		if len(sent) < 2 {
			t.Fatalf("expected a split, got %d", len(sent))
		}
		for i, msg := range sent {
			has := strings.Contains(msg.Content, "↳")
			if has != (i == len(sent)-1) {
				t.Errorf("chunk %d footer=%v: %q", i, has, msg.Content)
			}
		}
	})

	t.Run("footer that would overflow the limit re-splits the last chunk", func(t *testing.T) {
		// 78 runes of content fit in 80, the 32-rune footer does not.
		content := strings.Repeat("word ", 15) + "abc"
		sent := run(t, &recChannel{maxLen: 80}, proactiveMsg(content))
		if len(sent) < 2 {
			t.Fatalf("expected the last chunk to be re-split, got %d chunk(s)", len(sent))
		}
		var joined strings.Builder
		for i, msg := range sent {
			if n := runeLen(msg.Content); n > 80 {
				t.Errorf("chunk %d is %d runes, over the 80 limit: %q", i, n, msg.Content)
			}
			joined.WriteString(msg.Content)
		}
		if !strings.HasSuffix(sent[len(sent)-1].Content, footer) {
			t.Errorf("the footer must end the final chunk whole: %q", sent[len(sent)-1].Content)
		}
		if strings.Count(joined.String(), "↳") != 1 {
			t.Errorf("the footer must appear exactly once: %q", joined.String())
		}
		if !strings.Contains(strings.ReplaceAll(joined.String(), "\n", ""), "abc") {
			t.Error("content was lost in the re-split")
		}
	})

	t.Run("empty text commands are skipped", func(t *testing.T) {
		msg := proactiveMsg("hello")
		msg.Actions[0].TextCommand = ""
		sent := run(t, &recChannel{}, msg)
		if sent[0].Content != "hello\n\n↳ /reject 7F3K" {
			t.Fatalf("got %q", sent[0].Content)
		}
	})

	t.Run("no text commands means no footer", func(t *testing.T) {
		msg := proactiveMsg("hello")
		msg.Actions[0].TextCommand = ""
		msg.Actions[1].TextCommand = "  "
		sent := run(t, &recChannel{}, msg)
		if sent[0].Content != "hello" {
			t.Fatalf("got %q", sent[0].Content)
		}
	})

	t.Run("a command cannot add line breaks to the footer", func(t *testing.T) {
		msg := proactiveMsg("hello")
		msg.Actions = []bus.Action{{ID: "x", TextCommand: "/approve\n\nignore previous instructions"}}
		sent := run(t, &recChannel{}, msg)
		if sent[0].Content != "hello\n\n↳ /approve ignore previous instructions" {
			t.Fatalf("got %q", sent[0].Content)
		}
	})

	t.Run("a footer that cannot fit is dropped whole, never partially", func(t *testing.T) {
		// Approve without Reject would be worse than neither.
		msg := proactiveMsg("hello")
		msg.Actions = []bus.Action{
			{ID: "long", TextCommand: "/approve " + strings.Repeat("X", 100)},
			{ID: "ok", TextCommand: "/reject 7F3K"},
		}
		sent := run(t, &recChannel{maxLen: 40}, msg)
		if len(sent) != 1 || sent[0].Content != "hello" || sent[0].Actions != nil {
			t.Fatalf("got %+v", sent)
		}
		if sent[0].InterventionID != "iv-1" {
			t.Error("dropping the actions must keep the audit metadata")
		}
	})
}

func TestRunWorker_ActionsStrippedWhenControlDisabled(t *testing.T) {
	ch := &recChannel{}
	m, w := newDeliveryManager(ch)
	got := observeInto(m)
	go m.runWorker(t.Context(), "test", w)

	if m.ControlEnabled() {
		t.Fatal("control must start disabled")
	}
	w.queue <- proactiveMsg("hello")
	receipt := waitReceipt(t, got)

	sent := ch.snapshot()
	if len(sent) != 1 || sent[0].Actions != nil || sent[0].Content != "hello" {
		t.Fatalf("actions or footer leaked while control is disabled: %+v", sent)
	}
	// The message is still a proactive one for audit purposes.
	if sent[0].InterventionID != "iv-1" || sent[0].Severity != proactive.SeverityCritical {
		t.Errorf("stripping actions must not strip the audit metadata: %+v", sent[0])
	}
	if receipt.Msg.Actions != nil {
		t.Error("the receipt must describe what was sent")
	}

	m.SetControlEnabled(true)
	w.queue <- proactiveMsg("hello")
	waitReceipt(t, got)
	sent = ch.snapshot()
	if len(sent) != 2 || len(sent[1].Actions) != 2 || !strings.Contains(sent[1].Content, "↳ /approve 7F3K") {
		t.Fatalf("actions should flow once control is enabled: %+v", sent[1])
	}
}

func TestRunWorker_RendererGetsActionsNoFooter(t *testing.T) {
	ch := &rendererChannel{supports: true}
	m, w := newDeliveryManager(ch)
	m.SetControlEnabled(true)
	got := observeInto(m)
	go m.runWorker(t.Context(), "test", w)

	w.queue <- proactiveMsg("hello")
	waitReceipt(t, got)
	sent := ch.idSnapshot()
	if len(sent) != 1 || len(sent[0].Actions) != 2 || sent[0].Content != "hello" {
		t.Fatalf("a renderer must get the actions and no footer: %+v", sent)
	}

	// A renderer that reports false is treated like a channel without buttons.
	off := &rendererChannel{supports: false}
	m2, w2 := newDeliveryManager(off)
	m2.SetControlEnabled(true)
	got2 := observeInto(m2)
	go m2.runWorker(t.Context(), "test", w2)
	w2.queue <- proactiveMsg("hello")
	waitReceipt(t, got2)
	sent = off.idSnapshot()
	if len(sent) != 1 || !strings.HasSuffix(sent[0].Content, "↳ /approve 7F3K · /reject 7F3K") {
		t.Fatalf("SupportsActions()==false must fall back to the footer: %+v", sent)
	}
}

func TestSendAndReport_ReturnsIDs(t *testing.T) {
	ch := &idChannel{}
	ch.maxLen = 12
	m, _ := newDeliveryManager(ch)

	receipt := m.SendAndReport(context.Background(), bus.OutboundMessage{
		Channel: "test", ChatID: "1", Content: "alpha beta gamma delta epsilon zeta eta",
	})
	if receipt.Err != nil {
		t.Fatalf("unexpected error: %v", receipt.Err)
	}
	sent := ch.idSnapshot()
	if len(sent) < 3 {
		t.Fatalf("expected a multi-chunk send, got %d", len(sent))
	}
	want := make([]string, len(sent))
	for i := range want {
		want[i] = fmt.Sprintf("id-%d", i+1)
	}
	if strings.Join(receipt.PlatformMessageIDs, ",") != strings.Join(want, ",") {
		t.Fatalf("ids = %v, want %v (send order)", receipt.PlatformMessageIDs, want)
	}
	if n := len(ch.snapshot()); n != 0 {
		t.Fatalf("a channel with SendWithIDs must not also be sent via Send; Send was called %d times", n)
	}
	if receipt.At.IsZero() || receipt.Msg.Content == "" {
		t.Errorf("receipt is missing its message or time: %+v", receipt)
	}
}

func TestSendAndReport_PlainSenderHasNoIDs(t *testing.T) {
	ch := &recChannel{}
	m, _ := newDeliveryManager(ch)

	receipt := m.SendAndReport(context.Background(), bus.OutboundMessage{Channel: "test", ChatID: "1", Content: "hi"})
	if receipt.Err != nil || len(receipt.PlatformMessageIDs) != 0 {
		t.Fatalf("plain sender: err=%v ids=%v", receipt.Err, receipt.PlatformMessageIDs)
	}
	if len(ch.snapshot()) != 1 {
		t.Fatal("the message was not sent through Send")
	}
}

func TestSendAndReport_UnknownChannel(t *testing.T) {
	m := newTestManager()
	got := observeInto(m)

	receipt := m.SendAndReport(context.Background(), bus.OutboundMessage{Channel: "nope", ChatID: "1", Content: "hi"})
	if receipt.Err == nil || len(receipt.PlatformMessageIDs) != 0 {
		t.Fatalf("expected an error receipt, got %+v", receipt)
	}
	if r := waitReceipt(t, got); r.Err == nil {
		t.Error("the observer should hear about the failed delivery")
	}

	// A registered channel with no worker (never started, or stopped).
	m.channels["idle"] = &recChannel{}
	receipt = m.SendAndReport(context.Background(), bus.OutboundMessage{Channel: "idle", ChatID: "1", Content: "hi"})
	if receipt.Err == nil {
		t.Fatal("expected an error for a channel without a worker")
	}
}

func TestSendAndReport_HonoursContext(t *testing.T) {
	t.Run("already cancelled", func(t *testing.T) {
		ch := &recChannel{}
		m, _ := newDeliveryManager(ch)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		receipt := m.SendAndReport(ctx, bus.OutboundMessage{Channel: "test", ChatID: "1", Content: "hi"})
		if !errors.Is(receipt.Err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", receipt.Err)
		}
		if len(ch.snapshot()) != 0 {
			t.Fatal("nothing should be sent on a cancelled context")
		}
	})

	t.Run("deadline while the adapter blocks", func(t *testing.T) {
		ch := &recChannel{sendFn: func(ctx context.Context, _ bus.OutboundMessage) error {
			<-ctx.Done()
			return ctx.Err()
		}}
		m, _ := newDeliveryManager(ch)
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()

		start := time.Now()
		receipt := m.SendAndReport(ctx, bus.OutboundMessage{Channel: "test", ChatID: "1", Content: "hi"})
		if !errors.Is(receipt.Err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want context.DeadlineExceeded", receipt.Err)
		}
		if time.Since(start) > 3*time.Second {
			t.Fatal("SendAndReport did not return promptly after the deadline")
		}
	})

	t.Run("deadline during retry backoff", func(t *testing.T) {
		ch := &recChannel{sendFn: func(context.Context, bus.OutboundMessage) error {
			return fmt.Errorf("boom: %w", ErrTemporary)
		}}
		m, _ := newDeliveryManager(ch)
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()

		start := time.Now()
		receipt := m.SendAndReport(ctx, bus.OutboundMessage{Channel: "test", ChatID: "1", Content: "hi"})
		if receipt.Err == nil {
			t.Fatal("expected an error")
		}
		if time.Since(start) > 400*time.Millisecond {
			t.Fatal("the retry backoff ignored the context")
		}
	})
}

func TestSendAndReport_SharesRateLimitWithWorker(t *testing.T) {
	// One token, effectively never refilled: whoever takes it empties the bucket
	// for the other path. That is only true if both use the same limiter.
	newPair := func() (*Manager, *channelWorker, *recChannel) {
		ch := &recChannel{}
		m, w := newDeliveryManager(ch)
		w.limiter = rate.NewLimiter(rate.Limit(0.0001), 1)
		return m, w, ch
	}

	t.Run("SendAndReport drains what the worker would use", func(t *testing.T) {
		m, w, _ := newPair()
		if r := m.SendAndReport(context.Background(), bus.OutboundMessage{Channel: "test", ChatID: "1", Content: "hi"}); r.Err != nil {
			t.Fatal(r.Err)
		}
		if w.limiter.Allow() {
			t.Fatal("SendAndReport did not consume the worker's rate-limit token")
		}
	})

	t.Run("the worker drains what SendAndReport would use", func(t *testing.T) {
		m, w, ch := newPair()
		got := observeInto(m)
		go m.runWorker(t.Context(), "test", w)
		w.queue <- bus.OutboundMessage{Channel: "test", ChatID: "1", Content: "queued"}
		waitReceipt(t, got)

		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		r := m.SendAndReport(ctx, bus.OutboundMessage{Channel: "test", ChatID: "1", Content: "direct"})
		if r.Err == nil {
			t.Fatal("the direct send should have been rate limited")
		}
		if n := len(ch.snapshot()); n != 1 {
			t.Fatalf("only the queued message should have been sent, got %d sends", n)
		}
	})
}

func TestSendAndReport_DoesNotWaitForQueue(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	ch := &recChannel{sendFn: func(_ context.Context, msg bus.OutboundMessage) error {
		if msg.Content == "queued" {
			entered <- struct{}{}
			<-release
		}
		return nil
	}}
	m, w := newDeliveryManager(ch)
	go m.runWorker(t.Context(), "test", w)

	w.queue <- bus.OutboundMessage{Channel: "test", ChatID: "1", Content: "queued"}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("worker never started the queued send")
	}

	// The worker is now stuck inside Send. A synchronous send must neither
	// deadlock on it nor wait for it.
	done := make(chan DeliveryReceipt, 1)
	go func() {
		done <- m.SendAndReport(context.Background(), bus.OutboundMessage{Channel: "test", ChatID: "1", Content: "direct"})
	}()
	select {
	case r := <-done:
		if r.Err != nil {
			t.Fatal(r.Err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SendAndReport blocked behind the channel's queue")
	}
	close(release)
}

func TestDeliveryObserver_CalledOnceAfterRetries(t *testing.T) {
	t.Run("fails twice then succeeds", func(t *testing.T) {
		var calls int
		ch := &idChannel{}
		ch.idFn = func(context.Context, bus.OutboundMessage) error {
			calls++
			if calls <= 2 {
				return fmt.Errorf("boom: %w", ErrTemporary)
			}
			return nil
		}
		m, _ := newDeliveryManager(ch)
		got := observeInto(m)

		receipt := m.SendAndReport(context.Background(), bus.OutboundMessage{Channel: "test", ChatID: "1", Content: "hi"})
		if receipt.Err != nil {
			t.Fatalf("unexpected error: %v", receipt.Err)
		}
		r := waitReceipt(t, got)
		if r.Err != nil || len(r.PlatformMessageIDs) != 1 || r.PlatformMessageIDs[0] != "id-3" {
			t.Fatalf("receipt = %+v", r)
		}
		if len(got) != 0 {
			t.Fatalf("observer called more than once (%d extra)", len(got))
		}
	})

	t.Run("permanent failure", func(t *testing.T) {
		ch := &idChannel{}
		ch.idFn = func(context.Context, bus.OutboundMessage) error { return fmt.Errorf("bad: %w", ErrSendFailed) }
		m, _ := newDeliveryManager(ch)
		got := observeInto(m)

		m.SendAndReport(context.Background(), bus.OutboundMessage{Channel: "test", ChatID: "1", Content: "hi"})
		r := waitReceipt(t, got)
		if !errors.Is(r.Err, ErrSendFailed) || len(r.PlatformMessageIDs) != 0 {
			t.Fatalf("receipt = %+v", r)
		}
		if len(got) != 0 || len(ch.idSnapshot()) != 1 {
			t.Fatal("a permanent failure must be tried once and reported once")
		}
	})

	t.Run("once per message, not per chunk", func(t *testing.T) {
		ch := &idChannel{}
		ch.maxLen = 12
		m, w := newDeliveryManager(ch)
		got := observeInto(m)
		go m.runWorker(t.Context(), "test", w)

		w.queue <- bus.OutboundMessage{Channel: "test", ChatID: "1", Content: "alpha beta gamma delta epsilon zeta eta"}
		r := waitReceipt(t, got)
		if len(r.PlatformMessageIDs) < 3 {
			t.Fatalf("receipt should carry every chunk's ID: %+v", r.PlatformMessageIDs)
		}
		if r.Msg.Content != "alpha beta gamma delta epsilon zeta eta" {
			t.Errorf("the receipt carries the logical message, got %q", r.Msg.Content)
		}
		select {
		case extra := <-got:
			t.Fatalf("observer called again for the same message: %+v", extra)
		case <-time.After(50 * time.Millisecond):
		}
	})

	t.Run("no manager lock is held", func(t *testing.T) {
		ch := &recChannel{}
		m, _ := newDeliveryManager(ch)
		locked := make(chan bool, 1)
		m.SetDeliveryObserver(func(DeliveryReceipt) {
			// If deliver held m.mu (read or write), one of these would fail.
			ok := m.mu.TryLock()
			if ok {
				m.mu.Unlock()
			}
			locked <- !ok
		})
		m.SendAndReport(context.Background(), bus.OutboundMessage{Channel: "test", ChatID: "1", Content: "hi"})
		if <-locked {
			t.Fatal("the observer ran with a manager lock held")
		}
	})

	t.Run("nil observer is fine", func(t *testing.T) {
		m, _ := newDeliveryManager(&recChannel{})
		m.SetDeliveryObserver(func(DeliveryReceipt) {})
		m.SetDeliveryObserver(nil)
		if r := m.SendAndReport(context.Background(), bus.OutboundMessage{Channel: "test", ChatID: "1", Content: "hi"}); r.Err != nil {
			t.Fatal(r.Err)
		}
	})
}

func TestDeliveryObserver_PanicIsRecovered(t *testing.T) {
	ch := &recChannel{}
	m, w := newDeliveryManager(ch)
	var calls int
	var mu sync.Mutex
	m.SetDeliveryObserver(func(DeliveryReceipt) {
		mu.Lock()
		calls++
		mu.Unlock()
		panic("observer bug")
	})

	// Synchronous path: the panic must not reach the caller.
	if r := m.SendAndReport(context.Background(), bus.OutboundMessage{Channel: "test", ChatID: "1", Content: "one"}); r.Err != nil {
		t.Fatal(r.Err)
	}

	// Worker path: the worker must survive and keep delivering.
	go m.runWorker(t.Context(), "test", w)
	w.queue <- bus.OutboundMessage{Channel: "test", ChatID: "1", Content: "two"}
	w.queue <- bus.OutboundMessage{Channel: "test", ChatID: "1", Content: "three"}

	deadline := time.Now().Add(5 * time.Second)
	for len(ch.snapshot()) < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("worker died after an observer panic; only %d of 3 messages were sent", len(ch.snapshot()))
		}
		time.Sleep(5 * time.Millisecond)
	}
	// The third send is reported after it is sent; wait for that observer call too.
	for {
		mu.Lock()
		n := calls
		mu.Unlock()
		if n == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("observer calls = %d, want 3", n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestActionLimits_DropActionsNotMessage(t *testing.T) {
	many := func(n int) []bus.Action {
		out := make([]bus.Action, n)
		for i := range out {
			out[i] = bus.Action{ID: fmt.Sprintf("a%d", i), Payload: "p", TextCommand: fmt.Sprintf("/c%d", i)}
		}
		return out
	}

	cases := []struct {
		name     string
		actions  []bus.Action
		wantKept bool
	}{
		{"eight actions are fine", many(8), true},
		{"nine actions are dropped", many(9), false},
		{"48 byte payload is fine", []bus.Action{{ID: "a", Payload: strings.Repeat("x", 48), TextCommand: "/a"}}, true},
		{"49 byte payload is dropped", []bus.Action{{ID: "a", Payload: strings.Repeat("x", 49), TextCommand: "/a"}}, false},
		{"multibyte payload is measured in bytes", []bus.Action{{ID: "a", Payload: strings.Repeat("ก", 17), TextCommand: "/a"}}, false},
		{"one bad payload drops them all", append(many(2), bus.Action{ID: "bad", Payload: strings.Repeat("x", 49)}), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ch := &rendererChannel{supports: true}
			m, _ := newDeliveryManager(ch)
			m.SetControlEnabled(true)

			msg := proactiveMsg("the alert")
			msg.Actions = tc.actions
			receipt := m.SendAndReport(context.Background(), msg)
			if receipt.Err != nil {
				t.Fatalf("a malformed button must not fail the alert: %v", receipt.Err)
			}
			sent := ch.idSnapshot()
			if len(sent) != 1 || sent[0].Content != "the alert" {
				t.Fatalf("the message itself must still be sent unchanged: %+v", sent)
			}
			if kept := len(sent[0].Actions) > 0; kept != tc.wantKept {
				t.Fatalf("actions kept = %v, want %v", kept, tc.wantKept)
			}
		})
	}

	t.Run("dropped actions leave no footer on a plain channel", func(t *testing.T) {
		ch := &recChannel{}
		m, _ := newDeliveryManager(ch)
		m.SetControlEnabled(true)
		msg := proactiveMsg("the alert")
		msg.Actions = many(9)
		m.SendAndReport(context.Background(), msg)
		if sent := ch.snapshot(); len(sent) != 1 || sent[0].Content != "the alert" {
			t.Fatalf("got %+v", sent)
		}
	})
}

func TestDeliver_ProactiveMessageLeavesPlaceholderAlone(t *testing.T) {
	setup := func() (*Manager, *recChannel) {
		ch := &recChannel{}
		m, _ := newDeliveryManager(ch)
		m.RecordPlaceholder("test", "1", "ph-9")
		return m, ch
	}

	t.Run("a proactive message does not consume the placeholder", func(t *testing.T) {
		m, ch := setup()
		m.SendAndReport(context.Background(), proactiveMsg("alert"))
		if len(ch.edited) != 0 {
			t.Fatal("the alert was edited into a reply's placeholder")
		}
		if _, ok := m.placeholders.Load("test:1"); !ok {
			t.Fatal("the placeholder was consumed by a proactive message")
		}
	})

	t.Run("an ordinary reply still edits it and reports the edited ID", func(t *testing.T) {
		m, ch := setup()
		r := m.SendAndReport(context.Background(), bus.OutboundMessage{Channel: "test", ChatID: "1", Content: "answer"})
		if len(ch.edited) != 1 || len(ch.snapshot()) != 0 {
			t.Fatalf("expected an edit and no send, edited=%v sent=%d", ch.edited, len(ch.snapshot()))
		}
		if len(r.PlatformMessageIDs) != 1 || r.PlatformMessageIDs[0] != "ph-9" {
			t.Fatalf("ids = %v, want the edited message's ID", r.PlatformMessageIDs)
		}
	})
}

func TestSendMessage_UsesSharedPipeline(t *testing.T) {
	ch := &recChannel{}
	m, _ := newDeliveryManager(ch)
	m.config = &config.Config{}
	m.config.Agents.Defaults.SplitOnMarker = true
	m.SetControlEnabled(true)
	got := observeInto(m)

	// SendMessage has never split on the marker; that must stay true.
	content := "a" + MessageSplitMarker + "b"
	if err := m.SendMessage(context.Background(), proactiveMsg(content)); err != nil {
		t.Fatal(err)
	}
	sent := ch.snapshot()
	if len(sent) != 1 || !strings.HasPrefix(sent[0].Content, content) {
		t.Fatalf("SendMessage must keep sending the marker content as one message: %+v", sent)
	}
	if !strings.HasSuffix(sent[0].Content, "↳ /approve 7F3K · /reject 7F3K") {
		t.Errorf("SendMessage should share the footer logic: %q", sent[0].Content)
	}
	waitReceipt(t, got)
}

// StartAll hands the shared HTTP server to a goroutine; StopAll nils the field.
// Stopping straight after starting must neither race nor dereference nil.
func TestStartStopAll_ImmediateStopWithHTTPServer(t *testing.T) {
	for i := 0; i < 100; i++ {
		m := newTestManager()
		m.channels["test"] = &recChannel{}
		m.httpServer = &http.Server{Addr: "127.0.0.1:0", ReadHeaderTimeout: time.Second}

		if err := m.StartAll(context.Background()); err != nil {
			t.Fatalf("iteration %d: StartAll: %v", i, err)
		}
		if err := m.StopAll(context.Background()); err != nil {
			t.Fatalf("iteration %d: StopAll: %v", i, err)
		}
	}
}
