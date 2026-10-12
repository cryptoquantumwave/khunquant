package broker

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	ccxt "github.com/ccxt/ccxt/go/v4"
)

// fakeNow is a settable clock for the cache tests.
type fakeNow struct {
	mu sync.Mutex
	t  time.Time
}

func (f *fakeNow) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *fakeNow) advance(d time.Duration) {
	f.mu.Lock()
	f.t = f.t.Add(d)
	f.mu.Unlock()
}

func newFakeNow() *fakeNow { return &fakeNow{t: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)} }

func TestCache_KeyFix(t *testing.T) {
	if got, want := ohlcvCacheKey("BTC/USDT", "1h", 500), "BTC/USDT|1h|500"; got != want {
		t.Fatalf("key = %q, want %q", got, want)
	}
	// string(rune(n)) mapped every n that is not a valid code point (a surrogate,
	// or beyond U+10FFFF) to U+FFFD: these limits all shared one key.
	seen := map[string]int{}
	for _, limit := range []int{0xD800, 0xD801, 0xDFFF, 0x110000, 0x110001, -1, -2} {
		k := ohlcvCacheKey("ETH/USDT", "1m", limit)
		if prev, dup := seen[k]; dup {
			t.Fatalf("limits %d and %d share the key %q", prev, limit, k)
		}
		seen[k] = limit
	}
	// and no key contains a control character for an ordinary limit.
	for limit := 1; limit <= 500; limit++ {
		k := ohlcvCacheKey("X/Y", "5m", limit)
		if strings.ContainsFunc(k, func(r rune) bool { return r < 0x20 }) {
			t.Fatalf("key for limit %d has a control character: %q", limit, k)
		}
	}
	// two limits for the same series are two entries.
	c := newOHLCVCache()
	c.set("BTC/USDT", "1h", 3, []ccxt.OHLCV{{Close: 1}})
	c.set("BTC/USDT", "1h", 4, []ccxt.OHLCV{{Close: 2}})
	a, _ := c.get("BTC/USDT", "1h", 3)
	b, _ := c.get("BTC/USDT", "1h", 4)
	if len(a) != 1 || len(b) != 1 || a[0].Close != 1 || b[0].Close != 2 {
		t.Fatalf("entries mixed up: %v %v", a, b)
	}
}

func TestCache_Eviction(t *testing.T) {
	clk := newFakeNow()
	c := newTTLLRU[int](3, clk.now)

	c.set("a", 1, time.Hour)
	c.set("b", 2, time.Hour)
	c.set("c", 3, time.Hour)
	if _, ok := c.get("a"); !ok { // a becomes the most recently used
		t.Fatal("a missing")
	}
	c.set("d", 4, time.Hour) // evicts b, the least recently used
	if c.len() != 3 {
		t.Fatalf("len = %d, want 3", c.len())
	}
	if _, ok := c.get("b"); ok {
		t.Fatal("b survived: eviction is not least-recently-used")
	}
	for _, k := range []string{"a", "c", "d"} {
		if _, ok := c.get(k); !ok {
			t.Fatalf("%s was evicted", k)
		}
	}
	if got := c.evicted.Load(); got != 1 {
		t.Fatalf("evicted = %d, want 1", got)
	}

	// overwriting a key does not evict anything.
	c.set("a", 10, time.Hour)
	if v, _ := c.get("a"); v != 10 || c.len() != 3 || c.evicted.Load() != 1 {
		t.Fatalf("overwrite: value %d len %d evicted %d", v, c.len(), c.evicted.Load())
	}

	// a flood never holds more than the cap.
	for i := 0; i < 500; i++ {
		c.set(fmt.Sprintf("k%d", i), i, time.Hour)
		if c.len() > 3 {
			t.Fatalf("len %d exceeds the cap", c.len())
		}
	}
}

func TestCache_DefaultCapIs1000(t *testing.T) {
	tc := newTickerCache()
	for i := 0; i < CacheMaxEntries+250; i++ {
		tc.set(fmt.Sprintf("S%d/USDT", i), ccxt.Ticker{})
	}
	if n := tc.c.len(); n != CacheMaxEntries {
		t.Fatalf("ticker cache holds %d entries, want %d", n, CacheMaxEntries)
	}
	oc := newOHLCVCache()
	for i := 0; i < CacheMaxEntries+250; i++ {
		oc.set("BTC/USDT", "1m", i+1, nil)
	}
	if n := oc.c.len(); n != CacheMaxEntries {
		t.Fatalf("ohlcv cache holds %d entries, want %d", n, CacheMaxEntries)
	}
}

func TestCache_SweepDropsExpiredEntries(t *testing.T) {
	clk := newFakeNow()
	c := newTTLLRU[int](100, clk.now)
	for i := 0; i < 10; i++ {
		c.set(fmt.Sprintf("short%d", i), i, time.Minute)
	}
	c.set("long", 1, time.Hour)
	if c.len() != 11 {
		t.Fatalf("len = %d", c.len())
	}

	// expired but the sweep is not due yet: they are still held, a read drops only
	// the one it touches.
	clk.advance(2 * time.Minute)
	if _, ok := c.get("short0"); ok {
		t.Fatal("an expired entry was served")
	}
	if c.len() != 10 {
		t.Fatalf("len = %d after reading one expired entry, want 10", c.len())
	}

	// once CacheSweepEvery has passed, the next access sweeps the rest.
	clk.advance(CacheSweepEvery)
	if _, ok := c.get("long"); !ok {
		t.Fatal("the long entry was swept")
	}
	if c.len() != 1 {
		t.Fatalf("len = %d after the sweep, want 1", c.len())
	}
	if got := c.swept.Load(); got != 9 {
		t.Fatalf("swept = %d, want 9", got)
	}
}

func TestCache_ConcurrentUse(t *testing.T) {
	c := newTTLLRU[int](50, nil)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				k := fmt.Sprintf("k%d", (i*7+g)%120)
				c.set(k, i, time.Millisecond)
				c.get(k)
			}
		}(g)
	}
	wg.Wait()
	if c.len() > 50 {
		t.Fatalf("len %d exceeds the cap", c.len())
	}
}
