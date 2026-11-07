package lab

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/market"
)

// A group op changes the world before it answers, so the world is saved before the
// answer: a price set through a group is in market.json when the call returns.
func TestCall_SavesTheWorldBeforeItAnswers(t *testing.T) {
	home := t.TempDir()
	t.Cleanup(func() { sandbox.SetGlobalState(false, "") })
	l, _ := labAt(t, home)
	t.Cleanup(func() { _ = l.Close() })
	err := l.AddGroup(Group{Name: "probe", Ops: []Op{{Name: "price", Run: func(context.Context, json.RawMessage) (any, error) {
		return nil, l.Market.Set("binance", market.Pair{Base: "SOL", Quote: "USDT"}, 321)
	}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Call(context.Background(), "probe", "price", nil); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(home, "lab", "world", "market.json"))
	if err != nil || !strings.Contains(string(b), "321") {
		t.Fatalf("market.json after the call: %q, %v", b, err)
	}
}
