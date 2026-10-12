package sources

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/perception/perceptiontest"
)

type coded int

func (c coded) Error() string   { return "boom" }
func (c coded) StatusCode() int { return int(c) }

func TestIsRateLimitErr(t *testing.T) {
	yes := []error{
		ErrRateLimited,
		fmt.Errorf("x: %w", ErrRateLimited),
		coded(429), coded(418),
		ccxt.RateLimitExceeded("slow down"), ccxt.DDoSProtection("ddos"),
		errors.New("HTTP 429 Too Many Requests"),
		errors.New("binance GET https://api/ticker: status 418"),
		errors.New("Too many requests; current limit is 1200"),
		errors.New("okx: code=50011 rate limit reached"),
		errors.New("Your IP has been auto-banned for scraping"),
	}
	for _, e := range yes {
		if !isRateLimitErr(e) {
			t.Errorf("%v is not recognised as pushback", e)
		}
	}
	no := []error{
		nil, errors.New("connection refused"), coded(500), coded(404),
		ccxt.BadSymbol("BTC/XYZ"), errors.New("price 14180.5 out of range"), errors.New("timeout after 4290ms"),
	}
	for _, e := range no {
		if isRateLimitErr(e) {
			t.Errorf("%v is taken for pushback", e)
		}
	}
}

func TestIsNotSupportedErr(t *testing.T) {
	for _, e := range []error{ccxt.NotSupported("x"), errors.New("fetchTickers is not supported"), errors.New("unsupported")} {
		if !isNotSupportedErr(e) {
			t.Errorf("%v not recognised", e)
		}
	}
	for _, e := range []error{nil, errors.New("timeout"), ccxt.BadSymbol("x")} {
		if isNotSupportedErr(e) {
			t.Errorf("%v taken for unsupported", e)
		}
	}
}

func TestTimeframeDuration(t *testing.T) {
	ok := map[string]time.Duration{"1m": time.Minute, "15m": 15 * time.Minute, "4h": 4 * time.Hour, "1d": 24 * time.Hour, "1w": 7 * 24 * time.Hour, "1M": 30 * 24 * time.Hour}
	for in, want := range ok {
		if got, good := timeframeDuration(in); !good || got != want {
			t.Errorf("%q = %v %v", in, got, good)
		}
	}
	for _, in := range []string{"", "m", "0m", "-1m", "1x", "abc", "99999m", "1.5h"} {
		if _, good := timeframeDuration(in); good {
			t.Errorf("%q parsed", in)
		}
	}
}

func TestErrLabelIsBoundedAndPrintable(t *testing.T) {
	got := errLabel(errors.New("bad\x00thing\n" + strings.Repeat("é", 500)))
	if len([]rune(got)) > 200 || strings.ContainsAny(got, "\x00\n") {
		t.Fatalf("label = %q", got)
	}
}

func TestReadLimiter_RefillsWithTheClockAndNeverBlocks(t *testing.T) {
	clk := perceptiontest.NewClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	l := newReadLimiter(clk, 3)
	for i := 0; i < 3; i++ {
		if !l.Allow("binance") {
			t.Fatalf("token %d refused", i)
		}
	}
	if l.Allow("binance") {
		t.Fatal("a fourth token in the same instant")
	}
	if !l.Allow("okx") {
		t.Fatal("providers share a bucket")
	}
	clk.Advance(20 * time.Second) // one token a 20 s
	if l.Left("binance") != 1 || !l.Allow("binance") || l.Allow("binance") {
		t.Fatal("refill is not continuous")
	}
	clk.Advance(time.Hour)
	if l.Left("binance") != 3 {
		t.Fatalf("left = %d, want the cap of 3", l.Left("binance"))
	}
	if newReadLimiter(clk, 0).perMin != DefaultReadsPerMinute {
		t.Fatal("no default budget")
	}
}

func TestBackoffDelayDoublesToTheCap(t *testing.T) {
	s := NewMarketSource(nil, nil, MarketOptions{PollInterval: time.Minute})
	var got []time.Duration
	for n := 1; n <= 7; n++ {
		got = append(got, s.backoffDelay(n))
	}
	want := []int{2, 4, 8, 15, 15, 15, 15}
	for i := range want {
		if got[i] != time.Duration(want[i])*time.Minute {
			t.Fatalf("delays = %v", got)
		}
	}
}
