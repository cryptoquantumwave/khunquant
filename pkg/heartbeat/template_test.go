package heartbeat

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/state"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

func TestTemplateV2_Contract(t *testing.T) {
	v2 := TemplateV2()
	if !IsV2Template(v2) || IsV1Template(v2) {
		t.Fatalf("v2 template: IsV2=%v IsV1=%v", IsV2Template(v2), IsV1Template(v2))
	}
	for _, want := range []string{"`respond_proactive`", "`dedup_key`", "`decision: silent`", userTasksMarker} {
		if !strings.Contains(v2, want) {
			t.Errorf("v2 template lacks %q", want)
		}
	}
	for _, banned := range []string{
		"`message`", "spawn", "ledger", "Ledger", "MEMORY.md", "append_file", "edit_file",
		"HEARTBEAT_OK", "Quiet hours", "Rate limit", "How delivery actually works",
	} {
		if strings.Contains(v2, banned) {
			t.Errorf("v2 template still contains %q", banned)
		}
	}
	// Every line is paid for in tokens on every heartbeat.
	if n := len(strings.Split(strings.TrimSpace(v2), "\n")); n > 100 {
		t.Errorf("v2 template is %d lines; keep it short", n)
	}
	if !heartbeatHasUserTasks(v2) {
		t.Error("the shipped v2 template must have a default task so new installs run")
	}
	if heartbeatHasUserTasks(idleTemplateV2()) {
		t.Error("the idle v2 template must run nothing until the user adds a task")
	}
	if !IsV2Template(idleTemplateV2()) {
		t.Error("the idle v2 template lost the v2 marker")
	}
}

func TestV1Template_IsRecognised(t *testing.T) {
	v1 := "# Heartbeat\n## ⚠️ How delivery actually works — READ THIS\nuse `message`\n"
	if !IsV1Template(v1) || IsV2Template(v1) {
		t.Fatal("a v1 file must be recognised as v1 and not v2")
	}
}

// A missing HEARTBEAT.md is recreated to match the contract: a governed install
// must never be given v1 instructions, and neither default may start paid turns.
func TestDefaultTemplate_MatchesTheContract(t *testing.T) {
	for _, c := range []Contract{ContractLegacy, ContractGoverned} {
		ws := t.TempDir()
		hs := NewHeartbeatService(ws, 30, true, nil)
		hs.SetContract(c)
		if prompt := hs.buildPrompt(); prompt != "" {
			t.Fatalf("%s: a missing file produced a prompt: %q", c, prompt)
		}
		data, err := os.ReadFile(filepath.Join(ws, "HEARTBEAT.md"))
		if err != nil {
			t.Fatalf("%s: %v", c, err)
		}
		got := string(data)
		switch c {
		case ContractGoverned:
			if !IsV2Template(got) || IsV1Template(got) || strings.Contains(got, "spawn") {
				t.Errorf("governed default is not v2:\n%s", got)
			}
		case ContractLegacy:
			if got != legacyDefaultTemplate || IsV2Template(got) || !strings.Contains(got, "use the spawn tool") {
				t.Errorf("legacy default changed:\n%s", got)
			}
		}
		if prompt := hs.buildPrompt(); prompt != "" {
			t.Errorf("%s: the default template must stay idle, got a prompt", c)
		}
	}
}

func TestBuildPrompt_WrapperByContract(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "HEARTBEAT.md"), []byte("- check SOL\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	clk := newFakeClock()
	hs := NewHeartbeatService(ws, 30, true, nil)
	hs.clock = clk

	// Legacy: byte-for-byte what it has always been.
	wantLegacy := "# Heartbeat Check\n\nCurrent time: 2026-07-13 09:00:00\n\n" +
		"You are a proactive AI assistant. This is a scheduled heartbeat check.\n" +
		"Review the following tasks and execute any necessary actions using available skills.\n" +
		"If there is nothing that requires attention, respond ONLY with: HEARTBEAT_OK\n\n- check SOL\n\n"
	if got := hs.buildPrompt(); got != wantLegacy {
		t.Fatalf("legacy prompt changed:\n%q\nwant\n%q", got, wantLegacy)
	}

	hs.SetContract(ContractGoverned)
	got := hs.buildPrompt()
	if !strings.Contains(got, "- check SOL") || !strings.Contains(got, "respond_proactive") {
		t.Errorf("governed prompt lacks the tasks or the contract:\n%s", got)
	}
	if strings.Contains(got, "HEARTBEAT_OK") || strings.Contains(got, "execute any necessary actions") {
		t.Errorf("governed prompt still carries the legacy wrapper:\n%s", got)
	}
}

func TestTemplateAdvisory(t *testing.T) {
	v1 := "x\nHow delivery actually works\n"
	v2 := TemplateV2()
	for name, tc := range map[string]struct {
		content  string
		contract Contract
		want     string // substring of the advisory; "" = none
	}{
		"governed + v1":      {v1, ContractGoverned, "khunquant heartbeat upgrade-template"},
		"governed + v1 says": {v1, ContractGoverned, "does not have"},
		"governed + v2":      {v2, ContractGoverned, ""},
		"governed + custom":  {"- my own tasks\n", ContractGoverned, ""},
		"legacy + v1":        {v1, ContractLegacy, ""},
		"legacy + custom":    {"- my own tasks\n", ContractLegacy, ""},
		"legacy + v2":        {v2, ContractLegacy, "heartbeats run and deliver nothing"},
		"legacy + v2 fix":    {v2, ContractLegacy, "HEARTBEAT.md.v1.bak"},
		"legacy + v2 fresh":  {v2, ContractLegacy, "delete HEARTBEAT.md to get the default back"},
	} {
		got := TemplateAdvisory(tc.content, tc.contract)
		if tc.want == "" && got != "" {
			t.Errorf("%s: unexpected advisory %q", name, got)
		}
		if tc.want != "" && !strings.Contains(got, tc.want) {
			t.Errorf("%s: advisory %q lacks %q", name, got, tc.want)
		}
	}
}

// Start logs the advisory once, and never changes the file.
func TestStart_WarnsOncePerStartAboutAMismatchedTemplate(t *testing.T) {
	var warned []string
	prev := warnLog
	warnLog = func(msg string, _ map[string]any) { warned = append(warned, msg) }
	t.Cleanup(func() { warnLog = prev })

	for name, tc := range map[string]struct {
		content  string
		contract Contract
		warns    int
	}{
		"governed + v1 file": {"- t\n\nHow delivery actually works\n", ContractGoverned, 1},
		"legacy + v2 file":   {TemplateV2(), ContractLegacy, 1},
		"governed + v2 file": {TemplateV2(), ContractGoverned, 0},
		"legacy + v1 file":   {"- t\n\nHow delivery actually works\n", ContractLegacy, 0},
	} {
		warned = nil
		ws := t.TempDir()
		path := filepath.Join(ws, "HEARTBEAT.md")
		if err := os.WriteFile(path, []byte(tc.content), 0o644); err != nil {
			t.Fatal(err)
		}
		hs := NewHeartbeatService(ws, 30, true, nil)
		hs.clock = newFakeClock()
		hs.SetContract(tc.contract)
		if err := hs.Start(); err != nil {
			t.Fatal(err)
		}
		if err := hs.Start(); err != nil { // already running: no second warning
			t.Fatal(err)
		}
		hs.Stop()
		if len(warned) != tc.warns {
			t.Errorf("%s: %d warnings %q, want %d", name, len(warned), warned, tc.warns)
		}
		if got, _ := os.ReadFile(path); string(got) != tc.content {
			t.Errorf("%s: the user's HEARTBEAT.md was changed", name)
		}

		// A restart or reload is a new Start, and warns again.
		warned = nil
		if err := hs.Start(); err != nil {
			t.Fatal(err)
		}
		hs.Stop()
		if len(warned) != tc.warns {
			t.Errorf("%s: after a restart %d warnings, want %d", name, len(warned), tc.warns)
		}
	}
}

func TestStart_NoWarningWhenTheFileIsMissingOrTheServiceDisabled(t *testing.T) {
	var warned int
	prev := warnLog
	warnLog = func(string, map[string]any) { warned++ }
	t.Cleanup(func() { warnLog = prev })

	hs := NewHeartbeatService(t.TempDir(), 30, true, nil)
	hs.clock = newFakeClock()
	hs.SetContract(ContractGoverned)
	_ = hs.Start()
	hs.Stop()

	ws := t.TempDir()
	_ = os.WriteFile(filepath.Join(ws, "HEARTBEAT.md"), []byte("How delivery actually works"), 0o644)
	off := NewHeartbeatService(ws, 30, false, nil)
	off.SetContract(ContractGoverned)
	_ = off.Start()
	if warned != 0 {
		t.Fatalf("%d warnings for a missing file or a disabled service, want 0", warned)
	}
}

// A governed heartbeat is not addressed to the last active chat, and whatever
// its handler returns is never published from the service.
func TestGoverned_IgnoresLastChannelAndNeverPublishes(t *testing.T) {
	ws := t.TempDir()
	st := state.NewManager(ws)
	if err := st.SetLastChannel("telegram:-1001234567890"); err != nil { // a group
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "HEARTBEAT.md"), []byte("- task\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mb := bus.NewMessageBus()
	defer mb.Close()

	hs := NewHeartbeatService(ws, 30, true, st)
	hs.SetContract(ContractGoverned)
	hs.SetBus(mb)
	hs.stopChan = make(chan struct{}) // Enable for testing

	var gotChannel, gotChat string
	called := false
	hs.SetContextHandler(func(_ context.Context, _ string, ch, chat string) *tools.ToolResult {
		called = true
		gotChannel, gotChat = ch, chat
		// Not silent, with text for a user: a legacy service would publish this.
		return &tools.ToolResult{ForLLM: "summary", ForUser: "hello"}
	})
	hs.executeHeartbeat()

	if !called {
		t.Fatal("the handler was not called")
	}
	if gotChannel != "" || gotChat != "" {
		t.Fatalf("a governed handler was given (%q, %q), want no destination", gotChannel, gotChat)
	}
	select {
	case m := <-mb.OutboundChan():
		t.Fatalf("a governed heartbeat published %+v", m)
	case <-time.After(50 * time.Millisecond):
	}
}
