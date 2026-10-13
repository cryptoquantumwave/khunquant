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
	for _, want := range []string{
		"`respond_proactive`", "`dedup_key`", "`decision: silent`", "`message`", "legacy heartbeat", userTasksMarker,
	} {
		if !strings.Contains(v2, want) {
			t.Errorf("v2 template lacks %q", want)
		}
	}
	for _, banned := range []string{
		"spawn", "ledger", "Ledger", "MEMORY.md", "append_file", "edit_file",
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

// A missing HEARTBEAT.md is recreated as the idle v2 template in every mode, and
// neither default may start paid turns.
func TestDefaultTemplate_IsV2InEveryMode(t *testing.T) {
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
		if got := string(data); got != idleTemplateV2() || !IsV2Template(got) || IsV1Template(got) ||
			strings.Contains(got, "spawn") {
			t.Errorf("%s: the default is not the idle v2 template:\n%s", c, got)
		}
		if prompt := hs.buildPrompt(); prompt != "" {
			t.Errorf("%s: the default template must stay idle, got a prompt", c)
		}
	}
}

// workspace/HEARTBEAT.md is what onboarding copies to a new install. It is the
// v2 template, byte for byte: there is one text, and this holds the copy to it.
func TestWorkspaceHeartbeatFile_IsTheV2Template(t *testing.T) {
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test directory")
		}
		dir = parent
	}
	got, err := os.ReadFile(filepath.Join(dir, "workspace", "HEARTBEAT.md"))
	if err != nil || len(got) == 0 {
		t.Fatalf("reading workspace/HEARTBEAT.md: %v (%d bytes)", err, len(got))
	}
	if string(got) != TemplateV2() {
		t.Fatal("workspace/HEARTBEAT.md differs from pkg/heartbeat/template_v2.md: copy the template over it")
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

	// The wrapper is one function, so the size of a governed turn can be measured
	// on the real text (pkg/agent budget tests); it must be what the service sends.
	wantGoverned := "# Heartbeat Check\n\nCurrent time: 2026-07-13 09:00:00\n\n" +
		"This is a scheduled heartbeat check. Review the following tasks, look at the data\n" +
		"with your read tools, and report through respond_proactive.\n\n- check SOL\n\n"
	if got != wantGoverned {
		t.Fatalf("governed prompt changed:\n%q\nwant\n%q", got, wantGoverned)
	}
	if exported := GovernedPrompt("2026-07-13 09:00:00", "- check SOL\n"); exported != got {
		t.Fatalf("GovernedPrompt differs from what the service builds:\n%q\n%q", exported, got)
	}
}

func TestTemplateAdvisory(t *testing.T) {
	v1 := "x\nHow delivery actually works\n"
	v2 := TemplateV2()
	for name, tc := range map[string]struct {
		content  string
		contract Contract
		want     string // substring of the advisory; "" = none
		warn     bool
	}{
		"governed + v1":      {v1, ContractGoverned, "khunquant heartbeat upgrade-template", true},
		"governed + v1 says": {v1, ContractGoverned, "does not have", true},
		"governed + v2":      {v2, ContractGoverned, "", false},
		"governed + custom":  {"- my own tasks\n", ContractGoverned, "", false},
		"legacy + v1":        {v1, ContractLegacy, "khunquant heartbeat upgrade-template", false},
		"legacy + v1 keeps":  {v1, ContractLegacy, "keeps working", false},
		"legacy + custom":    {"- my own tasks\n", ContractLegacy, "", false},
		"legacy + v2":        {v2, ContractLegacy, "", false},
	} {
		got, warn := TemplateAdvisory(tc.content, tc.contract)
		if tc.want == "" && got != "" {
			t.Errorf("%s: unexpected advisory %q", name, got)
		}
		if tc.want != "" && (!strings.Contains(got, tc.want) || warn != tc.warn) {
			t.Errorf("%s: advisory %q (warn=%v) lacks %q or has the wrong level (want warn=%v)",
				name, got, warn, tc.want, tc.warn)
		}
	}
}

// Start logs the advisory once - a warning for a governed heartbeat on a v1 file,
// a notice for a legacy one, nothing for a v2 file - and never changes the file.
func TestStart_AdvisoryOncePerStart(t *testing.T) {
	var warned, noticed []string
	prevW, prevI := warnLog, infoLog
	warnLog = func(msg string, _ map[string]any) { warned = append(warned, msg) }
	infoLog = func(msg string, _ map[string]any) { noticed = append(noticed, msg) }
	t.Cleanup(func() { warnLog, infoLog = prevW, prevI })

	const v1 = "- t\n\nHow delivery actually works\n"
	for name, tc := range map[string]struct {
		content  string
		contract Contract
		warns    int
		notices  int
	}{
		"governed + v1 file": {v1, ContractGoverned, 1, 0},
		"legacy + v1 file":   {v1, ContractLegacy, 0, 1},
		"governed + v2 file": {TemplateV2(), ContractGoverned, 0, 0},
		"legacy + v2 file":   {TemplateV2(), ContractLegacy, 0, 0},
		"legacy + own file":  {"- my tasks\n", ContractLegacy, 0, 0},
	} {
		warned, noticed = nil, nil
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
		if err := hs.Start(); err != nil { // already running: no second line
			t.Fatal(err)
		}
		hs.Stop()
		if len(warned) != tc.warns || len(noticed) != tc.notices {
			t.Errorf("%s: %d warnings %q and %d notices %q, want %d and %d",
				name, len(warned), warned, len(noticed), noticed, tc.warns, tc.notices)
		}
		if got, _ := os.ReadFile(path); string(got) != tc.content {
			t.Errorf("%s: the user's HEARTBEAT.md was changed", name)
		}

		// A restart or reload is a new Start, and says it again.
		warned, noticed = nil, nil
		if err := hs.Start(); err != nil {
			t.Fatal(err)
		}
		hs.Stop()
		if len(warned) != tc.warns || len(noticed) != tc.notices {
			t.Errorf("%s: after a restart %d warnings and %d notices, want %d and %d",
				name, len(warned), len(noticed), tc.warns, tc.notices)
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
