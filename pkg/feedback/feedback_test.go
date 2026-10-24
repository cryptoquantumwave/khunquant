package feedback

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/orderfeed"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRecord_IdempotentPerEvent(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	r := Response{InterventionID: "01HZ", Kind: KindUseful, Source: "telegram:button", Event: "55:useful"}

	for i, want := range []bool{true, false, false} {
		added, err := f.fb.Record(ctx, r)
		if err != nil || added != want {
			t.Fatalf("record %d = %v, %v; want %v", i, added, err, want)
		}
	}
	if f.count() != 1 {
		t.Fatalf("a repeated event wrote %d rows, want 1", f.count())
	}
	// Another message, another kind and another source are other events.
	for _, o := range []Response{
		{InterventionID: "01HZ", Kind: KindUseful, Source: "telegram:button", Event: "56:useful"},
		{InterventionID: "01HZ", Kind: KindNotUseful, Source: "telegram:button", Event: "55:not_useful"},
		{InterventionID: "01HZ", Kind: KindUseful, Source: "web:button", Event: "55:useful"},
	} {
		if added, err := f.fb.Record(ctx, o); err != nil || !added {
			t.Fatalf("%+v = %v, %v; want recorded", o, added, err)
		}
	}
	if f.count() != 4 {
		t.Fatalf("%d rows, want 4", f.count())
	}
}

func TestRecord_RefusesWhatIsNotAResponse(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for name, r := range map[string]Response{
		"no intervention": {Kind: KindUseful, Source: "x"},
		"no source":       {InterventionID: "01HZ", Kind: KindUseful},
		"unknown kind":    {InterventionID: "01HZ", Kind: "loved", Source: "x"},
	} {
		if added, err := f.fb.Record(ctx, r); err == nil || added {
			t.Errorf("%s: recorded (%v, %v)", name, added, err)
		}
	}
	if f.count() != 0 {
		t.Fatalf("%d rows written by refused responses", f.count())
	}
	var nilSvc *Service
	if added, err := nilSvc.Record(ctx, Response{}); added || err != nil {
		t.Fatalf("a nil service must do nothing: %v %v", added, err)
	}
}

func TestRecord_PublishesIdsAndKindOnly(t *testing.T) {
	f := newFixture(t)
	_, ch, err := f.bus.Channel().OfKind(events.KindFeedbackRecorded).SubscribeChan(context.Background(), events.SubscribeOptions{Name: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.fb.Record(context.Background(), Response{
		InterventionID: "01HZ", Kind: KindReply, Value: "negative", Source: "telegram:reply", Event: "9",
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-ch:
		p, _ := ev.Payload.(map[string]any)
		if len(p) != 3 || p["intervention_id"] != "01HZ" || p["kind"] != "reply" || p["id"] == "" {
			t.Fatalf("payload = %v, want id, intervention_id and kind only", p)
		}
		if strings.Contains(strings.ToLower(ev.Source.Component+ev.Source.Name), "negative") {
			t.Fatal("the value leaked into the event")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("feedback.recorded was not published")
	}
}

func fill(symbol, orderID string, at time.Time, mut ...func(*orderfeed.OrderEvent)) events.Event {
	o := orderfeed.OrderEvent{Provider: "okx", Symbol: symbol, Side: "buy", OrderID: orderID, Status: "closed",
		Origin: "place_order", TurnKind: proactive.TurnUser, At: at}
	for _, m := range mut {
		m(&o)
	}
	return events.Event{ID: "e-" + orderID, Kind: events.KindPerceptionOrderFilled, Time: at, Payload: o}
}

func TestActedOnWithinWindow(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if err := f.fb.Start(ctx, f.bus); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.fb.Stop)

	btc, _ := f.deliver("BTC/USDT", "price:btc")
	eth, _ := f.deliver("ETH/USDT", "price:eth")
	delivered := f.clock.Now()

	publish := func(ev events.Event) { f.bus.PublishNonBlocking(ev) }

	// Inside the window, on the same asset (a perpetual symbol is the same asset).
	publish(fill("BTC/USDT:USDT", "o1", delivered.Add(5*time.Hour)))
	waitFor(t, "acted_on for BTC", func() bool { return len(f.kinds(btc)) == 1 })
	rows, _ := f.fb.Responses(ctx, btc)
	if rows[0].Kind != string(KindActedOn) || rows[0].Source != SourceOrderFill {
		t.Fatalf("row = %+v, want acted_on from %s", rows[0], SourceOrderFill)
	}

	// Not acted on: too late, another asset, a paper order, a scheduled order, a
	// routine DCA workflow order. Then a marker event that is acted on, which proves
	// the earlier ones were handled (the subscriber is sequential) and recorded nothing.
	publish(fill("BTC/USDT", "late", delivered.Add(6*time.Hour+time.Minute)))
	publish(fill("SOL/USDT", "other", delivered.Add(time.Hour)))
	publish(fill("BTC/USDT", "paper", delivered.Add(time.Hour), func(o *orderfeed.OrderEvent) { o.Paper = true }))
	publish(fill("BTC/USDT", "cron", delivered.Add(time.Hour), func(o *orderfeed.OrderEvent) { o.TurnKind = proactive.TurnCronLegacy }))
	publish(fill("BTC/USDT", "dca", delivered.Add(time.Hour), func(o *orderfeed.OrderEvent) { o.Origin = "journal:dca.execute" }))
	publish(fill("ETH/USDT", "marker", delivered.Add(time.Hour)))
	waitFor(t, "the marker", func() bool { return len(f.kinds(eth)) == 1 })
	if got := f.kinds(btc); !reflect.DeepEqual(got, []string{"acted_on"}) {
		t.Fatalf("BTC rows = %v, want the one in-window fill only", got)
	}

	// A replayed fill is the same event; a second order is another.
	publish(fill("BTC/USDT:USDT", "o1", delivered.Add(5*time.Hour)))
	publish(fill("BTC/USDT", "o2", delivered.Add(2*time.Hour)))
	waitFor(t, "the second order", func() bool { return len(f.kinds(btc)) == 2 })
	time.Sleep(20 * time.Millisecond)
	if got := f.kinds(btc); len(got) != 2 {
		t.Fatalf("BTC rows = %v after a replay, want 2", got)
	}
}

func TestActedOn_PlanOrIntentChangeOnTheSameSubject(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	asset, _ := f.deliver("BTC/USDT", "price:btc")
	plan, _ := f.deliver("plan:12", "dn:12")
	at := f.clock.Now().Add(time.Hour)

	f.fb.ObserveChange(ctx, ChangeIntent, "BTC", at, "intent-1")
	f.fb.ObserveChange(ctx, ChangePlan, "plan:12", at, "plan-12-v2")
	f.fb.ObserveChange(ctx, ChangePlan, "plan:99", at, "plan-99-v2") // some other plan
	f.fb.ObserveChange(ctx, ChangeIntent, "BTC", at, "intent-1")     // replayed

	a, _ := f.fb.Responses(ctx, asset)
	p, _ := f.fb.Responses(ctx, plan)
	if len(a) != 1 || a[0].Source != ChangeIntent || a[0].Kind != string(KindActedOn) {
		t.Fatalf("asset rows = %+v, want one acted_on from %s", a, ChangeIntent)
	}
	if len(p) != 1 || p[0].Source != ChangePlan {
		t.Fatalf("plan rows = %+v, want one acted_on from %s", p, ChangePlan)
	}
	// "plan:12" never matches an asset.
	if got := baseOf("plan:12"); got != "" {
		t.Fatalf("baseOf(plan:12) = %q", got)
	}
}

func TestApprovalsAreLinkedToTheirIntervention(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if err := f.fb.Start(ctx, f.bus); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.fb.Stop)
	id, _ := f.deliver("BTC/USDT", "price:btc")

	ev := func(kind events.Kind, status, prop, iid string) events.Event {
		return events.Event{Kind: kind, Payload: map[string]any{"id": prop, "status": status, "intervention_id": iid}}
	}
	f.bus.PublishNonBlocking(ev(events.KindApprovalDecided, "approved", "p1", id))
	f.bus.PublishNonBlocking(ev(events.KindApprovalDecided, "approved", "p1", id)) // replay
	f.bus.PublishNonBlocking(ev(events.KindApprovalDecided, "rejected", "p2", id))
	f.bus.PublishNonBlocking(ev(events.KindApprovalExpired, "expired", "p3", id))
	f.bus.PublishNonBlocking(ev(events.KindApprovalDecided, "approved", "p4", "")) // a card with no intervention
	waitFor(t, "three decisions", func() bool { return len(f.kinds(id)) == 3 })
	time.Sleep(20 * time.Millisecond)

	got := map[string]bool{}
	for _, k := range f.kinds(id) {
		got[k] = true
	}
	if len(f.kinds(id)) != 3 || !got["approved"] || !got["rejected"] || !got["expired"] {
		t.Fatalf("kinds = %v, want approved, rejected and expired once each", f.kinds(id))
	}
}

func TestNoResponseAfter24h(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	quiet, _ := f.deliver("BTC/USDT", "price:btc")
	answered, _ := f.deliver("ETH/USDT", "price:eth")
	if _, err := f.fb.Record(ctx, Response{InterventionID: answered, Kind: KindUseful, Source: "telegram:button", Event: "1"}); err != nil {
		t.Fatal(err)
	}

	f.clock.Add(UnansweredAfter - time.Minute)
	if n, err := f.fb.Housekeep(ctx); err != nil || n != 0 {
		t.Fatalf("after 23h59m: marked %d (%v), want 0", n, err)
	}
	f.clock.Add(2 * time.Minute)
	if n, err := f.fb.Housekeep(ctx); err != nil || n != 1 {
		t.Fatalf("after 24h01m: marked %d (%v), want 1 (only the unanswered one)", n, err)
	}
	if n, _ := f.fb.Housekeep(ctx); n != 0 {
		t.Fatalf("a second pass marked %d more", n)
	}
	rows, _ := f.fb.Responses(ctx, quiet)
	if len(rows) != 1 || rows[0].Kind != string(KindNone) || rows[0].Value != "unknown" || rows[0].Source != SourceHousekeep {
		t.Fatalf("rows = %+v, want one none with the value unknown", rows)
	}
	if got := f.kinds(answered); !reflect.DeepEqual(got, []string{"useful"}) {
		t.Fatalf("an answered intervention has %v, want its own signal only", got)
	}
}

func TestNoResponse_NotForInterventionsBeforeTheTableExisted(t *testing.T) {
	f := newFixture(t)
	f.deliver("BTC/USDT", "price:btc")
	f.fb.since = f.clock.Now().Add(time.Minute) // the recorder was created after the delivery
	f.clock.Add(48 * time.Hour)
	if n, err := f.fb.Housekeep(context.Background()); err != nil || n != 0 {
		t.Fatalf("marked %d (%v) interventions older than the table", n, err)
	}
}

func TestHousekeepDeletesRowsOlderThan180Days(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	old := f.clock.Now().Add(-Retention - time.Hour)
	for i, at := range []time.Time{old, f.clock.Now()} {
		if _, err := f.fb.Record(ctx, Response{InterventionID: "01HZ", Kind: KindUseful, Source: "s", Event: string(rune('a' + i)), At: at}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.fb.Housekeep(ctx); err != nil {
		t.Fatal(err)
	}
	if f.count() != 1 {
		t.Fatalf("%d rows after the sweep, want 1", f.count())
	}
}

func TestClassifyReply(t *testing.T) {
	for text, want := range map[string]string{
		"yes":                ReplyAffirmative,
		"OK thanks!":         ReplyAffirmative,
		"ตกลง":               ReplyAffirmative,
		"ขอบคุณครับ":         ReplyAffirmative,
		"no":                 ReplyNegative,
		"not ok":             ReplyNegative,
		"stop sending these": ReplyNegative,
		"ไม่เอา หยุดส่ง":         ReplyNegative,
		"ไม่ใช่":                 ReplyNegative,
		"why did it drop?":       ReplyQuestion,
		"what is the price":      ReplyQuestion,
		"ทำไมถึงลง":              ReplyQuestion,
		"ซื้อได้ไหม":             ReplyQuestion,
		"I will look at it":      ReplyOther,
		"":                       ReplyOther,
		"   ":                    ReplyOther,
		"BTC 65000 then 66000":   ReplyOther,
		"does it matter?":        ReplyQuestion,
		"great, but why is that": ReplyAffirmative,
	} {
		if got := ClassifyReply(text); got != want {
			t.Errorf("ClassifyReply(%q) = %s, want %s", text, got, want)
		}
	}
}
