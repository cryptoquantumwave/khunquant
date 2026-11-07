package gateway

import (
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

func mandatesOn(cfg *config.Config) {
	cfg.Proactive.Enabled = true
	cfg.Proactive.Owner = config.OwnerConfig{Channel: "telegram", ChatID: "1001", SenderID: "telegram:1001"}
	cfg.Proactive.Approvals = config.ApprovalsConfig{Enabled: true, ExpiryMinutes: 10}
	cfg.Proactive.Mandates.Enabled = true
}

// A mandate run reads its order back at once in a locked lab (the sim clock cannot serve a
// wait), and an ordinary gateway keeps the workflow's own default wait.
func TestDCAFlow_PollIntervalOnlyChangesInALockedLab(t *testing.T) {
	plain := newAssemblyFixture(t, mandatesOn)
	if plain.services.DCAFlow == nil {
		t.Fatal("the unlocked fixture built no DCA flow, so there is nothing to check")
	}
	if got := plain.services.DCAFlow.deps.PollInterval; got != 0 {
		t.Errorf("unlocked gateway: PollInterval = %v, want 0 (the workflow's default wait)", got)
	}

	locked := newLockedFixture(t, mandatesOn)
	if locked.services.DCAFlow == nil {
		t.Fatal("the locked fixture built no DCA flow, so there is nothing to check")
	}
	if got := locked.services.DCAFlow.deps.PollInterval; got >= 0 {
		t.Errorf("locked lab: PollInterval = %v, want negative (no wait)", got)
	}
}
