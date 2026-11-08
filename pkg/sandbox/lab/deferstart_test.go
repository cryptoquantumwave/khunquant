package lab

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/fault"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/market"
)

// With DeferStart the server does not exist until Start: the caller restores the rest of the
// world first, so the first request is served on a restored world.
func TestDeferStart_ServerStartsAfterRestore(t *testing.T) {
	home := t.TempDir()
	t.Cleanup(func() { sandbox.SetGlobalState(false, "") })
	a, _ := labAt(t, home)
	if err := a.Market.Set("binance", market.Pair{Base: "BTC", Quote: "USDT"}, 4242); err != nil {
		t.Fatal(err)
	}
	a.persistAll()
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}

	fixtures := filepath.Join(home, "workspace", "sandbox")
	b, err := New(context.Background(), Options{Home: home, FixturesDir: fixtures, Token: testToken, DeferStart: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	if b.Server.IsRunning() || sandbox.GetInstance() != nil {
		t.Fatal("the server runs before Start")
	}
	if px, ok := b.Market.Price("binance", market.Pair{Base: "BTC", Quote: "USDT"}, b.Now()); !ok || px != 4242 {
		t.Errorf("the market was not restored before Start: %v, %v", px, ok)
	}
	inj := fault.New(fault.Options{})
	b.Register(inj)
	if err := b.Restore(); err != nil {
		t.Fatal(err)
	}
	if err := b.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !b.Server.IsRunning() || sandbox.GetInstance() != b.Server {
		t.Error("Start did not start the server")
	}
}
