package policy

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/perceptiontest"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex/sqlitextest"
)

// pkg/tools calls this package, and pkg/approval will, so it may import neither
// (nor the agent, the channels or the notify service). It reads the broker for the
// order gates and the shared database for its own table, nothing else.
func TestPolicy_ImportBoundary(t *testing.T) {
	perceptiontest.AssertImports(t, perceptiontest.Boundary{
		Root: "pkg/policy",
		Allowed: []string{
			"pkg/config", "pkg/events", "pkg/logger", "pkg/proactive", "pkg/providers/broker", "pkg/sqlitex",
		},
		Forbidden: []string{
			"pkg/agent", "pkg/tools", "pkg/approval", "pkg/journal", "pkg/notify", "pkg/channels", "pkg/attention",
			"pkg/providers", "pkg/proactive/engine",
		},
		Permit: []string{"pkg/providers/broker"},
	})
}

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(context.Background(), sqlitextest.Open(t))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestStore_RoundTrip(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	at := time.Date(2026, 10, 18, 9, 30, 0, 0, time.UTC)
	want := Decision{
		At: at, Tool: "create_order", TurnKind: "proactive", Trust: proactive.TrustExchange, Tier: T3Discretionary,
		Mode: ModeAsk, LegacyAllowed: false, Reasons: []string{"C: dial T3, column 1"},
	}
	if err := s.Add(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, err := s.Recent(ctx, 5)
	if err != nil || len(got) != 1 {
		t.Fatalf("Recent = %v, %v", got, err)
	}
	g := got[0]
	if !g.At.Equal(at) || g.Tool != want.Tool || g.TurnKind != want.TurnKind || g.Trust != want.Trust ||
		g.Tier != want.Tier || g.Mode != want.Mode || g.LegacyAllowed || len(g.Reasons) != 1 || g.Reasons[0] != want.Reasons[0] {
		t.Fatalf("round trip lost data: %+v", g)
	}
}

func TestPolicyDecisions_Bounded(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	total := MaxRows + 2*pruneEvery + 17
	for i := 0; i < total; i++ {
		d := Decision{At: time.UnixMilli(int64(i)), Tool: "t", Mode: ModeAct, Reasons: []string{"x"}}
		if err := s.Add(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.Count(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n > MaxRows+pruneEvery {
		t.Fatalf("%d rows after %d inserts, want at most %d", n, total, MaxRows+pruneEvery)
	}
	newest, _ := s.Recent(ctx, 1)
	if len(newest) != 1 || newest[0].At.UnixMilli() != int64(total-1) {
		t.Fatalf("the newest row was not kept: %+v", newest)
	}

	// The housekeeper trims to exactly MaxRows even when nothing writes, and only
	// every ten minutes.
	now := time.Now()
	s.Housekeep(ctx, now)
	if n, _ = s.Count(ctx); n != MaxRows {
		t.Fatalf("after housekeeping: %d rows, want %d", n, MaxRows)
	}
	_ = s.Add(ctx, Decision{At: now, Tool: "t", Mode: ModeAct})
	s.Housekeep(ctx, now.Add(time.Minute)) // too soon
	if n, _ = s.Count(ctx); n != MaxRows+1 {
		t.Fatalf("housekeeping ran again inside its interval: %d rows", n)
	}
	s.Housekeep(ctx, now.Add(11*time.Minute))
	if n, _ = s.Count(ctx); n != MaxRows {
		t.Fatalf("housekeeping did not run after its interval: %d rows", n)
	}
}

type spyAuthorizer struct {
	mu    sync.Mutex
	calls int
	notes int
	v     Verdict
}

func (s *spyAuthorizer) Authorize(context.Context, ActionRequest) Verdict {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return s.v
}

func (s *spyAuthorizer) Note(ActionRequest) {
	s.mu.Lock()
	s.notes++
	s.mu.Unlock()
}

func TestRecorder_RecordsPublishesAndFlagsLooserVerdicts(t *testing.T) {
	var buf bytes.Buffer
	prev := logger.GetLevel()
	logger.SetLevel(logger.INFO)
	logger.SetAdditionalWriter(&buf)
	t.Cleanup(func() { logger.SetAdditionalWriter(nil); logger.SetLevel(prev) })

	bus := events.NewBus()
	defer bus.Close()
	sub, ch, err := bus.Channel().OfKind(events.KindPolicyDecision).SubscribeChan(context.Background(), events.SubscribeOptions{Name: "t", Buffer: 8})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	store := newStore(t)
	spy := &spyAuthorizer{v: Verdict{Mode: ModeAct, Tier: T3Discretionary, Reasons: []string{"C: x"}}}
	rc := NewRecorder(spy, store, bus, 0)
	defer rc.Close(time.Second)

	req := ActionRequest{Tool: "create_order", Class: ClassTrade, TurnKind: proactive.TurnProactive, Trust: proactive.TrustExchange,
		Order: &Order{Symbol: "BTC/USDT", Amount: 12345, USD: 98765, USDKnown: true}}

	// The existing checks allowed it and the engine acts: consistent, no error.
	rc.Observe(context.Background(), req, true)
	rc.Flush(2 * time.Second)
	if strings.Contains(buf.String(), "looser") {
		t.Fatalf("a consistent verdict was flagged: %s", buf.String())
	}
	// The existing checks refused it and the engine acts: a bug, logged at ERROR.
	rc.Observe(context.Background(), req, false)
	rc.Flush(2 * time.Second)
	if out := buf.String(); !strings.Contains(out, "looser") || !strings.Contains(out, "create_order") || !strings.Contains(out, "proactive") {
		t.Fatalf("a looser verdict must be logged with the tool and turn kind, got %q", out)
	}
	if spy.notes != 1 {
		t.Fatalf("only an allowed call is noted for the brakes: %d", spy.notes)
	}

	rows, _ := store.Recent(context.Background(), 5)
	if len(rows) != 2 || !rows[1].LegacyAllowed || rows[0].LegacyAllowed {
		t.Fatalf("rows = %+v", rows)
	}

	select {
	case ev := <-ch:
		if ev.Kind != events.KindPolicyDecision || ev.Attrs["tool"] != "create_order" || ev.Attrs["mode"] != "act" || ev.Attrs["tier"] != "T3" {
			t.Fatalf("event = %+v", ev)
		}
		// Names, mode and tier only: no argument or amount anywhere in the event.
		raw := strings.Join([]string{ev.Source.Name, ev.Source.Component}, " ")
		for k, v := range ev.Attrs {
			raw += " " + k + "=" + strings.TrimSpace(strings.ReplaceAll(strings.ToLower(fmtAny(v)), "\n", " "))
		}
		if ev.Payload != nil || strings.Contains(raw, "12345") || strings.Contains(raw, "98765") || strings.Contains(raw, "btc/usdt") {
			t.Fatalf("the event leaks the call's arguments: %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no policy.decision event")
	}
}

func fmtAny(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	}
	return "?"
}
