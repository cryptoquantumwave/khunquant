package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// checkGolden compares got with testdata/<name>; UPDATE_GOLDEN=1 rewrites it, for a
// deliberate wording change only.
func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if os.Getenv("UPDATE_GOLDEN") != "" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if got != string(want) {
		t.Errorf("dry-run output changed.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// The dry-run reviews of unwind and resize are pinned byte for byte: moving the
// confirmed path onto the journal must not change what a preview says.
func TestUnwindDeltaNeutralPosition_DryRunGolden(t *testing.T) {
	store := setupTempDeltaNeutralStore(t)
	defer store.Close()
	installDNFakes(t, &dnFakeSpot{id: "dnfake", baseFree: 10}, &dnFakeFutures{mockFuturesProvider: &mockFuturesProvider{}})
	id := seedDNFakePlan(t, store, "unwind-golden", "active")
	seedDNOpenLegs(t, store, id, "both_legs_filled", 10, 10)

	res := NewUnwindDeltaNeutralPositionTool(leverageOnCfg(), store).
		Execute(context.Background(), map[string]any{"plan_id": float64(id), "confirm": false})
	if res.IsError {
		t.Fatalf("dry-run errored: %s", res.ForLLM)
	}
	checkGolden(t, "dn_unwind_dryrun.golden", res.ForUser)
}

func TestResizeDeltaNeutralPosition_DryRunGolden(t *testing.T) {
	for name, args := range map[string]map[string]any{
		"up":       {"delta_notional_usdt": 500.0},
		"down":     {"delta_notional_usdt": -500.0},
		"down_pct": {"delta_pct": -10.0},
	} {
		t.Run(name, func(t *testing.T) {
			store := setupTempDeltaNeutralStore(t)
			defer store.Close()
			installDNFakes(t, &dnFakeSpot{id: "dnfake", baseFree: 10}, &dnFakeFutures{mockFuturesProvider: &mockFuturesProvider{}})
			id := seedDNFakePlan(t, store, "resize-golden", "active")
			args["plan_id"], args["confirm"] = float64(id), false
			res := NewResizeDeltaNeutralPositionTool(leverageOnCfg(), store).Execute(context.Background(), args)
			if res.IsError {
				t.Fatalf("dry-run errored: %s", res.ForLLM)
			}
			checkGolden(t, "dn_resize_"+name+"_dryrun.golden", res.ForUser)
		})
	}
}
