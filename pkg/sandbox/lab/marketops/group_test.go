package marketops

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/market"
)

func TestGroup_OpsDriveTheBook(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	b := market.NewBook(market.Options{Now: func() time.Time { return now }})
	l := &lab.Lab{}
	if err := l.AddGroup(Group(b, Options{Home: t.TempDir()})); err != nil {
		t.Fatal(err) // names, ops and schemas are valid for the registry
	}
	call := func(op, args string) (string, error) {
		res, err := l.Call(context.Background(), "market", op, json.RawMessage(args))
		out, _ := json.Marshal(res)
		return string(out), err
	}
	px := func(venue string, d time.Duration) float64 {
		p, _ := b.Price(venue, market.Pair{Base: "BTC", Quote: "USDT"}, now.Add(d))
		return p
	}

	if _, err := call("set", `{"symbol":"BTC/USDT","price":100}`); err != nil {
		t.Fatal(err)
	}
	if _, err := call("path", `{"symbol":"btc/usdt","to":200,"over":"100s","shape":"step","keyframes":[{"at":"+30s","price":150}]}`); err != nil {
		t.Fatal(err)
	}
	if px("okx", 29*time.Second) != 100 || px("okx", 30*time.Second) != 150 || px("okx", 100*time.Second) != 200 {
		t.Errorf("step path with a keyframe: %v %v %v", px("okx", 29*time.Second), px("okx", 30*time.Second), px("okx", 100*time.Second))
	}
	if _, err := call("walk", `{"symbol":"BTC/USDT","venue":"binance","vol":0.01,"seed":4,"step":"30s"}`); err != nil {
		t.Fatal(err)
	}
	if px("binance", time.Hour) == px("okx", time.Hour) {
		t.Error("a binance-only walk moved okx")
	}
	out, err := call("get", `{}`)
	if err != nil || !strings.Contains(out, `"model":"walk"`) || !strings.Contains(out, `"venue":"binance"`) {
		t.Errorf("get = %s (%v)", out, err)
	}
	for _, bad := range []string{`{"symbol":"BTC/USDT","pct":"x"}`, `{"symbol":"BTC/USDT","extra":1,"pct":1}`, `{"pct":1}`} {
		var le *lab.Error
		if _, err := call("shock", bad); err == nil || !asBadArgs(err, &le) {
			t.Errorf("shock %s: err = %v, want bad_args", bad, err)
		}
	}
}

func TestGroup_DelistRelistAndReload(t *testing.T) {
	b := market.NewBook(market.Options{Now: time.Now})
	resets := 0
	l := &lab.Lab{}
	if err := l.AddGroup(Group(b, Options{Home: t.TempDir(), ResetClients: func() { resets++ }})); err != nil {
		t.Fatal(err)
	}
	call := func(op, args string) string {
		res, err := l.Call(context.Background(), "market", op, json.RawMessage(args))
		if err != nil {
			t.Fatalf("%s %s: %v", op, args, err)
		}
		out, _ := json.Marshal(res)
		return string(out)
	}
	sol := market.Pair{Base: "SOL", Quote: "USDT"}
	if out := call("delist", `{"symbol":"SOLUSDT","venue":"binance"}`); resets != 0 || !b.IsDelisted("binance", sol) ||
		!strings.Contains(out, `"delisted":[{"venue":"binance","symbol":"SOL/USDT"}]`) {
		t.Errorf("delist = %s (resets %d, delisted %v)", out, resets, b.IsDelisted("binance", sol))
	}
	if b.IsDelisted("okx", sol) {
		t.Error("a binance delist reached okx")
	}
	if out := call("get", `{}`); !strings.Contains(out, `"delisted":[{`) {
		t.Errorf("get does not list the delisted symbols: %s", out)
	}
	call("relist", `{"symbol":"SOL/USDT","venue":"binance","reload_markets":true}`)
	if resets != 1 || b.IsDelisted("binance", sol) {
		t.Errorf("relist with reload_markets: resets %d, delisted %v", resets, b.IsDelisted("binance", sol))
	}
	call("delist", `{"symbol":"SOL/USDT","reload_markets":true}`) // the wildcard: every venue
	if resets != 2 || !b.IsDelisted("okx", sol) || !b.IsDelisted("binance", sol) {
		t.Errorf("wildcard delist: resets %d", resets)
	}
	if _, err := l.Call(context.Background(), "market", "delist", json.RawMessage(`{"symbol":"SOL/USDT","venue":"kraken"}`)); err == nil {
		t.Error("an unknown venue was accepted")
	}
}

func TestGroup_SetFundingRate(t *testing.T) {
	b := market.NewBook(market.Options{Now: time.Now})
	l := &lab.Lab{}
	if err := l.AddGroup(Group(b, Options{Home: t.TempDir()})); err != nil {
		t.Fatal(err)
	}
	call := func(args string) error {
		_, err := l.Call(context.Background(), "market", "set", json.RawMessage(args))
		return err
	}
	pair := market.Pair{Base: "BTC", Quote: "USDT"}
	if err := call(`{"symbol":"BTC/USDT","price":100,"funding_rate":0.0003}`); err != nil {
		t.Fatal(err)
	}
	if got := b.Funding("okx", pair); got != 0.0003 {
		t.Errorf("funding = %v, want 0.0003", got)
	}
	if err := call(`{"symbol":"BTC/USDT","venue":"binance","funding_rate":-0.001}`); err != nil { // the rate alone
		t.Fatal(err)
	}
	if b.Funding("binance", pair) != -0.001 || b.Funding("okx", pair) != 0.0003 {
		t.Errorf("a venue's rate leaked: binance %v okx %v", b.Funding("binance", pair), b.Funding("okx", pair))
	}
	if p, _ := b.Price("binance", pair, time.Now()); p != 100 {
		t.Errorf("setting only the rate moved the price to %v", p)
	}
	for _, bad := range []string{`{"symbol":"BTC/USDT"}`, `{"symbol":"BTC/USDT","funding_rate":5}`} {
		if err := call(bad); err == nil {
			t.Errorf("%s was accepted", bad)
		}
	}
	if b.Funding("okx", market.Pair{Base: "ETH", Quote: "USDT"}) != market.DefaultFunding {
		t.Error("an unset pair lost the default rate")
	}
}

func asBadArgs(err error, le **lab.Error) bool {
	e, ok := err.(*lab.Error)
	*le = e
	return ok && e.Code == lab.CodeBadArgs
}

func TestGroup_ReplayCSVPathStaysInsideHome(t *testing.T) {
	now := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	b := market.NewBook(market.Options{Now: func() time.Time { return now }})
	home, outside := t.TempDir(), t.TempDir()
	l := &lab.Lab{}
	if err := l.AddGroup(Group(b, Options{Home: home})); err != nil {
		t.Fatal(err)
	}
	const csv = "ts,open,high,low,close\n1700000000,100,110,95,105\n"
	for dir, name := range map[string]string{home: "in.csv", outside: "out.csv"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(csv), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(home, "link")); err != nil {
		t.Fatal(err)
	}
	call := func(args string) error {
		_, err := l.Call(context.Background(), "market", "replay", json.RawMessage(args))
		return err
	}
	for _, ok := range []string{
		`{"symbol":"BTC/USDT","timeframe":"1m","csv_path":"in.csv"}`,
		`{"symbol":"BTC/USDT","timeframe":"1m","csv_path":"` + filepath.Join(home, "in.csv") + `"}`,
		`{"symbol":"BTC/USDT","timeframe":"1m","csv_inline":"1700000000,1,2,1,2"}`,
	} {
		if err := call(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	if p, _ := b.Price("binance", market.Pair{Base: "BTC", Quote: "USDT"}, now); p <= 0 {
		t.Errorf("replay left no price: %v", p)
	}
	for _, bad := range []string{
		`{"symbol":"BTC/USDT","timeframe":"1m","csv_path":"` + filepath.Join(outside, "out.csv") + `"}`,
		`{"symbol":"BTC/USDT","timeframe":"1m","csv_path":"../x.csv"}`,
		`{"symbol":"BTC/USDT","timeframe":"1m","csv_path":"link/out.csv"}`,
		`{"symbol":"BTC/USDT","timeframe":"1m"}`,
		`{"symbol":"BTC/USDT","timeframe":"1m","csv_path":"in.csv","csv_inline":"x"}`,
		`{"symbol":"BTC/USDT","timeframe":"soon","csv_path":"in.csv"}`,
	} {
		if err := call(bad); err == nil {
			t.Errorf("%s was accepted", bad)
		}
	}
}
