package gateway

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/attention"
	"github.com/cryptoquantumwave/khunquant/pkg/attention/attentiontest"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/heartbeat"
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
