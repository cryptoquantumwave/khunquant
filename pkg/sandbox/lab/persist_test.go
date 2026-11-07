package lab

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/fault"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/market"
)

var (
	_ Persistent = (*sandbox.StatefulSimulator)(nil)
	_ Persistent = (*market.Book)(nil)
	_ Persistent = (*fault.Injector)(nil)
)

// labAt opens a lab on a home that may hold a saved world, with the fault injector
// registered as the gateway does.
func labAt(t *testing.T, home string) (*Lab, *fault.Injector) {
	t.Helper()
	fixtures := filepath.Join(home, "workspace", "sandbox")
	if _, err := os.Stat(fixtures); err != nil {
		copyFixtures(t, fixtures)
	}
	l, err := New(context.Background(), Options{Home: home, FixturesDir: fixtures, Token: testToken})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	f := fault.New(fault.Options{Now: func() time.Time { return l.Now() }})
	f.EnableOrderKinds("lookup_lag", "partial")
	l.Register(f)
	if err := l.Restore(); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	return l, f
}

func createOrder(t *testing.T, l *Lab, cid string) {
	t.Helper()
	url := l.Server.BaseURL() + "/__sbx__/binance/fapi.binance.com/fapi/v1/order?symbol=BTCUSDT&side=BUY&type=market&quantity=0.001&newClientOrderId=" + cid
	resp, err := http.Post(url, "application/x-www-form-urlencoded", nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("create = %d %s", resp.StatusCode, body)
	}
}

// The simulated world survives a close and a new lab on the same home: orders with their
// client IDs, the order-ID counter (the next ID is above every stored one), the market
// book and the fault rules with their firing counts.
func TestPersist_RestoreExchangeBookFaults(t *testing.T) {
	home := t.TempDir()
	t.Cleanup(func() { sandbox.SetGlobalState(false, "") })
	a, fa := labAt(t, home)
	pair := market.Pair{Base: "BTC", Quote: "USDT"}
	if err := a.Market.Set("binance", pair, 61000); err != nil {
		t.Fatal(err)
	}
	if err := a.Market.Path("binance", pair, 62000, time.Hour, market.ShapeLinear, nil); err != nil {
		t.Fatal(err)
	}
	if err := a.Market.Delist("okx", "ETH/USDT"); err != nil {
		t.Fatal(err)
	}
	if err := a.Market.SetFunding("binance", pair, 0.0007); err != nil {
		t.Fatal(err)
	}
	createOrder(t, a, "cid-one")
	createOrder(t, a, "cid-two")
	id, err := fa.Add(fault.Rule{Match: fault.Match{Venue: "binance"}, Effect: fault.Effect{Order: "lookup_lag", Lag: fault.Duration(time.Minute)}, When: fault.When{Times: 3}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := fa.OrderEffect(sandbox.OrderCreate, sandbox.OrderInfo{Venue: "binance"}); !ok {
		t.Fatal("the rule did not fire")
	}
	for _, key := range []string{"market", "exchange", "faults"} {
		if err := a.Persist(key); err != nil {
			t.Fatalf("Persist(%s): %v", key, err)
		}
	}
	var before []string
	a.Sim.Read("binance", func(st *sandbox.VenueState) {
		for oid := range st.OpenOrders {
			before = append(before, oid)
		}
		for oid := range st.ClosedOrders {
			before = append(before, oid)
		}
	})
	if len(before) != 2 {
		t.Fatalf("two orders were placed, the ledger holds %v", before)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}

	b, fb := labAt(t, home)
	t.Cleanup(func() { _ = b.Close() })
	if px, ok := b.Market.Price("binance", pair, b.Now()); !ok || px < 61000 || px > 62000 {
		t.Errorf("restored price = %v, %v", px, ok)
	}
	if !b.Market.IsDelisted("okx", market.Pair{Base: "ETH", Quote: "USDT"}) {
		t.Error("the delisting was lost")
	}
	if r := b.Market.Funding("binance", pair); r != 0.0007 {
		t.Errorf("funding rate = %v", r)
	}
	rules := fb.List()
	if len(rules) != 1 || rules[0].Rule.ID != id || rules[0].Fired != 1 {
		t.Fatalf("restored rules = %+v, want %s fired once", rules, id)
	}
	var cids []string
	var maxID int64
	b.Sim.Read("binance", func(st *sandbox.VenueState) {
		for _, set := range []map[string]*sandbox.Order{st.OpenOrders, st.ClosedOrders} {
			for oid, o := range set {
				cids = append(cids, o.ClientID)
				n, _ := strconv.ParseInt(oid, 10, 64)
				maxID = max(maxID, n)
			}
		}
	})
	if len(cids) != 2 {
		t.Fatalf("the restored ledger holds client IDs %v", cids)
	}
	next, _ := strconv.ParseInt(b.State.NextOrderID("binance"), 10, 64)
	if next <= maxID {
		t.Errorf("next order ID %d is not above the stored %d", next, maxID)
	}
}

// A saved file that does not fit its part stops the lab, and a part is never saved before
// it was restored: nothing is silently reset to a fresh world.
func TestRestore_CorruptFileIsAnError(t *testing.T) {
	home := t.TempDir()
	t.Cleanup(func() { sandbox.SetGlobalState(false, "") })
	a, _ := labAt(t, home)
	if err := a.Persist("exchange"); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "lab", "world", "exchange.json"), []byte(`{"venues": [`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := New(context.Background(), Options{Home: home, FixturesDir: filepath.Join(home, "workspace", "sandbox"), Token: testToken})
	if err == nil {
		t.Fatal("a corrupt exchange.json started the lab")
	}
	// A missing file is a fresh part.
	if err := os.Remove(filepath.Join(home, "lab", "world", "exchange.json")); err != nil {
		t.Fatal(err)
	}
	b, _ := labAt(t, home)
	t.Cleanup(func() { _ = b.Close() })
	l := &Lab{Home: t.TempDir()}
	l.Register(b.Market)
	if err := l.Persist("market"); err == nil {
		t.Error("a part was saved before it was restored")
	}
	if info, err := os.Stat(filepath.Join(home, "lab", "world")); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("world dir: %v, %v", info, err)
	}
}
