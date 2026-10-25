package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

// The owner's dial through the real assembly and tool funnel: in live gate C enforces
// it, in shadow it is recorded and nothing is refused, with the policy off it does
// nothing. The owner's own order is never dialled.
func TestOwnerDial_ShadowRecordsLiveEnforcesOffDoesNothing(t *testing.T) {
	t.Cleanup(func() { tools.SetPolicyShadow(nil); tools.SetPolicyLive(nil) })
	f := newAssemblyFixture(t, withPolicy(config.ProactiveModeLive, true))
	db, err := sqlitex.Shared(f.workspace)
	if err != nil {
		t.Fatal(err)
	}
	store, err := intent.NewStore(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	set := func(dial map[string]string) {
		t.Helper()
		if _, err := store.SetAutonomy(context.Background(), dial, "web"); err != nil {
			t.Fatal(err)
		}
	}
	denied := func() int {
		t.Helper()
		if !f.services.Policy.rec.Flush(5 * time.Second) {
			t.Fatal("the recorder did not drain")
		}
		return f.services.Policy.stats(time.Now()).DecisionsToday["deny"]
	}

	// Another person's order is notify_only today (it can only inform the owner).
	// With T3 off it is denied: in the record and, in live, in effect.
	set(map[string]string{"T3": "off"})
	if ran, text := tradeAs(f, "telegram:999"); ran {
		t.Fatalf("live: another person's order ran with T3 off: %q", text)
	}
	if n := denied(); n != 1 {
		t.Fatalf("live: %d deny verdicts recorded, want 1", n)
	}
	if ran, text := tradeAs(f, "telegram:1001"); !ran {
		t.Fatalf("live: the owner's own order was dialled: %q", text)
	}

	// Shadow: recorded, nothing refused.
	if err := f.reload(withPolicy(config.ProactiveModeShadow, true)); err != nil {
		t.Fatal(err)
	}
	if ran, text := tradeAs(f, "telegram:999"); !ran {
		t.Fatalf("shadow enforced the dial: %q", text)
	}
	if n := denied(); n != 2 {
		t.Fatalf("shadow: %d deny verdicts recorded today, want 2", n)
	}

	// Off: nothing is armed and nothing is refused.
	if err := f.reload(withPolicy(config.ProactiveModeOff, true)); err != nil {
		t.Fatal(err)
	}
	if f.services.Policy != nil {
		t.Fatal("a policy runtime survived mode off")
	}
	if ran, text := tradeAs(f, "telegram:999"); !ran {
		t.Fatalf("mode off enforced the dial: %q", text)
	}
}
