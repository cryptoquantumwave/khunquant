package dca

import (
	"context"
	"testing"
	"time"
)

func storeWithPlan(t *testing.T) (*Store, int64) {
	t.Helper()
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	now := time.Date(2026, 10, 12, 2, 0, 0, 0, time.UTC)
	id, err := st.SavePlan(context.Background(), &Plan{Name: "p", Provider: "bitkub", Symbol: "BTC/THB", AmountPerOrder: 2000,
		FrequencyExpr: "0 9 * * 1", Timezone: "Asia/Bangkok", StartDate: now, Enabled: true, CreatedAt: now, UpdatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	return st, id
}

func insertTestMandate(t *testing.T, st *Store, planID int64, mut func(*Mandate)) Mandate {
	t.Helper()
	now := time.Date(2026, 10, 12, 2, 0, 0, 0, time.UTC)
	m := Mandate{ID: "mnd_" + t.Name(), Kind: "dca", PlanID: planID, Provider: "bitkub", Symbol: "BTC/THB", Side: "buy", Quote: "THB",
		MaxPerRunQuote: 2000, MaxPerPeriodQuote: 8000, MaxPerExecutionUSD: 55, MaxPerPeriodUSD: 220, Period: "month", Timezone: "Asia/Bangkok",
		ValidFrom: now, ExpiresAt: now.AddDate(0, 6, 0), Status: MandateActive}
	if mut != nil {
		mut(&m)
	}
	m.ProposalID = "p_" + m.ID
	if _, err := st.db.Exec(`INSERT INTO dca_mandates (`+mandateCols+`, created_at_ms) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		m.ID, m.Kind, m.PlanID, m.Provider, m.Account, m.Symbol, m.Side, m.Quote, m.MaxPerRunQuote, m.MaxPerPeriodQuote,
		m.MaxPerExecutionUSD, m.MaxPerPeriodUSD, m.Period, m.Timezone, m.ValidFrom.UnixMilli(), m.ExpiresAt.UnixMilli(), m.Status,
		m.ProposalID, "", 0, now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	return m
}

// RecordMandateRun writes the execution row, the plan totals and the usage in one
// transaction keyed by the workflow id: a replay writes nothing again.
func TestMandateStore_RecordRunIsOnceAndAtomic(t *testing.T) {
	st, planID := storeWithPlan(t)
	m := insertTestMandate(t, st, planID, nil)
	at := time.Date(2026, 10, 12, 2, 0, 5, 0, time.UTC)
	exec := func() *Execution {
		return &Execution{PlanID: planID, ExecutedAt: at, Symbol: "BTC/THB", Provider: "bitkub", AmountQuote: 2000, FilledPrice: 2e6,
			FilledQuantity: 0.001, Status: "completed", Side: "buy", CreatedAt: at}
	}
	ctx := context.Background()
	for i, want := range []bool{true, false, false} {
		got, err := st.RecordMandateRun(ctx, exec(), &StatsDelta{Side: "buy", AmountQuote: 2000, Quantity: 0.001}, m.ID, "wf1", 55.2, false, at)
		if err != nil || got != want {
			t.Fatalf("record %d = %v, %v; want %v", i, got, err, want)
		}
	}
	rows, _ := st.CountExecutions(ctx, planID)
	plan, _ := st.GetPlan(ctx, planID)
	usage, _ := st.MandateUsage(ctx, m.ID, time.Time{}, false)
	if rows != 1 || plan.TotalInvested != 2000 || usage != 55.2 {
		t.Errorf("rows %d, invested %v, usage %v after three records of one workflow", rows, plan.TotalInvested, usage)
	}
	if e, _ := st.ExecutionByWorkflow(ctx, "wf1"); e == nil || e.Status != "completed" {
		t.Errorf("ExecutionByWorkflow = %+v", e)
	}
	// A paper run's usage counts only while paper mode is on.
	if _, err := st.RecordMandateRun(ctx, exec(), nil, m.ID, "wf2", 10, true, at); err != nil {
		t.Fatal(err)
	}
	live, _ := st.MandateUsage(ctx, m.ID, time.Time{}, false)
	paper, _ := st.MandateUsage(ctx, m.ID, time.Time{}, true)
	if live != 55.2 || paper != 65.2 {
		t.Errorf("usage live %v paper %v", live, paper)
	}
	// A failed run records no usage.
	failed := exec()
	failed.Status = "failed"
	if _, err := st.RecordMandateRun(ctx, failed, nil, m.ID, "wf3", 0, false, at); err != nil {
		t.Fatal(err)
	}
	if u, _ := st.MandateUsage(ctx, m.ID, time.Time{}, true); u != 65.2 {
		t.Errorf("a failed run changed the usage: %v", u)
	}
}

func TestMandateStore_PeriodsAreInTheOwnersZone(t *testing.T) {
	// Sunday 2026-11-01 20:00 UTC is already Monday 2026-11-02 03:00 in Bangkok: a new week and a new month.
	now := time.Date(2026, 11, 1, 20, 0, 0, 0, time.UTC)
	m, _ := PeriodStart(now, "month", "Asia/Bangkok")
	w, _ := PeriodStart(now, "week", "Asia/Bangkok")
	bkk, _ := time.LoadLocation("Asia/Bangkok")
	if !m.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, bkk)) || !w.Equal(time.Date(2026, 11, 2, 0, 0, 0, 0, bkk)) {
		t.Errorf("month starts %v, week starts %v", m, w)
	}
	if _, err := PeriodStart(now, "year", "Asia/Bangkok"); err == nil {
		t.Error("an unknown period was accepted")
	}
	if _, err := PeriodStart(now, "month", "Not/AZone"); err == nil {
		t.Error("an unknown zone was accepted")
	}
}

func TestMandateStore_LifecycleAndStatus(t *testing.T) {
	st, planID := storeWithPlan(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 12, 2, 0, 0, 0, time.UTC)
	if s, _ := st.MandateStatus(ctx, planID, now); s != "" {
		t.Errorf("a plan with no mandate is %q", s)
	}
	if err := st.SetMandatePending(ctx, planID, "prop1", now); err != nil {
		t.Fatal(err)
	}
	if s, _ := st.MandateStatus(ctx, planID, now); s != StatusAwaitingMandate {
		t.Errorf("pending = %q", s)
	}
	if first, _ := st.MarkReminded(ctx, planID); !first {
		t.Error("the first reminder was not marked")
	}
	if again, _ := st.MarkReminded(ctx, planID); again {
		t.Error("a second reminder was allowed")
	}
	m := insertTestMandate(t, st, planID, nil)
	if s, _ := st.MandateStatus(ctx, planID, now); s != StatusMandated {
		t.Errorf("active = %q", s)
	}
	if n, _ := st.ExpireMandates(ctx, now.AddDate(0, 7, 0)); n != 1 {
		t.Errorf("expired %d", n)
	}
	if _, ok, _ := st.ActiveMandateForPlan(ctx, planID, now); ok {
		t.Error("an expired mandate is active")
	}
	if got, ok, _ := st.LatestMandateForPlan(ctx, planID); !ok || got.ID != m.ID || got.Status != MandateExpired {
		t.Errorf("latest = %+v, %v", got, ok)
	}
	if s, _ := st.MandateStatus(ctx, planID, now); s != StatusAwaitingMandate {
		t.Errorf("expired = %q", s)
	}
}

func TestMandateStore_AskUnprovenAndDailyNotice(t *testing.T) {
	st, planID := storeWithPlan(t)
	ctx := context.Background()
	if ok, _ := st.ReserveSlotAsk(ctx, planID, 1); !ok {
		t.Fatal("the first reservation failed")
	}
	if ok, _ := st.ReserveSlotAsk(ctx, planID, 1); ok {
		t.Error("a slot was reserved twice")
	}
	if ok, _ := st.ReserveSlotAsk(ctx, planID, 2); !ok {
		t.Error("another slot was refused")
	}
	if err := st.MarkUnproven(ctx, planID, "wf", "why", time.Now()); err != nil {
		t.Fatal(err)
	}
	if r, _ := st.Unproven(ctx, planID); r != "why" {
		t.Errorf("unproven = %q", r)
	}
	if err := st.ClearUnproven(ctx, planID); err != nil {
		t.Fatal(err)
	}
	if r, _ := st.Unproven(ctx, planID); r != "" {
		t.Errorf("unproven after clear = %q", r)
	}
	if a, _ := st.FirstToday(ctx, "kill", "2026-10-12"); !a {
		t.Error("first notice of the day not allowed")
	}
	if b, _ := st.FirstToday(ctx, "kill", "2026-10-12"); b {
		t.Error("second notice of the day allowed")
	}
	if c, _ := st.FirstToday(ctx, "kill", "2026-10-13"); !c {
		t.Error("next day not allowed")
	}
}
