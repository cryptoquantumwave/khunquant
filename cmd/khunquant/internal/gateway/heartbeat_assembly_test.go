package gateway

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/attention"
	"github.com/cryptoquantumwave/khunquant/pkg/attention/attentiontest"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/heartbeat"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

func withHeartbeat(mode string) func(*config.Config) {
	return func(c *config.Config) {
		c.Heartbeat.Enabled = true
		c.Proactive.Heartbeat.Mode = mode
	}
}

// The mode is read through Effective at assembly time: governed only with a live
// outbox, legacy otherwise, and a reload switches it without a restart.
func TestHeartbeatAssembly_ModeFollowsEffectiveAndReload(t *testing.T) {
	governedLive := compose(withOwner("1001"), withAttentionMode(config.ProactiveModeLive),
		withHeartbeat(config.HeartbeatModeGoverned))

	f := newAssemblyFixture(t, governedLive)
	if got := f.services.HeartbeatService.Contract(); got != heartbeat.ContractGoverned {
		t.Fatalf("governed + live attention + owner: contract = %s", got)
	}

	// Attention to shadow: Effective downgrades governed to legacy, and the
	// reload picks that up.
	if err := f.reload(compose(withOwner("1001"), withAttentionMode(config.ProactiveModeShadow),
		withHeartbeat(config.HeartbeatModeGoverned))); err != nil {
		t.Fatal(err)
	}
	if got := f.services.HeartbeatService.Contract(); got != heartbeat.ContractLegacy {
		t.Fatalf("governed + shadow attention: contract = %s, want legacy", got)
	}

	// Legacy stays legacy even with everything live.
	if err := f.reload(compose(withOwner("1001"), withAttentionMode(config.ProactiveModeLive),
		withHeartbeat(config.HeartbeatModeLegacy))); err != nil {
		t.Fatal(err)
	}
	if got := f.services.HeartbeatService.Contract(); got != heartbeat.ContractLegacy {
		t.Fatalf("legacy mode: contract = %s", got)
	}

	// And back to governed.
	if err := f.reload(governedLive); err != nil {
		t.Fatal(err)
	}
	if got := f.services.HeartbeatService.Contract(); got != heartbeat.ContractGoverned {
		t.Fatalf("after reloading to governed: contract = %s", got)
	}
	if f.services.Attention == nil || f.services.attentionRun == nil {
		t.Fatal("a governed heartbeat is running without a live outbox")
	}
}

func withHeartbeatModel(tier string) func(*config.Config) {
	return func(c *config.Config) { c.Proactive.Heartbeat.Model = tier }
}

// recordGovernedTiers swaps the handler factory for one that records the tier the
// assembly gave it, and returns the tiers seen so far.
func recordGovernedTiers(t *testing.T) func() []proactive.ModelTier {
	t.Helper()
	var mu sync.Mutex
	var seen []proactive.ModelTier
	orig := newGovernedHandler
	newGovernedHandler = func(
		run proactiveRunner, out attentionSubmitter, interval time.Duration, tier proactive.ModelTier,
		toolNames []string,
	) heartbeat.ContextHandler {
		mu.Lock()
		seen = append(seen, tier)
		mu.Unlock()
		return orig(run, out, interval, tier, toolNames)
	}
	t.Cleanup(func() { newGovernedHandler = orig })
	return func() []proactive.ModelTier {
		mu.Lock()
		defer mu.Unlock()
		return append([]proactive.ModelTier(nil), seen...)
	}
}

// The governed handler is built with proactive.heartbeat.model as Effective
// resolves it: primary by default, light only when asked for, and a reload that changes it builds a handler with the new one.
func TestHeartbeatAssembly_TierFollowsConfigAndReload(t *testing.T) {
	tiers := recordGovernedTiers(t)
	governed := func(model string) func(*config.Config) {
		return compose(withOwner("1001"), withAttentionMode(config.ProactiveModeLive),
			withHeartbeat(config.HeartbeatModeGoverned), withHeartbeatModel(model))
	}
	want := func(label string, wantTiers ...proactive.ModelTier) {
		t.Helper()
		got := tiers()
		if len(got) != len(wantTiers) {
			t.Fatalf("%s: handler built with tiers %v, want %v", label, got, wantTiers)
		}
		for i := range got {
			if got[i] != wantTiers[i] {
				t.Fatalf("%s: handler built with tiers %v, want %v", label, got, wantTiers)
			}
		}
	}

	// Nothing says otherwise: the normal model.
	f := newAssemblyFixture(t, compose(withOwner("1001"), withAttentionMode(config.ProactiveModeLive),
		withHeartbeat(config.HeartbeatModeGoverned)))
	want("default", proactive.ModelTierPrimary)

	if err := f.reload(governed("light")); err != nil {
		t.Fatal(err)
	}
	want("reload to light", proactive.ModelTierPrimary, proactive.ModelTierLight)

	if err := f.reload(governed("primary")); err != nil {
		t.Fatal(err)
	}
	want("reload back to primary", proactive.ModelTierPrimary, proactive.ModelTierLight, proactive.ModelTierPrimary)

	// A typo makes the section invalid, which Effective treats as no owner: the
	// heartbeat goes back to legacy rather than running governed on any model.
	if err := f.reload(governed("lite")); err != nil {
		t.Fatal(err)
	}
	want("reload to an unknown tier", proactive.ModelTierPrimary, proactive.ModelTierLight, proactive.ModelTierPrimary)
	if got := f.services.HeartbeatService.Contract(); got != heartbeat.ContractLegacy {
		t.Fatalf("an unknown tier: contract = %s, want legacy", got)
	}

	// A legacy heartbeat builds no governed handler, whatever the tier says.
	before := len(tiers())
	if err := f.reload(compose(withOwner("1001"), withAttentionMode(config.ProactiveModeLive),
		withHeartbeat(config.HeartbeatModeLegacy), withHeartbeatModel("light"))); err != nil {
		t.Fatal(err)
	}
	if n := len(tiers()); n != before {
		t.Fatalf("a legacy heartbeat built a governed handler (%d -> %d)", before, n)
	}
}

func TestHeartbeatAssembly_DefaultInstallIsLegacy(t *testing.T) {
	f := newAssemblyFixture(t, func(c *config.Config) { c.Heartbeat.Enabled = true })
	if got := f.services.HeartbeatService.Contract(); got != heartbeat.ContractLegacy {
		t.Fatalf("default install: contract = %s", got)
	}
}

// The v1 template under a governed heartbeat: ONE warning per start and one per
// reload, naming the command and saying the turn still works.
func TestHeartbeatAssembly_V1TemplateWarnsOncePerStartAndReload(t *testing.T) {
	const phrase = "khunquant heartbeat upgrade-template"
	governedLive := compose(withOwner("1001"), withAttentionMode(config.ProactiveModeLive),
		withHeartbeat(config.HeartbeatModeGoverned))

	logs := captureLogs(t)
	f := newAssemblyFixtureNoBuild(t, governedLive)
	// A v1 file with no tasks below the marker: the warning fires, no turn runs.
	v1 := heartbeat.V1Marker + "\n\nAdd your heartbeat tasks below this line:\n"
	path := filepath.Join(f.workspace, "HEARTBEAT.md")
	if err := os.WriteFile(path, []byte(v1), 0o644); err != nil {
		t.Fatal(err)
	}
	f.build(f.initial)

	if n := strings.Count(logs.String(), phrase); n != 1 {
		t.Fatalf("%d warnings at start, want 1\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), "still works") {
		t.Errorf("the warning does not say the heartbeat still works:\n%s", logs.String())
	}

	if err := f.reload(governedLive); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(logs.String(), phrase); n != 2 {
		t.Fatalf("%d warnings after one reload, want 2", n)
	}
	if got, _ := os.ReadFile(path); string(got) != v1 {
		t.Fatal("the user's HEARTBEAT.md was rewritten")
	}

	// Legacy with a v1 file: nothing to say.
	if err := f.reload(compose(withOwner("1001"), withHeartbeat(config.HeartbeatModeLegacy))); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(logs.String(), phrase); n != 2 {
		t.Fatalf("legacy + v1 file warned (%d total)", n)
	}
}

// A v2 file under a legacy heartbeat: the model would be told to call a tool
// that does not exist, so say so once and leave the file alone.
func TestHeartbeatAssembly_V2TemplateUnderLegacyWarns(t *testing.T) {
	logs := captureLogs(t)
	f := newAssemblyFixtureNoBuild(t, withHeartbeat(config.HeartbeatModeLegacy))
	idleV2 := heartbeat.TemplateV2()
	idleV2 = idleV2[:strings.Index(idleV2, "Add your heartbeat tasks below this line:")] +
		"Add your heartbeat tasks below this line:\n"
	path := filepath.Join(f.workspace, "HEARTBEAT.md")
	if err := os.WriteFile(path, []byte(idleV2), 0o644); err != nil {
		t.Fatal(err)
	}
	f.build(f.initial)

	out := logs.String()
	if n := strings.Count(out, "HEARTBEAT.md is the v2 template"); n != 1 {
		t.Fatalf("%d warnings, want 1\n%s", n, out)
	}
	if !strings.Contains(out, "HEARTBEAT.md.v1.bak") {
		t.Errorf("the warning does not give the fix:\n%s", out)
	}
	if got, _ := os.ReadFile(path); string(got) != idleV2 {
		t.Fatal("the user's HEARTBEAT.md was rewritten")
	}
}

// stopServices stops the heartbeat before the outbox (a governed run's decisions
// need somewhere to go) and the outbox before the notify service.
func TestStopServices_HeartbeatStopsBeforeTheOutbox(t *testing.T) {
	ws := t.TempDir()
	hs := heartbeat.NewHeartbeatService(ws, 30, true, nil)
	hs.SetContract(heartbeat.ContractGoverned)
	if err := hs.Start(); err != nil {
		t.Fatal(err)
	}
	defer hs.Stop()

	var heartbeatRunningAtOutboxStop atomic.Bool
	done := make(chan struct{})
	runner := &attentionRunner{
		cancel: func() {
			heartbeatRunningAtOutboxStop.Store(hs.IsRunning())
			close(done)
		},
		done: done,
	}
	ob := attention.NewOutbox(&attentiontest.Fake{}, &attentiontest.Deliverer{}, 0, nil)
	s := &gatewayServices{HeartbeatService: hs, Attention: ob, attentionRun: runner}

	stopServices(s, 5*time.Second)

	if heartbeatRunningAtOutboxStop.Load() {
		t.Fatal("the outbox was stopped while the heartbeat was still running")
	}
	if hs.IsRunning() {
		t.Fatal("stopServices left the heartbeat running")
	}
	if s.Attention != nil || s.attentionRun != nil {
		t.Fatal("stopServices left the outbox set")
	}
}

func withHeartbeatTools(names ...string) func(*config.Config) {
	return func(c *config.Config) { c.Proactive.Heartbeat.Tools = names }
}

// recordGovernedTools swaps the handler factory for one that records the tool
// list the assembly gave it, and returns the lists seen so far.
func recordGovernedTools(t *testing.T) func() [][]string {
	t.Helper()
	var mu sync.Mutex
	var seen [][]string
	orig := newGovernedHandler
	newGovernedHandler = func(
		run proactiveRunner, out attentionSubmitter, interval time.Duration, tier proactive.ModelTier,
		toolNames []string,
	) heartbeat.ContextHandler {
		mu.Lock()
		seen = append(seen, append([]string(nil), toolNames...))
		mu.Unlock()
		return orig(run, out, interval, tier, toolNames)
	}
	t.Cleanup(func() { newGovernedHandler = orig })
	return func() [][]string {
		mu.Lock()
		defer mu.Unlock()
		return append([][]string(nil), seen...)
	}
}

// The governed handler is built with proactive.heartbeat.tools as Effective
// cleans it (empty by default: the turn's own default subset), and a reload that
// changes the list builds a handler with the new one.
func TestHeartbeatAssembly_ToolsFollowConfigAndReload(t *testing.T) {
	seen := recordGovernedTools(t)
	governed := func(names ...string) func(*config.Config) {
		return compose(withOwner("1001"), withAttentionMode(config.ProactiveModeLive),
			withHeartbeat(config.HeartbeatModeGoverned), withHeartbeatTools(names...))
	}
	want := func(label string, wantLists ...[]string) {
		t.Helper()
		got := seen()
		if fmt.Sprintf("%q", got) != fmt.Sprintf("%q", wantLists) {
			t.Fatalf("%s: handler built with tool lists %q, want %q", label, got, wantLists)
		}
	}

	f := newAssemblyFixture(t, governed())
	want("default", []string{})

	if err := f.reload(governed(" get_ticker ", "get_ticker", "web_search")); err != nil {
		t.Fatal(err)
	}
	want("reload to an explicit list (trimmed, de-duplicated)", []string{}, []string{"get_ticker", "web_search"})

	if err := f.reload(governed("*")); err != nil {
		t.Fatal(err)
	}
	want("reload to all", []string{}, []string{"get_ticker", "web_search"}, []string{"*"})

	if err := f.reload(governed()); err != nil {
		t.Fatal(err)
	}
	want("reload back to the default", []string{}, []string{"get_ticker", "web_search"}, []string{"*"}, []string{})
}
