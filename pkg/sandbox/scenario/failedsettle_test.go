package scenario

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labjournal"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/supervisor"
)

// A settle an advance's own wall budget cut is not a failed settle; one that hit its wall
// cap is, and is named.
func TestFailedSettle_IgnoresACutSettleButNeverASwallowedFailure(t *testing.T) {
	sup := newFakeSup(t)
	info, err := sup.Up(context.Background(), supervisor.UpOptions{ClockStart: "2026-10-20T09:00:00+07:00", ClockMode: "manual"})
	if err != nil {
		t.Fatal(err)
	}
	in, _ := sup.get(info.Name)
	c, err := sup.Client(info.Name)
	if err != nil {
		t.Fatal(err)
	}
	add := func(p map[string]any) {
		b, _ := json.Marshal(p)
		in.j.Append(labjournal.Entry{Kind: "settle", Source: "quiesce", Payload: b})
		if err := in.j.Flush(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()

	add(map[string]any{"ok": false, "cut": true, "wall_ms": 1, "polls": 5, "busy": []string{"journal"}})
	if se, err := failedSettle(ctx, c, 0); err != nil || se != nil {
		t.Fatalf("a cut settle: %v, %v; want no failure", se, err)
	}
	add(map[string]any{"ok": true, "cut": false, "wall_ms": 0, "polls": 2, "busy": []string{}})
	if se, err := failedSettle(ctx, c, 0); err != nil || se != nil {
		t.Fatalf("a cut and a passed settle: %v, %v; want no failure", se, err)
	}
	add(map[string]any{"ok": false, "cut": false, "wall_ms": 10000, "polls": 900, "busy": []string{"approval"}})
	se, err := failedSettle(ctx, c, 0)
	if err != nil || se == nil || len(se.Busy) != 1 || se.Busy[0] != "approval" {
		t.Fatalf("a settle that hit its cap: %v, %v; want it reported naming approval", se, err)
	}
	if want := "1 settle(s) failed"; se.Detail != want {
		t.Errorf("detail = %q, want %q (the cut one is not counted)", se.Detail, want)
	}
}
