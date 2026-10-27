package marketops

import (
	"context"
	"encoding/json"
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
	if err := l.AddGroup(Group(b)); err != nil {
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

func asBadArgs(err error, le **lab.Error) bool {
	e, ok := err.(*lab.Error)
	*le = e
	return ok && e.Code == lab.CodeBadArgs
}
