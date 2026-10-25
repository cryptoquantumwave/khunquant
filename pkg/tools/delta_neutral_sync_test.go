package tools

import (
	"context"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral"
)

func createArgs() map[string]any {
	return map[string]any{
		"plan_name": "BTC Funding Harvest", "asset": "BTC",
		"spot_provider": "binance", "spot_account": "my_spot", "spot_symbol": "BTC/USDT",
		"futures_provider": "binance", "futures_account": "my_futures", "futures_symbol": "BTC/USDT:USDT",
		"capital_usdt": 10000.0, "leverage": 5.0, "monitor_interval": "5m",
	}
}

// Creating a plan makes its cron job: the hook is called once, after it exists; a nil
// hook changes nothing about the tool.
func TestCreateDeltaNeutralPlanTool_CallsTheSyncHookOnceAfterTheJobExists(t *testing.T) {
	store, _ := deltaneutral.NewStore(t.TempDir())
	cs := newTestCronServiceForDN(t)
	jobsAtCall, calls := -1, 0
	tool := NewCreateDeltaNeutralPlanTool(&config.Config{}, store, cs).WithSync(func() {
		calls++
		jobsAtCall = len(cs.ListJobs(true))
	})
	if res := tool.Execute(context.Background(), createArgs()); res.IsError {
		t.Fatalf("create: %s", res.ForLLM)
	}
	if calls != 1 || jobsAtCall != 1 {
		t.Fatalf("hook calls = %d, jobs at the call = %d, want 1 and 1", calls, jobsAtCall)
	}

	// A refused create does not call it.
	bad := createArgs()
	bad["monitor_interval"] = "2m30s"
	if res := tool.Execute(context.Background(), bad); !res.IsError || calls != 1 {
		t.Fatalf("refused create: error=%v calls=%d", res.IsError, calls)
	}

	// No hook: same behaviour, no panic.
	store2, _ := deltaneutral.NewStore(t.TempDir())
	cs2 := newTestCronServiceForDN(t)
	if res := NewCreateDeltaNeutralPlanTool(&config.Config{}, store2, cs2).Execute(context.Background(), createArgs()); res.IsError {
		t.Fatalf("create without a hook: %s", res.ForLLM)
	}
	if len(cs2.ListJobs(true)) != 1 {
		t.Fatal("no job without a hook")
	}
}
