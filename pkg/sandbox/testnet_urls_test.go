package sandbox_test

import (
	"testing"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

// A scenario can patch exchanges.<venue>.testnet to true, which switches ccxt to the
// venue's testnet hosts. The lab rewrites every URL ccxt holds, so the testnet ones go to the
// lab server too and the egress guard has nothing left to refuse.
func TestTestnetHostsAreRewrittenToo(t *testing.T) {
	for venue, ex := range map[string]interface {
		SetSandboxMode(on any)
	}{
		"binance": ccxt.NewBinance(map[string]any{}),
		"okx":     ccxt.NewOkx(map[string]any{}),
	} {
		ex.SetSandboxMode(true)
		if err := sandbox.RewriteExchangeURLs(venue, ex, "http://127.0.0.1:9"); err != nil {
			t.Fatalf("%s: rewrite: %v", venue, err)
		}
		if err := sandbox.VerifyExchangeURLsSandboxed(ex); err != nil {
			t.Errorf("%s: a testnet host survives the rewrite: %v", venue, err)
		}
	}
}
