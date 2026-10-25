package triggers

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/worldmodel"
)

const intentMarker = "ZXQ-TITLE-MARKER-4417"

func TestOwnerAlertSpec_IntentOrigins(t *testing.T) {
	cases := []struct {
		kind, origin string
		want         bool
	}{
		{KindPriceCross, IntentOrigin("01HABC"), true},
		{KindIndicatorCross, IntentOrigin("01HABC"), true},
		{KindTimeAt, IntentOrigin("01HABC"), true},
		// a standing order is a Notice like any other
		{KindPriceCross, IntentOrderOrigin("01HABC"), false},
		{KindIndicatorCross, IntentOrderOrigin("01HABC"), false},
		{KindTimeAt, IntentOrderOrigin("01HABC"), false},
		// no id, or more than one token
		{KindPriceCross, "intent:", false},
		{KindPriceCross, "intent:a:b", false},
		{KindPriceCross, "intent: a", false},
		{KindTimeAt, "intent:", false},
		{KindTimeAt, "intent", false},
		// a time_at has no alert tool
		{KindTimeAt, OriginSetPriceAlert, false},
		{KindTimeAt, MigratedOriginPrefix + "1", false},
		// the intent origin on any other kind does not get the mark
		{KindDCADue, IntentOrigin("01HABC"), false},
		{KindDNHealth, IntentOrigin("01HABC"), false},
		{KindExternal, IntentOrigin("01HABC"), false},
		{KindMoveVsVol, IntentOrigin("01HABC"), false},
		{KindDrawdown, IntentOrigin("01HABC"), false},
		{KindAllocationDrift, IntentOrigin("01HABC"), false},
		{KindLiquidationDistance, IntentOrigin("01HABC"), false},
		{KindDataStale, IntentOrigin("01HABC"), false},
	}
	for _, c := range cases {
		if got := OwnerAlertSpec(c.kind, c.origin); got != c.want {
			t.Errorf("OwnerAlertSpec(%q, %q) = %v, want %v", c.kind, c.origin, got, c.want)
		}
	}
}

func TestIntentOfOrigin(t *testing.T) {
	if id, order, ok := IntentOfOrigin(IntentOrigin("abc")); id != "abc" || order || !ok {
		t.Fatalf("watch origin: %q %v %v", id, order, ok)
	}
	if id, order, ok := IntentOfOrigin(IntentOrderOrigin("abc")); id != "abc" || !order || !ok {
		t.Fatalf("order origin: %q %v %v", id, order, ok)
	}
	for _, o := range []string{"", "intent", "intent:", "intent:a:b", "tool:set_price_alert", "intent-order:", "intentx:abc"} {
		if _, _, ok := IntentOfOrigin(o); ok {
			t.Errorf("%q read as an intent origin", o)
		}
	}
}

func addIntentSpec(t *testing.T, ce *crossEnv, id, origin, params string) {
	t.Helper()
	if _, err := ce.r.Add(context.Background(), Spec{
		ID: id, Kind: KindPriceCross, Mode: ModeLive, Origin: origin, IntentID: id, Params: json.RawMessage(params),
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}
}

// The title of an intent travels in the Quote and nowhere else; the key is built from
// the kind and the intent's id; a watch is the owner's alert and a standing order is
// not.
func TestIntentCrossing_CandidateShape(t *testing.T) {
	for _, c := range []struct {
		name, origin string
		owner, order bool
	}{
		{"watch", IntentOrigin("01HWATCH"), true, false},
		{"standing order", IntentOrderOrigin("01HWATCH"), false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			ce := newCrossEnv(t)
			addIntentSpec(t, ce, "01HWATCH", c.origin,
				priceParams("above", 60000, `"fire_if_already":false,"message":"`+intentMarker+`"`))
			ce.at(noon).tick(59000)
			cs := ce.at(noon.Add(time.Minute)).tick(61000)
			if len(cs) != 1 {
				t.Fatalf("got %d candidates", len(cs))
			}
			got := cs[0]
			if got.Quote != intentMarker {
				t.Fatalf("Quote = %q", got.Quote)
			}
			if want := "price_cross:01HWATCH:BTC/USDT"; got.DedupKey != want {
				t.Fatalf("DedupKey = %q, want %q", got.DedupKey, want)
			}
			ev, _ := json.Marshal(got.Evidence)
			for name, field := range map[string]string{
				"Title": got.Title, "Subject": got.Subject, "Detail": got.Detail, "DedupKey": got.DedupKey,
				"TriggerID": got.TriggerID, "Evidence": string(ev),
			} {
				if strings.Contains(field, intentMarker) {
					t.Errorf("%s holds the intent's title: %q", name, field)
				}
			}
			if got.IntentID != "01HWATCH" {
				t.Fatalf("IntentID = %q", got.IntentID)
			}
			if got.OwnerRequested != c.owner {
				t.Fatalf("OwnerRequested = %v, want %v", got.OwnerRequested, c.owner)
			}
			if c.order != strings.HasPrefix(got.Title, "Standing order: ") {
				t.Fatalf("Title = %q, standing order = %v", got.Title, c.order)
			}
		})
	}
}

// An alert the owner set with the tools keeps its own key and evidence.
func TestIntentCrossing_AlertToolSpecIsUnchanged(t *testing.T) {
	ce := newCrossEnv(t)
	addOrigin(t, ce, "p1", KindPriceCross, ModeLive, OriginSetPriceAlert,
		priceParams("above", 60000, `"fire_if_already":false,"message":"hello"`))
	ce.at(noon).tick(59000)
	cs := ce.at(noon.Add(time.Minute)).tick(61000)
	if len(cs) != 1 || !strings.HasPrefix(cs[0].DedupKey, "price_cross:okx:BTC/USDT:above:") || cs[0].Evidence["message"] != "hello" {
		t.Fatalf("candidate = %+v", cs)
	}
}

func TestFireObserver_ReportsLiveCandidatesAfterTheCommit(t *testing.T) {
	ce := newCrossEnv(t)
	var mu sync.Mutex
	var got []Firing
	ce.r.SetFireObserver(func(f Firing) {
		mu.Lock()
		got = append(got, f)
		mu.Unlock()
	})
	count := func() int { mu.Lock(); defer mu.Unlock(); return len(got) }
	addIntentSpec(t, ce, "01HLIVE", IntentOrigin("01HLIVE"), priceParams("above", 60000, `"fire_if_already":false`))
	addOrigin(t, ce, "p1", KindPriceCross, ModeLive, OriginSetPriceAlert, priceParams("above", 60000, `"fire_if_already":false`))

	ce.at(noon).tick(59000)
	// Evaluated but not committed: nothing is counted.
	ce.at(noon.Add(time.Minute))
	v := crossView{now: ce.now, prices: map[[2]string]crossPrice{{"okx", "BTC/USDT"}: {61000, ce.now, proactive.TrustExchange}}}
	cs := ce.evalView(v, exchangeDiff(events.KindPerceptionMarketTick, ce.now), false)
	if len(cs) != 2 || count() != 0 {
		t.Fatalf("%d candidates, %d firings before the commit", len(cs), count())
	}
	// A commit that names only the alert tool's candidate leaves the intent's alone.
	var other []string
	for _, c := range cs {
		if c.IntentID == "" {
			other = append(other, c.ID)
		}
	}
	ce.r.Committed(context.Background(), other)
	if count() != 0 {
		t.Fatalf("a firing was reported for a candidate attention had not decided")
	}
	// The intent's trigger kept its old state and fires again; committed in full this
	// time, that is one firing, for the intent.
	cs = ce.evalView(v, exchangeDiff(events.KindPerceptionMarketTick, ce.now), true)
	if len(cs) != 1 || count() != 1 || got[0].IntentID != "01HLIVE" {
		t.Fatalf("%d candidates, firings = %+v", len(cs), got)
	}
}

func TestFireObserver_ShadowDoesNotCountAndANoticeIsNotAFiring(t *testing.T) {
	ce := newCrossEnv(t)
	var n int
	ce.r.SetFireObserver(func(Firing) { n++ })
	if _, err := ce.r.Add(context.Background(), Spec{
		ID: "01HSHAD", Kind: KindPriceCross, Mode: ModeShadow, Origin: IntentOrigin("01HSHAD"), IntentID: "01HSHAD",
		Params: json.RawMessage(priceParams("above", 60000, `"fire_if_already":false`)),
	}); err != nil {
		t.Fatal(err)
	}
	ce.at(noon).tick(59000)
	cs := ce.at(noon.Add(time.Minute)).tick(61000)
	if len(cs) != 1 || !cs[0].Shadow || n != 0 {
		t.Fatalf("shadow: %d candidates, %d firings", len(cs), n)
	}
}

// The global mode caps a live spec: in global shadow a live intent spec is only
// rehearsed, and a rehearsal does not count against the intent's budget.
func TestFireObserver_GlobalShadowDoesNotCount(t *testing.T) {
	ce := newCrossEnv(t)
	ce.setGlobal(ModeShadow)
	var n int
	ce.r.SetFireObserver(func(Firing) { n++ })
	addIntentSpec(t, ce, "01HSHAD", IntentOrigin("01HSHAD"), priceParams("above", 60000, `"fire_if_already":false`))
	ce.at(noon).tick(59000)
	if cs := ce.at(noon.Add(time.Minute)).tick(61000); len(cs) != 1 || !cs[0].Shadow || n != 0 {
		t.Fatalf("global shadow: %+v, %d firings", cs, n)
	}
}

// A live firing is reported once, with the ids and the key and no text.
func TestFireObserver_LiveFiringCarriesIDsOnly(t *testing.T) {
	ce := newCrossEnv(t)
	var got []Firing
	ce.r.SetFireObserver(func(f Firing) { got = append(got, f) })
	addIntentSpec(t, ce, "01HLIVE", IntentOrigin("01HLIVE"),
		priceParams("above", 60000, `"fire_if_already":false,"message":"`+intentMarker+`"`))
	ce.at(noon).tick(59000)
	ce.at(noon.Add(time.Minute)).tick(61000)
	if len(got) != 1 || got[0].IntentID != "01HLIVE" || got[0].SpecID != "01HLIVE" || got[0].Kind != KindPriceCross ||
		got[0].DedupKey != "price_cross:01HLIVE:BTC/USDT" {
		t.Fatalf("firings = %+v", got)
	}
}

// ---- time_at -------------------------------------------------------------------

type timerEv perception.TimerDue

func (e timerEv) TimerDue() perception.TimerDue { return perception.TimerDue(e) }

// timeAtEnv drives time_at through a real registry.
type timeAtEnv struct {
	*env
	lang string
}

func newTimeAtEnv(t *testing.T) *timeAtEnv {
	t.Helper()
	te := &timeAtEnv{lang: "en"}
	te.env = newEnv(t, map[string]Factory{KindTimeAt: TimeAtFactory(CrossConfig{Language: func() string { return te.lang }})})
	return te
}

func (te *timeAtEnv) addTimeAt(id, origin string, p TimeAtParams) {
	te.t.Helper()
	if _, err := te.r.Add(context.Background(), Spec{
		ID: id, Kind: KindTimeAt, Mode: ModeLive, Origin: origin, IntentID: id, Params: p.JSON(),
	}); err != nil {
		te.t.Fatalf("Add: %v", err)
	}
}

func (te *timeAtEnv) fire(td perception.TimerDue) []proactive.Candidate {
	te.t.Helper()
	d := worldmodel.Diff{
		Event:   events.Event{ID: "t", Kind: events.KindPerceptionClockTimer, Trust: events.TrustPtr(proactive.TrustSystem)},
		At:      td.Due,
		Payload: timerEv(td),
	}
	cs, err := te.r.Evaluate(context.Background(), fakeView{now: noon}, d)
	if err != nil {
		te.t.Fatal(err)
	}
	te.r.Committed(context.Background(), idsOf(cs))
	return cs
}

func TestTimeAt_FiresForItsOwnTimerOnly(t *testing.T) {
	te := newTimeAtEnv(t)
	due := time.Date(2026, 10, 16, 2, 0, 0, 0, time.UTC)
	te.addTimeAt("01HREM", IntentOrigin("01HREM"), TimeAtParams{AtMS: due.UnixMilli(), Zone: "Asia/Bangkok", Budget: 1, Message: intentMarker})

	if cs := te.fire(perception.TimerDue{ID: "someone-else", Due: due}); len(cs) != 0 {
		t.Fatalf("another timer's event fired it: %+v", cs)
	}
	cs := te.fire(perception.TimerDue{ID: "01HREM", Due: due})
	if len(cs) != 1 {
		t.Fatalf("got %d candidates", len(cs))
	}
	c := cs[0]
	if c.Title != "Reminder" || c.Severity != proactive.SeverityNotice || c.Compose != proactive.ComposeTemplate {
		t.Fatalf("candidate = %+v", c)
	}
	if c.Detail != "Due 2026-10-16 09:00 +07" {
		t.Fatalf("Detail = %q: the due time is shown in the owner's zone", c.Detail)
	}
	if c.Quote != intentMarker || !c.OwnerRequested {
		t.Fatalf("Quote = %q, OwnerRequested = %v", c.Quote, c.OwnerRequested)
	}
	if c.DedupKey != TimeAtKey("01HREM", due.UnixMilli()) {
		t.Fatalf("DedupKey = %q", c.DedupKey)
	}
	ev, _ := json.Marshal(c.Evidence)
	for name, field := range map[string]string{"Title": c.Title, "Subject": c.Subject, "Detail": c.Detail, "DedupKey": c.DedupKey, "Evidence": string(ev)} {
		if strings.Contains(field, intentMarker) {
			t.Errorf("%s holds the intent's title: %q", name, field)
		}
	}
	// A one-shot reminder switches itself off once decided, and never fires twice.
	if mode, _, _ := te.row("01HREM"); mode != ModeOff {
		t.Fatalf("mode after its firing = %q", mode)
	}
}

func TestTimeAt_StandingOrderOriginIsNotTheOwnersAlert(t *testing.T) {
	te := newTimeAtEnv(t)
	due := time.Date(2026, 10, 16, 2, 0, 0, 0, time.UTC)
	te.addTimeAt("01HREM", IntentOrderOrigin("01HREM"), TimeAtParams{AtMS: due.UnixMilli(), Budget: 1})
	cs := te.fire(perception.TimerDue{ID: "01HREM", Due: due})
	if len(cs) != 1 || cs[0].OwnerRequested {
		t.Fatalf("candidates = %+v", cs)
	}
}

func TestTimeAt_RecurringHonoursItsBudgetAndExpiry(t *testing.T) {
	te := newTimeAtEnv(t)
	d0 := time.Date(2026, 10, 16, 2, 0, 0, 0, time.UTC)
	te.addTimeAt("01HREC", IntentOrigin("01HREC"), TimeAtParams{Cron: "0 9 * * *", TZ: "Asia/Bangkok", Budget: 2, UntilMS: d0.Add(72 * time.Hour).UnixMilli()})
	if cs := te.fire(perception.TimerDue{ID: "01HREC", Due: d0}); len(cs) != 1 {
		t.Fatalf("first: %d", len(cs))
	}
	if mode, _, _ := te.row("01HREC"); mode != ModeLive {
		t.Fatalf("a recurring reminder with budget left switched itself off")
	}
	if cs := te.fire(perception.TimerDue{ID: "01HREC", Due: d0.Add(24 * time.Hour)}); len(cs) != 1 {
		t.Fatalf("second: %d", len(cs))
	}
	if mode, _, _ := te.row("01HREC"); mode != ModeOff {
		t.Fatalf("a spent budget leaves the reminder on: %q", mode)
	}
	// Past its expiry nothing fires, whatever the budget.
	te2 := newTimeAtEnv(t)
	te2.addTimeAt("01HEXP", IntentOrigin("01HEXP"), TimeAtParams{Cron: "0 9 * * *", TZ: "Asia/Bangkok", UntilMS: d0.UnixMilli()})
	if cs := te2.fire(perception.TimerDue{ID: "01HEXP", Due: d0}); len(cs) != 0 {
		t.Fatalf("a reminder fired at its intent's expiry: %+v", cs)
	}
}

// A missed reminder is one Notice with its time, and it is not a fire.
func TestTimeAt_MissedIsOneNoticeAndDoesNotCount(t *testing.T) {
	te := newTimeAtEnv(t)
	due := time.Date(2026, 10, 16, 2, 0, 0, 0, time.UTC)
	te.addTimeAt("01HMISS", IntentOrigin("01HMISS"), TimeAtParams{AtMS: due.UnixMilli(), Zone: "Asia/Bangkok", Budget: 1, Message: intentMarker})
	cs := te.fire(perception.TimerDue{ID: "01HMISS", Due: due, Late: true, Missed: true})
	if len(cs) != 1 || cs[0].Title != "Reminder missed" || !strings.Contains(cs[0].Detail, "2026-10-16 09:00") {
		t.Fatalf("candidates = %+v", cs)
	}
	if !IsTimeAtMissedKey(cs[0].DedupKey) || IsTimeAtMissedKey(TimeAtKey("01HMISS", due.UnixMilli())) {
		t.Fatalf("key = %q", cs[0].DedupKey)
	}
	if cs[0].Quote != intentMarker {
		t.Fatalf("Quote = %q", cs[0].Quote)
	}
}

func TestTimeAt_ThaiAndParams(t *testing.T) {
	te := newTimeAtEnv(t)
	te.lang = "th"
	due := time.Date(2026, 10, 16, 2, 0, 0, 0, time.UTC)
	te.addTimeAt("01HTH", IntentOrigin("01HTH"), TimeAtParams{AtMS: due.UnixMilli(), Zone: "Asia/Bangkok", Budget: 1})
	if cs := te.fire(perception.TimerDue{ID: "01HTH", Due: due}); len(cs) != 1 || cs[0].Title != "เตือนความจำ" {
		t.Fatalf("candidates = %+v", cs)
	}
	for name, p := range map[string]TimeAtParams{
		"neither":        {},
		"both":           {AtMS: 1, Cron: "* * * * *"},
		"tz with at":     {AtMS: 1, TZ: "Asia/Bangkok"},
		"negative until": {AtMS: 1, UntilMS: -1},
		"budget too big": {AtMS: 1, Budget: MaxTimeAtBudget + 1},
	} {
		if err := p.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := ParseTimeAt(json.RawMessage(`{"at_ms":1,"bogus":2}`)); err == nil {
		t.Error("an unknown field was accepted")
	}
}
