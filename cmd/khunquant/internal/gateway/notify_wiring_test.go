package gateway

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/agent"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/notify"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex"
)

func withOwner(chatID string) func(*config.Config) {
	return func(cfg *config.Config) {
		cfg.Proactive.Enabled = true
		cfg.Proactive.Owner = config.OwnerConfig{
			Channel: "telegram", ChatID: chatID, SenderID: "telegram:" + chatID,
		}
	}
}

func countShared(r *sharedResources, name string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.closers {
		if c.name == name {
			n++
		}
	}
	return n
}

func TestNotify_RebuiltOnReloadWithChangedOwnerAndClosedOnStop(t *testing.T) {
	f := newAssemblyFixture(t, withOwner("1001"))

	first := f.services.NotifyService
	if first == nil {
		t.Fatal("no notify service although proactive is enabled with an owner")
	}
	if r, ok := first.OwnerRoute(); !ok || r.ChatID != "1001" {
		t.Fatalf("route = %+v, %v", r, ok)
	}

	if err := f.reload(withOwner("2002")); err != nil {
		t.Fatalf("reload: %v", err)
	}
	second := f.services.NotifyService
	if second == nil || second == first {
		t.Fatalf("notify service not rebuilt on reload (first=%p second=%p)", first, second)
	}
	if r, ok := second.OwnerRoute(); !ok || r.ChatID != "2002" {
		t.Fatalf("after reload route = %+v, %v", r, ok)
	}
	// The old generation was closed: it refuses new work.
	res, err := first.Deliver(context.Background(), notify.Notification{Title: "late"})
	if err != nil || res.Delivered || res.Reason != notify.ReasonClosed {
		t.Fatalf("old service after reload = %+v, %v", res, err)
	}

	// One shared database registration across both generations.
	if n := countShared(f.shared, proactiveDBResource); n != 1 {
		t.Fatalf("shared database registered %d times, want 1", n)
	}

	// Stop closes the current one and clears the field.
	stopServices(f.services, 5*time.Second)
	if f.services.NotifyService != nil {
		t.Fatal("NotifyService not cleared by stopServices")
	}
	res, _ = second.Deliver(context.Background(), notify.Notification{Title: "late"})
	if res.Reason != notify.ReasonClosed {
		t.Fatalf("service after stop = %+v", res)
	}
	f.services = nil // already stopped; the cleanup must not stop it twice
}

func TestNotify_AbsentWhenProactiveOff(t *testing.T) {
	f := newAssemblyFixture(t, nil)
	if f.services.NotifyService != nil {
		t.Fatal("notify service built while proactive is off")
	}
	// A nil service is a disabled one.
	res, err := f.services.NotifyService.Deliver(context.Background(), notify.Notification{Title: "x"})
	if err != nil || res.Delivered || res.Reason != notify.ReasonDisabled {
		t.Fatalf("nil service Deliver = %+v, %v", res, err)
	}
}

func TestNotify_DatabaseFailureDoesNotStopGateway(t *testing.T) {
	f := newAssemblyFixtureNoBuild(t, withOwner("1001"))
	// The agent loop already opened the shared database. Close it, then put a
	// directory where the file must be, so the gateway's own open fails.
	if err := sqlitex.CloseShared(); err != nil {
		t.Fatal(err)
	}
	dbDir := filepath.Join(agent.DefaultAgentWorkspace(f.initial), "memory", "proactive")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(filepath.Join(dbDir, "proactive.db"+suffix))
	}
	if err := os.MkdirAll(filepath.Join(dbDir, "proactive.db"), 0o700); err != nil {
		t.Fatal(err)
	}
	f.build(f.initial) // fatals if the gateway fails to start
	if f.services.NotifyService != nil {
		t.Fatal("notify service built despite the database failure")
	}
}

func TestNotify_DeviceNotifierRoutesThroughService(t *testing.T) {
	f := newAssemblyFixture(t, withOwner("1001"))
	n := deviceNotifier{svc: f.services.NotifyService}
	if !n.CanDeliver() {
		t.Fatal("device notifier cannot deliver with an owner configured")
	}
}
