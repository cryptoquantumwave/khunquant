package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/heartbeat"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/perceptiontest"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive/sweep"
	"github.com/cryptoquantumwave/khunquant/pkg/worldmodel"
)

// sweepHeadroom is the spare room the sweep's first call must keep under the
// governed heartbeat's budget.
const sweepHeadroom = 1_500

// busyWorld is a real world model with both marks (the sweep's and the changes
// section's) set before a busy half hour: 20 holdings vanish and 20 appear, and the
// portfolio total, a class total, a futures position, a source and a plan move. It
// holds far more changes than the sweep prompt shows, so the cap is what binds.
func busyWorld(t *testing.T) *worldmodel.Store {
	t.Helper()
	clk := perceptiontest.NewClock(time.Date(2026, 3, 10, 5, 0, 0, 0, time.UTC))
	store, err := worldmodel.NewStore(context.Background(), nil, clk, worldmodel.WithZone(func() *time.Location { return time.UTC }))
	if err != nil {
		t.Fatal(err)
	}
	snap := func(id, first int, scale float64) events.Event {
		var ps []worldmodel.PortfolioPositionPayload
		total := 0.0
		for i := first; i < first+40; i++ {
			v := 1000 * scale
			ps = append(ps, worldmodel.PortfolioPositionPayload{
				Asset: fmt.Sprintf("LONGNAMEDCOIN%02d-WITH-A-LONG-TAIL-XYZ", i), Source: "binance", Class: "crypto", Value: v, Qty: 10 * scale,
			})
			total += v
		}
		return events.Event{
			ID: fmt.Sprintf("s%d", id), Kind: events.KindPerceptionPortfolioSnapshot, Time: clk.Now(),
			Source: events.Source{Component: "perception", Name: "portfolio"},
			Payload: worldmodel.PortfolioPayload{
				SnapshotID: int64(id), TakenAt: clk.Now(), Quote: "USDT", TotalValueUSD: total,
				ByClass: map[string]float64{"crypto": total}, Positions: ps,
			},
			Trust: events.TrustPtr(proactive.TrustExchange),
		}
	}
	ctx := context.Background()
	if _, err := store.Apply(ctx, snap(1, 0, 1)); err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{sweep.Mark, worldmodel.MarkProactive} {
		if err := store.SetMark(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	clk.Advance(30 * time.Minute)
	if _, err := store.Apply(ctx, snap(2, 20, 1.2)); err != nil {
		t.Fatal(err)
	}
	return store
}

func atCapHandledTopics() []sweep.Topic {
	var out []sweep.Topic
	kinds := []string{"move_vs_vol", "drawdown", "liquidation_distance", "allocation_drift", "data_stale", "price_cross"}
	for i := range sweep.MaxHandledTopics {
		k := kinds[i%len(kinds)]
		out = append(out, sweep.Topic{
			Key: fmt.Sprintf("%s:LONGASSETNAME%d/USDT:USDT:tier%d", k, i, i%3), Kind: k, Asset: fmt.Sprintf("LONGASSETNAME%d", i),
			Severity: proactive.SeverityWarning, At: time.Now(),
		})
	}
	return out
}

// firstCallOfSweep runs a sweep-shaped turn on the default workspace with the real
// changes contributor attached to the loop, and measures its first provider call.
func firstCallOfSweep(t *testing.T, heartbeatFile string, callerSupplies bool, fixtures ...budgetFixture) (firstCall, *worldmodel.Store, *AgentLoop) {
	t.Helper()
	store := busyWorld(t)
	diff, err := store.SinceMark(context.Background(), sweep.Mark)
	if err != nil {
		t.Fatal(err)
	}
	changes := worldmodel.RenderChanges(diff, sweep.MaxChangeItems, sweep.MaxChangeChars)
	// The diff holds far more than the prompt shows: its caps (items or characters)
	// are what bind, and the section is as full as it can be.
	if n := strings.Count(changes, "\n- "); n < 10 || n > sweep.MaxChangeItems || n >= len(diff.Changes)+len(diff.Signals) {
		t.Fatalf("the prompt shows %d of %d changes: the test must be at the cap", n, len(diff.Changes)+len(diff.Signals))
	}
	hbPrompt := heartbeat.GovernedPrompt(time.Now().Format("2006-01-02 15:04:05"), heartbeatFile)
	prompt := sweep.BuildPrompt(changes, atCapHandledTopics(), hbPrompt)
	t.Logf("sweep prompt parts: changes %d bytes, handled list %d bytes, framing %d bytes, heartbeat prompt %d bytes",
		len(changes), len(sweep.RenderHandled(atCapHandledTopics())),
		len(prompt)-len(changes)-len(sweep.RenderHandled(atCapHandledTopics()))-len(hbPrompt), len(hbPrompt))

	prov := script(respondResponse(map[string]any{"decision": "silent"}))
	al, _ := defaultWorkspaceLoopWith(t, prov, fixtures...)
	registerGatewayReadTools(al)
	detach := al.SetBackgroundSection(worldmodel.NewChangesContributor(store))
	t.Cleanup(detach)
	mustRun(t, al, ProactiveTurnRequest{
		Kind: proactive.TurnHeartbeatLegacy, Prompt: prompt, Deadline: 20 * time.Second,
		CallerSuppliesChanges: callerSupplies,
	})
	return measureFirstCall(t, prov), store, al
}

// A sweep's first call, on the runtime path (the real changes contributor attached
// to the loop, a world model with more changes than fit, the handled list at its cap),
// stays inside the governed heartbeat's 40,000-byte budget with real headroom when
// HEARTBEAT.md is the v2 template, which is the file a sweep (like a governed
// heartbeat) is meant to run on.
func TestProactiveTurn_SweepHeartbeatFirstCallWithinBudget(t *testing.T) {
	fc, _, _ := firstCallOfSweep(t, heartbeat.TemplateV2(), true)
	t.Logf("sweep first call: %d bytes (budget %d, headroom %d)\n%s",
		fc.total, governedHeartbeatFirstCallBudget, governedHeartbeatFirstCallBudget-fc.total, fc)
	if fc.total > governedHeartbeatFirstCallBudget-sweepHeadroom {
		t.Errorf("a sweep's first call is %d bytes; budget %d with %d bytes of headroom required\n%s",
			fc.total, governedHeartbeatFirstCallBudget, sweepHeadroom, fc)
	}
}

// The default workspace's own HEARTBEAT.md is the v2 template (about 5 KB; an install
// that still has the v1 file has one of about 9 KB, which is already near the budget).
// What the sweep adds on top of whatever file it runs on is bounded by its caps, and
// that is what is checked here.
func TestProactiveTurn_SweepAddsABoundedAmountToAnyHeartbeatFile(t *testing.T) {
	_, ws := defaultWorkspaceLoop(t, script())
	v1, err := os.ReadFile(filepath.Join(ws, "HEARTBEAT.md"))
	if err != nil {
		t.Fatal(err)
	}
	prov := script(respondResponse(map[string]any{"decision": "silent"}))
	al, _ := defaultWorkspaceLoop(t, prov)
	registerGatewayReadTools(al)
	mustRun(t, al, ProactiveTurnRequest{
		Kind: proactive.TurnHeartbeatLegacy, Deadline: 20 * time.Second,
		Prompt: heartbeat.GovernedPrompt(time.Now().Format("2006-01-02 15:04:05"), string(v1)),
	})
	governed := measureFirstCall(t, prov)

	sweepFC, _, _ := firstCallOfSweep(t, string(v1), true)
	added := sweepFC.total - governed.total
	t.Logf("default workspace HEARTBEAT.md (%d bytes): governed first call %d, sweep %d, the sweep adds %d bytes",
		len(v1), governed.total, sweepFC.total, added)
	if added > 3_000 {
		t.Errorf("the sweep adds %d bytes to a governed heartbeat's first call, want at most 3000", added)
	}
}

// The model reads the changes once. Without the seam the loop's own section would be
// in the same call as well: that is what it replaces, and what it costs.
func TestProactiveTurn_SweepCarriesTheChangesOnce(t *testing.T) {
	count := func(callerSupplies bool) (int, int) {
		fc, _, _ := firstCallOfSweep(t, heartbeat.TemplateV2(), callerSupplies)
		return strings.Count(fc.String(), "x"), fc.total
	}
	_, withSeam := count(true)
	_, without := count(false)
	t.Logf("first call with the seam %d bytes, without %d bytes", withSeam, without)
	if without-withSeam < 800 {
		t.Errorf("the loop's section adds only %d bytes without the seam: the test does not show what it removes", without-withSeam)
	}

	prov := script(respondResponse(map[string]any{"decision": "silent"}))
	store := busyWorld(t)
	al, _ := defaultWorkspaceLoop(t, prov)
	al.SetBackgroundSection(worldmodel.NewChangesContributor(store))
	mustRun(t, al, ProactiveTurnRequest{Kind: proactive.TurnHeartbeatLegacy, Prompt: "p", CallerSuppliesChanges: true})
	prov.mu.Lock()
	defer prov.mu.Unlock()
	for _, m := range prov.msgs[0] {
		if strings.Contains(m.Content, "Changes since your last look") {
			t.Fatal("a turn whose caller supplies the changes carried the loop's section")
		}
	}
}

// What happens to MarkProactive when a sweep turn completes: nothing. The section was
// not rendered, so there is nothing to move, and the next turn that does carry the
// section still sees the changes since ITS last look. A turn that does not set the
// seam moves it, as before.
func TestProactiveTurn_SweepTurnLeavesTheChangesMarkWhereItWas(t *testing.T) {
	run := func(callerSupplies bool) (before, after int64, section string) {
		store := busyWorld(t)
		prov := script(respondResponse(map[string]any{"decision": "silent"}))
		al, _ := defaultWorkspaceLoop(t, prov)
		al.SetBackgroundSection(worldmodel.NewChangesContributor(store))
		before, _, _ = store.MarkInfo(worldmodel.MarkProactive)
		mustRun(t, al, ProactiveTurnRequest{
			Kind: proactive.TurnHeartbeatLegacy, Prompt: "p", CallerSuppliesChanges: callerSupplies,
		})
		after, _, _ = store.MarkInfo(worldmodel.MarkProactive)
		section, _ = worldmodel.NewChangesContributor(store).Section(context.Background())
		return before, after, section
	}

	before, after, section := run(true)
	if after != before {
		t.Fatalf("a sweep turn moved MarkProactive from %d to %d", before, after)
	}
	if !strings.Contains(section, "Changes since your last look") {
		t.Fatalf("the next candidate-driven turn would see no changes:\n%q", section)
	}

	// Control: the same turn without the seam shows the section and moves the mark.
	before, after, _ = run(false)
	if after <= before {
		t.Fatalf("a turn that carries the section left MarkProactive at %d (was %d): the control proves nothing", after, before)
	}
}
