package heartbeat

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/state"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

func ledgerService(t *testing.T) (*HeartbeatService, *fakeClock, string) {
	t.Helper()
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "HEARTBEAT.md"), []byte("- check SOL\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	clk := newFakeClock()
	hs := NewHeartbeatService(ws, 30, true, nil)
	hs.clock = clk
	return hs, clk, ws
}

func TestLedger_HeartbeatMessageSendIsRecordedAndShownNextPrompt(t *testing.T) {
	hs, clk, ws := ledgerService(t)
	mt := tools.NewMessageTool()
	mt.SetSendCallback(func(string, string, string) error { return nil })
	hs.ObserveMessageSends(mt)

	// A heartbeat turn's send is recorded...
	hbCtx := tools.WithToolOrigin(context.Background(), tools.OriginHeartbeat)
	if res := mt.Execute(hbCtx, map[string]any{
		"content": "SOL is down 12%\ntoday", "channel": "telegram", "chat_id": "42",
	}); res.IsError {
		t.Fatalf("send failed: %s", res.ForLLM)
	}
	// ...a cron job's or an untagged turn's send through the same tool is not.
	cronCtx := tools.WithToolOrigin(context.Background(), tools.OriginCron)
	mt.Execute(cronCtx, map[string]any{"content": "cron says hi", "channel": "telegram", "chat_id": "42"})
	mt.Execute(context.Background(), map[string]any{"content": "untagged", "channel": "telegram", "chat_id": "42"})

	clk.now = clk.now.Add(30 * time.Minute)
	prompt := hs.buildPrompt()
	want := "Alerts you already sent in the last 24h:\n- 2026-07-13 09:00 +00:00 [telegram] SOL is down 12% today\n"
	if !strings.Contains(prompt, want) {
		t.Fatalf("prompt does not list the sent alert:\n%s", prompt)
	}
	if strings.Contains(prompt, "cron says hi") || strings.Contains(prompt, "untagged") || strings.Contains(prompt, "(none)") {
		t.Fatalf("prompt lists a send that was not a heartbeat's, or says none:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Current time: 2026-07-13 09:30:00 +00:00") {
		t.Errorf("legacy prompt time lacks the UTC offset:\n%s", prompt)
	}

	// The file is under the workspace, one JSON object per line.
	data, err := os.ReadFile(filepath.Join(ws, "heartbeat", "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var e SentAlert
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &e); err != nil || e.ChatID != "42" || e.Channel != "telegram" {
		t.Fatalf("ledger content %q (%v)", data, err)
	}
}

func TestLedger_SendResponseIsRecorded(t *testing.T) {
	ws := t.TempDir()
	st := state.NewManager(ws)
	if err := st.SetLastChannel("telegram:4242"); err != nil {
		t.Fatal(err)
	}
	mb := bus.NewMessageBus()
	defer mb.Close()
	hs := NewHeartbeatService(ws, 30, true, st)
	hs.SetBus(mb)

	hs.sendResponse("BTC funding spiked")
	got := recentSent(ws, time.Now())
	if len(got) != 1 || got[0].Text != "BTC funding spiked" || got[0].ChatID != "4242" {
		t.Fatalf("ledger = %+v", got)
	}
}

func TestLedger_TextIsTruncated(t *testing.T) {
	ws := t.TempDir()
	now := time.Now()
	recordSent(ws, now, "telegram", "1", strings.Repeat("é", 500))
	got := recentSent(ws, now)
	if len(got) != 1 || len([]rune(got[0].Text)) != ledgerTextMax+3 || !strings.HasSuffix(got[0].Text, "...") {
		t.Fatalf("entry = %+v", got)
	}
}

func TestLedger_OldEntriesHiddenAfter24hAndPrunedAfter48h(t *testing.T) {
	ws := t.TempDir()
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	recordSent(ws, now.Add(-60*time.Hour), "telegram", "1", "ancient")
	recordSent(ws, now.Add(-30*time.Hour), "telegram", "1", "yesterday")
	recordSent(ws, now.Add(-1*time.Hour), "telegram", "1", "recent")

	// The 60h-old entry went when the 30h-old one was written; the 30h-old one is
	// kept (for 48h) but not shown.
	data, err := os.ReadFile(ledgerPath(ws))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "ancient") {
		t.Errorf("an entry older than 48h was not pruned:\n%s", data)
	}
	if !strings.Contains(string(data), "yesterday") {
		t.Errorf("a 30h-old entry was pruned (they are kept for 48h):\n%s", data)
	}

	got := recentSent(ws, now)
	if len(got) != 1 || got[0].Text != "recent" {
		t.Fatalf("recentSent = %+v, want only the entry from the last 24h", got)
	}
	if p := LegacyPrompt("t", "tasks", got); strings.Contains(p, "yesterday") || !strings.Contains(p, "recent") {
		t.Errorf("prompt shows the wrong entries:\n%s", p)
	}
}

func TestLedger_MissingOrCorruptFileIsNone(t *testing.T) {
	hs, _, ws := ledgerService(t)
	if p := hs.buildPrompt(); !strings.HasSuffix(p, "Alerts you already sent in the last 24h:\n(none)\n") {
		t.Fatalf("missing ledger:\n%s", p)
	}

	if err := os.MkdirAll(filepath.Join(ws, "heartbeat"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ledgerPath(ws), []byte("{not json\n\x00\x01garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	if p := hs.buildPrompt(); !strings.HasSuffix(p, "(none)\n") {
		t.Fatalf("corrupt ledger:\n%s", p)
	}

	// A corrupt file does not stop a new record: the bad lines are dropped.
	hs.RecordSent("telegram", "1", "after corruption")
	if p := hs.buildPrompt(); !strings.Contains(p, "after corruption") {
		t.Fatalf("a record after a corrupt file was lost:\n%s", p)
	}
}

func TestLedger_WriteFailureIsNotFatal(t *testing.T) {
	ws := t.TempDir()
	// "heartbeat" is a file, so the ledger's directory cannot be created.
	if err := os.WriteFile(filepath.Join(ws, "heartbeat"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	recordSent(ws, time.Now(), "telegram", "1", "text") // must not panic
	if got := recentSent(ws, time.Now()); len(got) != 0 {
		t.Fatalf("recentSent = %+v", got)
	}
}

// The guardrails are part of the legacy prompt, between the tasks and the list of
// alerts already sent. They are not in HEARTBEAT.md or the governed prompt, which
// governed and sweep turns read and pay for.
func TestLegacyPrompt_Guardrails(t *testing.T) {
	p := LegacyPrompt("2026-07-13 09:00:00 +00:00", "- my task\n", []SentAlert{{
		Time: time.Date(2026, 7, 13, 8, 0, 0, 0, time.UTC), Channel: "telegram", ChatID: "1", Text: "SOL down",
	}})
	flat := strings.Join(strings.Fields(p), " ")
	for _, want := range []string{
		"Quiet hours 22:00-07:00 in the timezone in USER.md: critical events only",
		"closed for hours",
		"unless it crossed the next severity tier; then say it is an update",
		"ONE message per heartbeat",
		"3 non-critical briefings",
		"If the tasks above give different rules, follow the tasks.",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("legacy prompt lacks the guardrail %q", want)
		}
	}
	task, guard, sent := strings.Index(p, "- my task"), strings.Index(p, LegacyGuardrails), strings.Index(p, "Alerts you already sent")
	if task < 0 || guard < task || sent < guard {
		t.Errorf("order must be tasks, guardrails, sent alerts; got %d, %d, %d", task, guard, sent)
	}
	if len(LegacyGuardrails) > 500 {
		t.Errorf("the guardrail block is %d bytes; keep it under 500", len(LegacyGuardrails))
	}

	for name, other := range map[string]string{
		"template":        TemplateV2(),
		"governed prompt": GovernedPrompt("t", "- my task\n"),
	} {
		if strings.Contains(other, "Guardrails:") || strings.Contains(other, "22:00-07:00") {
			t.Errorf("the %s carries the legacy-only guardrails", name)
		}
	}
}
