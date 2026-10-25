package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex"
)

func proactiveDBClosers(f *assemblyFixture) int {
	f.shared.mu.Lock()
	defer f.shared.mu.Unlock()
	n := 0
	for _, c := range f.shared.closers {
		if c.name == proactiveDBResource {
			n++
		}
	}
	return n
}

// The shared proactive database is opened once and closed once: a reload keeps
// the very same handle open (the profile tools hold it), and gateway shutdown,
// through sharedResources, is what closes it.
func TestReload_ProactiveDBHandleSurvives(t *testing.T) {
	f := newAssemblyFixture(t, nil)
	ctx := context.Background()

	db, err := sqlitex.Shared(f.workspace)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.PingContext(ctx); err != nil {
		t.Fatal(err)
	}
	if got := proactiveDBClosers(f); got != 1 {
		t.Fatalf("closer registered %d times after the first build, want 1", got)
	}

	for i := 0; i < 2; i++ {
		if err := f.reload(nil); err != nil {
			t.Fatalf("reload %d: %v", i+1, err)
		}
		again, err := sqlitex.Shared(f.workspace)
		if err != nil {
			t.Fatal(err)
		}
		if again != db {
			t.Fatalf("reload %d reopened the proactive database", i+1)
		}
		if err := db.PingContext(ctx); err != nil {
			t.Fatalf("reload %d closed the proactive database: %v", i+1, err)
		}
	}
	if got := proactiveDBClosers(f); got != 1 {
		t.Fatalf("closer registered %d times after reloads, want 1", got)
	}

	// The profile tools built by the agent loop work on that same handle.
	if _, ok := f.al.GetRegistry().GetDefaultAgent().Tools.Get("update_profile"); !ok {
		t.Fatal("update_profile is not registered in the gateway")
	}

	// Shutdown closes it.
	stopServices(f.services, 5*time.Second)
	f.services = nil
	if err := f.shared.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.PingContext(ctx); err == nil {
		t.Fatal("gateway shutdown left the proactive database open")
	}
}
