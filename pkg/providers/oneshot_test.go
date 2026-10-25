package providers

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

// recordingProvider captures the last Chat request and replies with resp/err.
type recordingProvider struct {
	calls    int
	messages []Message
	tools    []ToolDefinition
	model    string
	options  map[string]any
	resp     *LLMResponse
	err      error
}

func (r *recordingProvider) Chat(
	_ context.Context,
	messages []Message,
	tools []ToolDefinition,
	model string,
	options map[string]any,
) (*LLMResponse, error) {
	r.calls++
	r.messages, r.tools, r.model, r.options = messages, tools, model, options
	return r.resp, r.err
}

func (r *recordingProvider) GetDefaultModel() string { return "default" }

func TestComplete_MaxTokensIsInt(t *testing.T) {
	p := &recordingProvider{resp: &LLMResponse{Content: "ok"}}
	if _, _, err := Complete(context.Background(), p, "m", "hi", CompleteOptions{MaxTokens: 321}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	// The Anthropic provider asserts .(int); any other numeric type is ignored.
	got, ok := p.options["max_tokens"].(int)
	if !ok {
		t.Fatalf("max_tokens = %#v (%T), want int", p.options["max_tokens"], p.options["max_tokens"])
	}
	if got != 321 {
		t.Errorf("max_tokens = %d, want 321", got)
	}
}

func TestComplete_MaxTokensMustBePositive(t *testing.T) {
	for _, n := range []int{0, -1} {
		p := &recordingProvider{resp: &LLMResponse{Content: "ok"}}
		if _, _, err := Complete(context.Background(), p, "m", "hi", CompleteOptions{MaxTokens: n}); err == nil {
			t.Errorf("MaxTokens=%d: want error", n)
		}
		if p.calls != 0 {
			t.Errorf("MaxTokens=%d: provider was called", n)
		}
	}
}

func TestComplete_NilProvider(t *testing.T) {
	if _, _, err := Complete(context.Background(), nil, "m", "hi", CompleteOptions{MaxTokens: 1}); err == nil {
		t.Fatal("want error for nil provider")
	}
}

func TestComplete_SystemMessage(t *testing.T) {
	t.Run("set", func(t *testing.T) {
		p := &recordingProvider{resp: &LLMResponse{Content: "ok"}}
		_, _, err := Complete(context.Background(), p, "m", "the prompt", CompleteOptions{MaxTokens: 10, System: "be terse"})
		if err != nil {
			t.Fatalf("Complete: %v", err)
		}
		want := []Message{
			{Role: "system", Content: "be terse"},
			{Role: "user", Content: "the prompt"},
		}
		if !reflect.DeepEqual(p.messages, want) {
			t.Errorf("messages = %#v, want %#v", p.messages, want)
		}
	})
	t.Run("empty", func(t *testing.T) {
		p := &recordingProvider{resp: &LLMResponse{Content: "ok"}}
		if _, _, err := Complete(context.Background(), p, "m", "the prompt", CompleteOptions{MaxTokens: 10}); err != nil {
			t.Fatalf("Complete: %v", err)
		}
		want := []Message{{Role: "user", Content: "the prompt"}}
		if !reflect.DeepEqual(p.messages, want) {
			t.Errorf("messages = %#v, want %#v", p.messages, want)
		}
	})
}

func TestComplete_NoTools(t *testing.T) {
	p := &recordingProvider{resp: &LLMResponse{Content: "ok"}}
	if _, _, err := Complete(context.Background(), p, "m", "hi", CompleteOptions{MaxTokens: 10}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(p.tools) != 0 {
		t.Errorf("tools = %#v, want none", p.tools)
	}
}

func TestComplete_RequestFields(t *testing.T) {
	p := &recordingProvider{resp: &LLMResponse{Content: "ok"}}
	_, _, err := Complete(context.Background(), p, "model-x", "hi", CompleteOptions{
		MaxTokens: 50, Temperature: 0.25, CacheKey: "k",
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if p.model != "model-x" {
		t.Errorf("model = %q", p.model)
	}
	want := map[string]any{"max_tokens": 50, "temperature": 0.25, "prompt_cache_key": "k"}
	if !reflect.DeepEqual(p.options, want) {
		t.Errorf("options = %#v, want %#v", p.options, want)
	}

	// Temperature is always sent (even zero); an empty CacheKey is omitted.
	if _, _, err := Complete(context.Background(), p, "m", "hi", CompleteOptions{MaxTokens: 50}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	want = map[string]any{"max_tokens": 50, "temperature": float64(0)}
	if !reflect.DeepEqual(p.options, want) {
		t.Errorf("options = %#v, want %#v", p.options, want)
	}
}

func TestComplete_ReturnsTextAndUsage(t *testing.T) {
	p := &recordingProvider{resp: &LLMResponse{
		Content: "answer",
		Usage:   &UsageInfo{PromptTokens: 3, CompletionTokens: 4, TotalTokens: 7},
	}}
	text, usage, err := Complete(context.Background(), p, "m", "hi", CompleteOptions{MaxTokens: 10})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if text != "answer" {
		t.Errorf("text = %q", text)
	}
	if want := (UsageInfo{PromptTokens: 3, CompletionTokens: 4, TotalTokens: 7}); usage != want {
		t.Errorf("usage = %+v, want %+v", usage, want)
	}
}

func TestComplete_NoUsageIsZero(t *testing.T) {
	p := &recordingProvider{resp: &LLMResponse{Content: "answer"}}
	_, usage, err := Complete(context.Background(), p, "m", "hi", CompleteOptions{MaxTokens: 10})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if usage != (UsageInfo{}) {
		t.Errorf("usage = %+v, want zero", usage)
	}
}

func TestComplete_ProviderErrorReturnedAsIs(t *testing.T) {
	sentinel := &FailoverError{Reason: FailoverRateLimit, Provider: "p", Model: "m", Status: 429}
	p := &recordingProvider{err: sentinel}
	text, usage, err := Complete(context.Background(), p, "m", "hi", CompleteOptions{MaxTokens: 10})
	if err != error(sentinel) {
		t.Fatalf("err = %v, want the provider's own error value", err)
	}
	if text != "" || usage != (UsageInfo{}) {
		t.Errorf("text=%q usage=%+v, want zero values on error", text, usage)
	}
}

func TestComplete_NilResponse(t *testing.T) {
	p := &recordingProvider{}
	if _, _, err := Complete(context.Background(), p, "m", "hi", CompleteOptions{MaxTokens: 10}); err == nil {
		t.Fatal("want error for a nil response")
	}
}

// ctxProvider blocks until the context is done, then returns its error.
type ctxProvider struct{ recordingProvider }

func (c *ctxProvider) Chat(
	ctx context.Context, _ []Message, _ []ToolDefinition, _ string, _ map[string]any,
) (*LLMResponse, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestComplete_DeadlineHonoured(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, _, err := Complete(ctx, &ctxProvider{}, "m", "hi", CompleteOptions{MaxTokens: 10})
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want context.DeadlineExceeded", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Complete did not return after the context deadline")
	}
}
