package sim

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/providers/protocoltypes"
)

// engineAt is an engine over a budget store, as a restarted instance builds it.
func engineAt(t *testing.T, store string, usd float64, tok int) (*Engine, *LiveOptions, *fakeInner) {
	t.Helper()
	e, err := NewEngine(EngineOptions{Mode: ModeLive, BudgetUSD: usd, BudgetTok: tok, BudgetStore: store,
		TurnInfo: func(context.Context) CallInfo { return CallInfo{Turn: "user", TurnID: "t1"} }})
	if err != nil {
		t.Fatal(err)
	}
	in := &fakeInner{}
	return e, &LiveOptions{Inner: in, InnerModel: "m", Price: testPrice, Key: sentinelKey}, in
}

// A restart does not give the budget back: spend, the sticky exhausted flag and a lowered
// limit all survive, and the call that would pass the limit is refused by the new process
// without reaching the provider.
func TestBudgetSurvivesRestart(t *testing.T) {
	store := filepath.Join(t.TempDir(), "lab", "budget.json")
	e, lo, _ := engineAt(t, store, 0.02, 0)
	if _, err := ask(e, lo, "one"); err != nil {
		t.Fatal(err)
	}
	spent := e.Budget().SpentUSD
	if spent <= 0 {
		t.Fatalf("nothing was charged: %+v", e.Budget())
	}
	if _, err := e.LowerBudget(0.012, 0); err != nil {
		t.Fatal(err)
	}

	// The restarted process starts from the same environment limit (0.02) and from what
	// was saved: the spend and the lowered 0.012.
	e2, lo2, in2 := engineAt(t, store, 0.02, 0)
	if st := e2.Budget(); st.SpentUSD != spent || st.LimitUSD != 0.012 || st.Calls != 1 {
		t.Fatalf("restarted budget = %+v, want spent %v, limit 0.012, 1 call", st, spent)
	}
	if _, err := ask(e2, lo2, "two"); err != nil { // 0.006 + the dearest call so far (0.006) = 0.012: the limit exactly
		t.Fatalf("second call = %v", err)
	}
	e3, lo3, in3 := engineAt(t, store, 0.5, 0) // a bigger environment limit does not undo the lowered one
	if st := e3.Budget(); st.LimitUSD != 0.012 || !st.Exhausted {
		t.Fatalf("after the second restart: %+v, want limit 0.012 and exhausted", st)
	}
	if _, err := ask(e3, lo3, "three"); !errors.Is(err, ErrBudget) {
		t.Fatalf("the call over the limit = %v, want %v", err, ErrBudget)
	}
	if in2.count() != 1 || in3.count() != 0 {
		t.Errorf("provider calls: restarted %d, last %d, want 1 and 0 (the refused call must not reach it)", in2.count(), in3.count())
	}
	if info, err := os.Stat(store); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("budget store: %v, %v", info, err)
	}
}

// A process killed during a call has been charged for the whole reservation: it was saved
// before the provider was asked.
func TestBudgetCharged_BeforeTheCall(t *testing.T) {
	store := filepath.Join(t.TempDir(), "budget.json")
	e, lo, in := engineAt(t, store, 1, 0)
	in.resp = func(int) (*protocoltypes.LLMResponse, error) {
		// What a restarted process would read, while the call is still in flight.
		e2, _, _ := engineAt(t, store, 1, 0)
		if st := e2.Budget(); st.SpentUSD <= 0 || st.Calls != 1 {
			t.Errorf("during the call the saved budget is %+v, want the reservation charged", st)
		}
		return &protocoltypes.LLMResponse{Content: "ok", FinishReason: "stop", Usage: &protocoltypes.UsageInfo{PromptTokens: 100, CompletionTokens: 20}}, nil
	}
	if _, err := ask(e, lo, "go"); err != nil {
		t.Fatal(err)
	}
}

// A store that cannot be read back or written refuses the live call (fail closed), and
// the provider is not asked.
func TestBudgetStoreFailsClosed(t *testing.T) {
	dir := t.TempDir()
	corrupt := filepath.Join(dir, "corrupt.json")
	if err := os.WriteFile(corrupt, []byte(`{"spent_usd": `), 0o600); err != nil {
		t.Fatal(err)
	}
	e, lo, in := engineAt(t, corrupt, 1, 0)
	if _, err := ask(e, lo, "go"); !errors.Is(err, ErrBudgetStore) || !errors.Is(err, ErrBudget) {
		t.Errorf("a corrupt store: %v, want %v", err, ErrBudgetStore)
	}
	if _, err := e.LowerBudget(0.5, 0); !errors.Is(err, ErrBudgetStore) {
		t.Errorf("lowering over a corrupt store: %v", err)
	}
	// A path that cannot be written: its parent is a file.
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	e2, lo2, in2 := engineAt(t, filepath.Join(file, "budget.json"), 1, 0)
	if _, err := ask(e2, lo2, "go"); !errors.Is(err, ErrBudgetStore) {
		t.Errorf("an unwritable store: %v, want %v", err, ErrBudgetStore)
	}
	if in.count()+in2.count() != 0 {
		t.Errorf("the provider was called %d times with a broken store, want 0", in.count()+in2.count())
	}
}
