package triggers

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex/sqlitextest"
)

func TestUpdateFresh_ClearsTheStateUpdateKeeps(t *testing.T) {
	ce := newCrossEnv(t)
	s := ce.add("u1", KindPriceCross, ModeShadow, priceParams("above", 60000, `"recurring":true,"fire_if_already":false`))
	ce.at(noon).tick(61000) // first observation above: a state is saved
	if _, state, _ := ce.row("u1"); state == "" {
		t.Fatal("no state saved")
	}

	s.Mode = ModeLive
	if err := ce.r.Update(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if _, state, _ := ce.row("u1"); state == "" {
		t.Fatal("Update with the same parameters dropped the state")
	}
	if err := ce.r.UpdateFresh(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if mode, state, errs := ce.row("u1"); state != "" || mode != ModeLive || errs != 0 {
		t.Fatalf("after UpdateFresh: mode %q state %q errors %d", mode, state, errs)
	}
	// And the trigger really starts from nothing: a first observation, on the target
	// side, with fire_if_already=false, saves a disarmed state again.
	ce.at(noon.Add(time.Minute)).tick(61000)
	if _, state, _ := ce.row("u1"); !strings.Contains(state, `"side":"above"`) {
		t.Fatalf("state = %q", state)
	}
}

func TestReadSpecs(t *testing.T) {
	db := sqlitextest.Open(t)
	// No table, no error, nothing created.
	specs, err := ReadSpecs(context.Background(), db, ListFilter{})
	if err != nil || specs != nil {
		t.Fatalf("ReadSpecs on an empty database = %v, %v", specs, err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name = 'triggers'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("ReadSpecs created the table (%d, %v)", n, err)
	}
	if specs, err := ReadSpecs(context.Background(), nil, ListFilter{}); err != nil || specs != nil {
		t.Fatalf("nil db: %v, %v", specs, err)
	}

	ce := newCrossEnv(t)
	ce.add("r1", KindPriceCross, ModeLive, priceParams("above", 1, ""))
	s := ce.add("r2", KindPriceCross, ModeShadow, priceParams("above", 2, ""))
	s.Origin = "migration:cron:abc"
	if err := ce.r.Update(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	all, err := ReadSpecs(context.Background(), ce.db, ListFilter{})
	if err != nil || len(all) != 2 {
		t.Fatalf("all = %v, %v", all, err)
	}
	mig, _ := ReadSpecs(context.Background(), ce.db, ListFilter{Origin: "migration:"})
	live, _ := ReadSpecs(context.Background(), ce.db, ListFilter{Mode: ModeLive})
	if len(mig) != 1 || mig[0].ID != "r2" || len(live) != 1 || live[0].ID != "r1" {
		t.Fatalf("filters: %v / %v", mig, live)
	}
}

func TestGlobalMode(t *testing.T) {
	ce := newCrossEnv(t)
	for in, want := range map[string]string{ModeLive: ModeLive, ModeShadow: ModeShadow, ModeOff: ModeOff, "": ModeOff, "bogus": ModeOff, " LIVE ": ModeLive} {
		ce.setGlobal(in)
		if got := ce.r.GlobalMode(); got != want {
			t.Errorf("global %q -> %q, want %q", in, got, want)
		}
	}
}

func TestTurnOff_IsHeldBackUntilEveryCandidateIsDecided(t *testing.T) {
	ce := newCrossEnv(t)
	ce.add("t1", KindPriceCross, ModeLive, priceParams("above", 60000, `"fire_if_already":false`))
	ce.at(noon).tick(59000)
	v := crossView{now: noon, prices: map[[2]string]crossPrice{{"okx", "BTC/USDT"}: {61000, noon, 1}}}
	cs := ce.evalView(v, exchangeDiff(kindTick, noon), false)
	if len(cs) != 1 {
		t.Fatalf("got %d candidates", len(cs))
	}
	// The next event arrives without Committed ever being called for this one (a
	// crash, a failed decision): the switch-off is dropped with the rest.
	ce.evalView(crossView{now: noon}, exchangeDiff(kindTick, noon), false)
	if mode, _, _ := ce.row("t1"); mode != ModeLive {
		t.Fatalf("mode = %q", mode)
	}
}
