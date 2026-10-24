package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestOpenDeltaNeutralPosition_DryRunGolden pins the dry-run review byte for byte:
// moving the confirmed path onto the journal must not change what a preview says.
// Regenerate with UPDATE_GOLDEN=1 only for a deliberate wording change.
func TestOpenDeltaNeutralPosition_DryRunGolden(t *testing.T) {
	store := setupTempDeltaNeutralStore(t)
	defer store.Close()
	installDNFakes(t, &dnFakeSpot{id: "dnfake", baseFree: 10}, &dnFakeFutures{mockFuturesProvider: &mockFuturesProvider{}})
	id := seedDNFakePlan(t, store, "dry-golden", "ready")

	res := NewOpenDeltaNeutralPositionTool(leverageOnCfg(), store).
		Execute(context.Background(), map[string]any{"plan_id": float64(id), "confirm": false})
	if res.IsError {
		t.Fatalf("dry-run errored: %s", res.ForLLM)
	}
	path := filepath.Join("testdata", "dn_open_dryrun.golden")
	if os.Getenv("UPDATE_GOLDEN") != "" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(res.ForUser), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if res.ForUser != string(want) {
		t.Errorf("dry-run output changed.\n--- got ---\n%s\n--- want ---\n%s", res.ForUser, want)
	}
}
