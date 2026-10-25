package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/cron"
	"github.com/cryptoquantumwave/khunquant/pkg/triggers"
)

func TestLiveCreateResultSaysAnAlertYouSetBreaksQuietHours(t *testing.T) {
	reg := &fakeAlertTriggers{live: true}
	for name, tool := range map[string]interface {
		Execute(ctx context.Context, args map[string]any) *ToolResult
	}{
		"price":     NewSetPriceAlertTool(config.DefaultConfig(), newTestCronServiceForAlerts(t)).WithTriggers(resolver(reg)),
		"indicator": NewSetIndicatorAlertTool(config.DefaultConfig(), newTestCronServiceForAlerts(t)).WithTriggers(resolver(reg)),
	} {
		args := priceArgs(nil)
		if name == "indicator" {
			args = map[string]any{"action": "create", "provider": "okx", "symbol": "BTC/USDT", "indicator": "RSI", "condition": "above", "threshold": 70.0}
		}
		res := tool.Execute(groupCtx, args)
		for _, out := range []string{res.ForLLM, res.ForUser} {
			if !strings.Contains(out, "Quiet hours") || !strings.Contains(out, "sent at once, even in your quiet hours") ||
				strings.Contains(out, "daily digest") {
				t.Fatalf("%s: the result does not say that the alert is sent in quiet hours:\n%s", name, out)
			}
		}
	}
}

func TestList_ShowsInactiveDoneAndHidesSpecsWithAStandIn(t *testing.T) {
	reg := &fakeAlertTriggers{live: false}
	cronSvc := newTestCronServiceForAlerts(t)
	tool := NewSetPriceAlertTool(config.DefaultConfig(), cronSvc).WithTriggers(resolver(reg))
	p := json.RawMessage(`{"provider":"okx","symbol":"BTC/USDT","direction":"above","threshold":1}`)
	reg.specs = []triggers.Spec{
		{ID: "no-owner", Kind: triggers.KindPriceCross, Mode: triggers.ModeLive, Origin: "tool:set_price_alert", Params: p, CreatedAt: time.Unix(1, 0)},
		{ID: "has-standin", Kind: triggers.KindPriceCross, Mode: triggers.ModeOff, Origin: "tool:set_price_alert", Params: p, CreatedAt: time.Unix(2, 0)},
		{ID: "fired", Kind: triggers.KindPriceCross, Mode: triggers.ModeOff, Origin: "tool:set_price_alert", Params: p, CreatedAt: time.Unix(3, 0)},
	}
	j, err := cronSvc.AddStandInJob("price_alert:okx:BTC/USDT:above:1", cron.CronSchedule{Kind: "cron", Expr: "* * * * *"}, "{}", "telegram", "1001", "has-standin")
	if err != nil {
		t.Fatal(err)
	}

	out := tool.Execute(groupCtx, map[string]any{"action": "list"}).ForLLM
	if !strings.Contains(out, j.ID) {
		t.Fatalf("the stand-in job is not listed as a cron alert:\n%s", out)
	}
	if strings.Contains(out, "has-standin") {
		t.Fatalf("a spec with a stand-in is listed twice:\n%s", out)
	}
	for _, want := range []string{"no-owner", "inactive", "fired", "done", "Inactive alerts are not being checked"} {
		if !strings.Contains(out, want) {
			t.Fatalf("list lacks %q:\n%s", want, out)
		}
	}

	// Live: the same spec is simply on.
	reg.live = true
	out = tool.Execute(groupCtx, map[string]any{"action": "list"}).ForLLM
	if strings.Contains(out, "inactive") || strings.Contains(out, "Inactive alerts") {
		t.Fatalf("a live registry still lists inactive alerts:\n%s", out)
	}
}

func TestCancel_AStandInJobCancelsItsSpecToo(t *testing.T) {
	reg := &fakeAlertTriggers{live: false}
	cronSvc := newTestCronServiceForAlerts(t)
	tool := NewSetPriceAlertTool(config.DefaultConfig(), cronSvc).WithTriggers(resolver(reg))
	reg.specs = []triggers.Spec{{ID: "s1", Kind: triggers.KindPriceCross, Mode: triggers.ModeOff, Origin: "tool:set_price_alert"}}
	j, err := cronSvc.AddStandInJob("price_alert:x", cron.CronSchedule{Kind: "cron", Expr: "* * * * *"}, "{}", "telegram", "1001", "s1")
	if err != nil {
		t.Fatal(err)
	}
	if res := tool.Execute(groupCtx, map[string]any{"action": "cancel", "alert_id": j.ID}); res.IsError {
		t.Fatal(res.ForLLM)
	}
	if len(reg.specs) != 0 || cronSvc.GetJob(j.ID) != nil {
		t.Fatalf("specs %d, job %v: cancelling the stand-in must leave neither", len(reg.specs), cronSvc.GetJob(j.ID))
	}
}
