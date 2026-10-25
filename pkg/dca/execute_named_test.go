package dca

import (
	"context"
	"strings"
	"testing"
	"time"
)

// askOnce runs one due slot against a mandate shaped by mut and setup and asserts it
// placed nothing and put exactly one question whose reason contains want.
func askOnce(t *testing.T, mut func(*Mandate), setup func(*xworld, *Store), want string) {
	t.Helper()
	w := newXWorld(t, true, monday)
	p := w.newProc()
	m := w.mandate(p.store, mut)
	if setup != nil {
		setup(w, p.store)
	}
	p.run(m, monday)
	if w.ex.orderCount() != 0 {
		t.Fatalf("%d orders placed outside the mandate", w.ex.orderCount())
	}
	if w.asker.count() != 1 || !strings.Contains(w.asker.asked[0], want) {
		t.Fatalf("questions = %v, want exactly one containing %q", w.asker.asked, want)
	}
}

func TestDCAExecute_PerExecutionCapAsks(t *testing.T) {
	// 2000 THB is $55.20 against a $50 cap, though 2000 is below the 3000 THB the card showed.
	askOnce(t, func(m *Mandate) { m.MaxPerRunQuote = 3000; m.MaxPerExecutionUSD = 50 }, nil, "over the per-run limit")
}

func TestDCAExecute_PeriodCapAsks(t *testing.T) {
	askOnce(t, func(m *Mandate) { m.MaxPerPeriodUSD = 80 }, func(w *xworld, st *Store) { w.spend(st, 40) }, "limit would be exceeded")
}

func TestDCAExecute_ExpiredMandateAsks(t *testing.T) {
	askOnce(t, func(m *Mandate) { m.ExpiresAt = monday.Add(-time.Hour) }, nil, "the mandate expired on")
}

// No model is involved: the workflow completes a run with nothing that could reach a
// provider wired into it, and the package does not import one. What this does and does
// not prove: Deps has no provider field and no file of the execution path imports an
// agent, tool or LLM-provider package, so there is no call to count; the gateway-level
// assembly (a counting provider on the loop) is not exercised here.
func TestDCAExecute_NoLLMInvolved(t *testing.T) {
	w := newXWorld(t, true, monday)
	p := w.newProc()
	m := w.mandate(p.store, nil)
	p.run(m, monday)
	if w.ex.orderCount() != 1 {
		t.Fatalf("%d orders", w.ex.orderCount())
	}
	for _, f := range []string{"workflow.go", "messages.go", "mandate.go"} {
		src := readFile(t, f)
		for _, banned := range []string{`khunquant/pkg/agent"`, `khunquant/pkg/tools"`, `khunquant/pkg/providers"`,
			`khunquant/pkg/providers/anthropic"`, `khunquant/pkg/providers/openai"`, `khunquant/pkg/mcp"`, ".Chat("} {
			if strings.Contains(src, banned) {
				t.Errorf("%s mentions %s", f, banned)
			}
		}
	}
}

// RevokeAll (the /stop path) revokes every active mandate, once; a revoked plan has no
// active mandate and is awaiting one.
func TestRevokeAll(t *testing.T) {
	w := newXWorld(t, true, monday)
	st := w.openStore()
	m := w.mandate(st, nil)
	n, err := st.RevokeAll(context.Background(), "stop")
	if err != nil || n != 1 {
		t.Fatalf("RevokeAll = %d, %v", n, err)
	}
	if n, _ := st.RevokeAll(context.Background(), "stop"); n != 0 {
		t.Errorf("a second RevokeAll revoked %d", n)
	}
	if _, ok, _ := st.ActiveMandateForPlan(context.Background(), w.planID, monday); ok {
		t.Error("a revoked mandate is active")
	}
	got, err := st.GetMandate(context.Background(), m.ID)
	if err != nil || got.Status != MandateRevoked || got.RevokedReason != "stop" {
		t.Errorf("mandate = %+v, %v", got, err)
	}
	if s, _ := st.MandateStatus(context.Background(), w.planID, monday); s != StatusAwaitingMandate {
		t.Errorf("status = %q", s)
	}
	// A revoked mandate never executes.
	p := w.newProc()
	p.run(got, monday)
	if w.ex.orderCount() != 0 || w.asker.count() != 0 {
		t.Error("a revoked mandate bought or asked")
	}
}
