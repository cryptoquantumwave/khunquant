package marketops

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/market"
)

const recJSONL = `{"clock":"2026-03-10T05:00:00Z","event":{"id":"a","kind":"perception.market.tick","time":"2026-03-10T05:00:00Z","payload":{"provider":"okx","symbol":"BTC/USDT","last":70000}}}
{"clock":"2026-03-10T05:05:00Z","event":{"id":"b","kind":"perception.market.tick","time":"2026-03-10T05:05:00Z","payload":{"provider":"okx","symbol":"BTC/USDT","last":55000}}}
`

// The recording op reads only inside the instance home, as replay's csv_path does.
func TestGroup_RecordingPathStaysInsideHome(t *testing.T) {
	now := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	home, outside := t.TempDir(), t.TempDir()
	b := market.NewBook(market.Options{Now: func() time.Time { return now }, Home: home})
	l := &lab.Lab{}
	if err := l.AddGroup(Group(b, Options{Home: home})); err != nil {
		t.Fatal(err)
	}
	for dir, name := range map[string]string{home: "in.jsonl", outside: "out.jsonl"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(recJSONL), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(home, "link")); err != nil {
		t.Fatal(err)
	}
	call := func(args string) error {
		_, err := l.Call(context.Background(), "market", "recording", json.RawMessage(args))
		return err
	}
	pair := market.Pair{Base: "BTC", Quote: "USDT"}
	if err := call(`{"symbol":"BTC/USDT","venue":"okx","path":"in.jsonl","align":"start"}`); err != nil {
		t.Fatal(err)
	}
	if p, _ := b.Price("okx", pair, now.Add(6*time.Minute)); p != 55000 {
		t.Errorf("price 6m after the start = %v, want the recording's 55000", p)
	}
	if p, ok := b.Price("binance", pair, now); ok {
		t.Errorf("a recording for okx priced binance: %v", p)
	}
	if err := call(`{"symbol":"BTC/USDT","path":"` + filepath.Join(home, "in.jsonl") + `","speed":2,"interpolate":"linear"}`); err != nil {
		t.Errorf("an absolute path inside the home: %v", err)
	}
	for _, bad := range []string{
		`{"symbol":"BTC/USDT","path":"` + filepath.Join(outside, "out.jsonl") + `"}`,
		`{"symbol":"BTC/USDT","path":"../x.jsonl"}`,
		`{"symbol":"BTC/USDT","path":"link/out.jsonl"}`,
		`{"symbol":"BTC/USDT","path":"missing.jsonl"}`,
		`{"symbol":"BTC/USDT"}`,
		`{"symbol":"BTC/USDT","path":"in.jsonl","align":"soon"}`,
		`{"symbol":"BTC/USDT","path":"in.jsonl","interpolate":"cubic"}`,
		`{"symbol":"BTC/USDT","path":"in.jsonl","source":"binance:BTC/USDT"}`,
		`{"symbol":"BTC/USDT","path":"in.jsonl","pathh":"x"}`,
	} {
		if err := call(bad); err == nil {
			t.Errorf("%s was accepted", bad)
		}
	}
}
