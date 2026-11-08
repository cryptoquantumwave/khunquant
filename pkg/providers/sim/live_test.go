package sim

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/providers/protocoltypes"
)

// fakeInner stands in for the real model: no network at all.
type fakeInner struct {
	mu    sync.Mutex
	calls int
	resp  func(n int) (*protocoltypes.LLMResponse, error)
}

func (f *fakeInner) Chat(_ context.Context, _ []protocoltypes.Message, _ []protocoltypes.ToolDefinition, _ string, _ map[string]any) (*protocoltypes.LLMResponse, error) {
	f.mu.Lock()
	f.calls++
	n := f.calls
	f.mu.Unlock()
	if f.resp != nil {
		return f.resp(n)
	}
	return &protocoltypes.LLMResponse{Content: "live", FinishReason: "stop", Usage: &protocoltypes.UsageInfo{PromptTokens: 1000, CompletionTokens: 200}}, nil
}

func (f *fakeInner) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// $0.003 for 1000 input tokens and $0.003 for 200 output at 3/15: 0.006 a call.
var testPrice = Price{In: 3, Out: 15}

const sentinelKey = "sk-test-SENTINEL-0123456789-not-a-real-key"

func newLiveEngine(t *testing.T, mode string, usd float64, tok int, rec string) (*Engine, *memJournal, *LiveOptions, *fakeInner) {
	t.Helper()
	j := &memJournal{}
	e, err := NewEngine(EngineOptions{Journal: j, Mode: mode, BudgetUSD: usd, BudgetTok: tok, RecordPath: rec,
		TurnInfo: func(context.Context) CallInfo { return CallInfo{Turn: "user", TurnID: "t1"} }})
	if err != nil {
		t.Fatal(err)
	}
	in := &fakeInner{}
	return e, j, &LiveOptions{Inner: in, InnerModel: "m", Price: testPrice, Key: sentinelKey}, in
}

func ask(e *Engine, lo *LiveOptions, text string) (*protocoltypes.LLMResponse, error) {
	return e.ChatWith(context.Background(), lo, user(text), nil, "m", map[string]any{"prompt_cache_key": "main", "max_tokens": 100})
}

func kinds(j *memJournal, kind string) []map[string]any {
	var out []map[string]any
	for _, en := range j.entries() {
		if en.Kind == kind {
			var m map[string]any
			_ = json.Unmarshal(en.Payload, &m)
			out = append(out, m)
		}
	}
	return out
}

func TestLiveBudgetExhaustion(t *testing.T) {
	// 0.006 a call; a second call reserves what the dearest so far cost, so 0.012 is reached exactly.
	e, j, lo, in := newLiveEngine(t, ModeLive, 0.012, 0, "")
	for i := 1; i <= 2; i++ {
		if r, err := ask(e, lo, "go"); err != nil || r.Content != "live" {
			t.Fatalf("call %d = %v, %v", i, r, err)
		}
	}
	if st := e.Budget(); st.Calls != 2 || st.SpentUSD < 0.0119 || st.SpentUSD > 0.0121 || !st.Exhausted {
		t.Fatalf("after two calls: %+v", st)
	}
	if _, err := ask(e, lo, "go"); !errors.Is(err, ErrBudget) {
		t.Fatalf("a third call = %v, want %v", err, ErrBudget)
	}
	if in.count() != 2 {
		t.Errorf("the inner model was called %d times, want 2 (a refused call must not reach it)", in.count())
	}
	var events []string
	for _, p := range kinds(j, "llm.budget") {
		events = append(events, p["event"].(string))
	}
	if strings.Join(events, ",") != "exhausted,refused" {
		t.Errorf("llm.budget events = %v, want exhausted then refused", events)
	}
	// Sticky: still refused.
	if _, err := ask(e, lo, "small"); !errors.Is(err, ErrBudget) {
		t.Errorf("after exhaustion: %v", err)
	}
}

func TestLiveRefusesTheCallThatWouldExceed(t *testing.T) {
	// One call costs 0.006, so 0.007 is not spent after it, but the next call's upper bound
	// would pass the limit: refused before it is made.
	e, j, lo, in := newLiveEngine(t, ModeLive, 0.007, 0, "")
	if _, err := ask(e, lo, "go"); err != nil {
		t.Fatal(err)
	}
	if st := e.Budget(); st.Exhausted {
		t.Fatalf("exhausted too early: %+v", st)
	}
	if _, err := ask(e, lo, "go"); !errors.Is(err, ErrBudget) {
		t.Fatalf("second call = %v, want the budget refusal", err)
	}
	if in.count() != 1 {
		t.Errorf("inner called %d times", in.count())
	}
	if len(kinds(j, "llm.budget")) != 1 || kinds(j, "llm.budget")[0]["event"] != "refused" {
		t.Errorf("llm.budget = %v", kinds(j, "llm.budget"))
	}
	// The refused call is in the journal as a failed llm.call too.
	if calls := kinds(j, "llm.call"); len(calls) != 2 || calls[1]["error"] == nil {
		t.Errorf("llm.call entries = %v", calls)
	}
}

func TestLiveTokenBudget(t *testing.T) {
	e, _, lo, _ := newLiveEngine(t, ModeLive, 0, 1500, "")
	if _, err := ask(e, lo, "go"); err != nil { // 1200 tokens
		t.Fatal(err)
	}
	if _, err := ask(e, lo, "go"); !errors.Is(err, ErrBudget) {
		t.Errorf("a second 1200-token call against 1500 = %v", err)
	}
}

func TestBudgetOnlyLowers(t *testing.T) {
	e, j, _, _ := newLiveEngine(t, ModeLive, 2, 200000, "")
	for name, c := range map[string]struct {
		usd float64
		tok int
	}{"the same usd": {2, 0}, "a higher usd": {5, 0}, "a higher tok": {0, 300000}, "the same tok": {0, 200000}, "a negative": {-1, 0}, "nothing": {0, 0}} {
		if _, err := e.LowerBudget(c.usd, c.tok); err == nil {
			t.Errorf("%s: must be refused", name)
		}
	}
	if st := e.Budget(); st.LimitUSD != 2 || st.LimitTok != 200000 {
		t.Fatalf("a refused raise changed the budget: %+v", st)
	}
	st, err := e.LowerBudget(1, 100000)
	if err != nil || st.LimitUSD != 1 || st.LimitTok != 100000 {
		t.Fatalf("lowering = %+v, %v", st, err)
	}
	if ev := kinds(j, "llm.budget"); len(ev) != 1 || ev[0]["event"] != "lowered" {
		t.Errorf("llm.budget = %v", ev)
	}
	// Only a live or record instance has a budget.
	sc, _ := newTestEngine(t)
	if _, err := sc.LowerBudget(1, 0); err == nil {
		t.Error("a scripted instance has no budget to lower")
	}
}

func TestLiveUsageMissingUsesTheUpperBound(t *testing.T) {
	e, j, lo, in := newLiveEngine(t, ModeLive, 100, 0, "")
	in.resp = func(int) (*protocoltypes.LLMResponse, error) {
		return &protocoltypes.LLMResponse{Content: strings.Repeat("x", 400), FinishReason: "stop"}, nil
	}
	if _, err := ask(e, lo, "go"); err != nil {
		t.Fatal(err)
	}
	st := e.Budget()
	if st.SpentTok <= 400 || st.SpentUSD <= 0 {
		t.Errorf("estimated spend = %+v, want at least the 400 bytes of output plus the input", st)
	}
	if c := kinds(j, "llm.call"); len(c) != 1 || c[0]["usage_estimated"] != true || c[0]["mode"] != "live" {
		t.Errorf("llm.call = %v", c)
	}
}

func TestLiveErrorChargesTheWholeReservationAndRedactsKey(t *testing.T) {
	e, j, lo, in := newLiveEngine(t, ModeLive, 100, 0, "")
	in.resp = func(int) (*protocoltypes.LLMResponse, error) {
		return nil, errors.New("bad request (400): invalid key " + sentinelKey)
	}
	_, err := ask(e, lo, "go")
	if err == nil || strings.Contains(err.Error(), sentinelKey) || !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("error = %v", err)
	}
	// The call was admitted at input bytes plus max_tokens (100) at the price; a failed call
	// may still have produced billed output, so all of that is charged, not the input alone.
	wantTok, wantUSD := bound(user("go"), nil, map[string]any{"max_tokens": 100}, testPrice)
	st := e.Budget()
	if st.Calls != 1 || st.SpentTok != wantTok || st.SpentUSD != wantUSD {
		t.Errorf("a failed call is charged its reservation (%d tok, %g usd): %+v", wantTok, wantUSD, st)
	}
	if inOnly := len(mustJSON(user("go"))); st.SpentTok <= inOnly {
		t.Errorf("charged %d tokens, no more than the input's %d", st.SpentTok, inOnly)
	}
	for _, en := range j.entries() {
		if strings.Contains(string(en.Payload), sentinelKey) {
			t.Errorf("the key reached the journal: %s", en.Payload)
		}
	}
}

func TestLiveConcurrentCallsNeverOverspend(t *testing.T) {
	e, _, lo, _ := newLiveEngine(t, ModeLive, 0.05, 0, "")
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = ask(e, lo, "go")
		}()
	}
	wg.Wait()
	if st := e.Budget(); st.SpentUSD > 0.05 || st.Calls < 1 {
		t.Errorf("12 racing calls against 0.05: %+v", st)
	}
}

func TestRecordThenReplayRoundTrip(t *testing.T) {
	rec := filepath.Join(t.TempDir(), "lab", "cassettes", "record.jsonl")
	e, _, lo, in := newLiveEngine(t, ModeRecord, 100, 0, rec)
	in.resp = func(n int) (*protocoltypes.LLMResponse, error) {
		if n == 1 {
			return &protocoltypes.LLMResponse{FinishReason: "tool_calls", Usage: &protocoltypes.UsageInfo{PromptTokens: 10, CompletionTokens: 5},
				ToolCalls: []protocoltypes.ToolCall{{ID: "toolu_REAL", Name: "read_file", Arguments: map[string]any{"path": "a.md"}}}}, nil
		}
		return &protocoltypes.LLMResponse{Content: "done", FinishReason: "stop", Usage: &protocoltypes.UsageInfo{PromptTokens: 12, CompletionTokens: 3}}, nil
	}
	first, err := ask(e, lo, "one")
	if err != nil {
		t.Fatal(err)
	}
	second, err := ask(e, lo, "two")
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(rec); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("cassette file: %v, %v", fi, err)
	}

	// A replay instance with no inner model answers the same, from the file alone.
	f, err := os.Open(rec)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	entries, err := ParseEntries(f)
	if err != nil || len(entries) != 2 {
		t.Fatalf("entries = %d, %v", len(entries), err)
	}
	r, _ := newReplayEngine(t, entries, "")
	r1, err := r.ChatWith(context.Background(), nil, user("one"), nil, "m", map[string]any{"prompt_cache_key": "main", "max_tokens": 100})
	if err != nil {
		t.Fatal(err)
	}
	r2, err := r.ChatWith(context.Background(), nil, user("two"), nil, "m", map[string]any{"prompt_cache_key": "main", "max_tokens": 100})
	if err != nil {
		t.Fatal(err)
	}
	if r1.Content != first.Content || r2.Content != second.Content || r1.FinishReason != "tool_calls" ||
		len(r1.ToolCalls) != 1 || r1.ToolCalls[0].Name != "read_file" || r1.ToolCalls[0].Arguments["path"] != "a.md" ||
		r1.Usage == nil || r1.Usage.PromptTokens != 10 || r2.Usage.CompletionTokens != 3 {
		t.Errorf("replay differs from the recording:\n%+v / %+v\n%+v / %+v", first, r1, second, r2)
	}
	if !strings.HasPrefix(r1.ToolCalls[0].ID, toolIDPrefix) {
		t.Errorf("replayed tool-call id %q was not re-minted", r1.ToolCalls[0].ID)
	}
	if in.count() != 2 {
		t.Errorf("inner called %d times", in.count())
	}
}

func TestRecordFailureToWriteIsAnError(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	e, _, lo, _ := newLiveEngine(t, ModeRecord, 100, 0, filepath.Join(blocker, "x", "record.jsonl"))
	if _, err := ask(e, lo, "go"); err == nil || !strings.Contains(err.Error(), "recording the cassette") {
		t.Errorf("a cassette that cannot be written = %v", err)
	}
}

func TestLiveWithoutInnerFails(t *testing.T) {
	e, _, _, _ := newLiveEngine(t, ModeLive, 1, 0, "")
	if _, err := e.ChatWith(context.Background(), nil, user("x"), nil, "m", agentOpts); err == nil {
		t.Error("a live call with no inner model must fail")
	}
}

func TestLiveEnvFrom(t *testing.T) {
	env := map[string]string{"KQ_SANDBOX_LLM_KEY": sentinelKey, "KQ_SANDBOX_LLM_MODELS": "a, b", "KQ_SANDBOX_LLM_BUDGET": "2USD",
		"KQ_SANDBOX_LLM_PRICES": "b=1:2"}
	le, err := LiveEnvFrom(func(k string) string { return env[k] })
	if err != nil || le.BudgetUSD != 2 || len(le.Models) != 2 || le.Prices["b"] != (Price{1, 2}) || le.Prices["claude-sonnet-4.6"].Out != 15 {
		t.Fatalf("env = %+v, %v", le, err)
	}
	for _, bad := range []map[string]string{{"KQ_SANDBOX_LLM_BUDGET": "2"}, {"KQ_SANDBOX_LLM_PRICES": "x"}} {
		_, err := LiveEnvFrom(func(k string) string { return bad[k] })
		if err == nil || strings.Contains(err.Error(), sentinelKey) {
			t.Errorf("%v: err = %v", bad, err)
		}
	}
}
