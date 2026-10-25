package sources_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/sources"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

func healthEvents(evs []events.Event) []events.Event {
	var out []events.Event
	for _, e := range evs {
		if e.Kind == events.KindPerceptionSourceHealth {
			out = append(out, e)
		}
	}
	return out
}

// An account that had open positions and can no longer be read must not go quiet,
// and is looked at again sooner than a flat one.
func TestFuturesSource_AFailingAccountWithPositionsIsReportedAndRetriedSooner(t *testing.T) {
	fp := &fakeFutures{positions: []ccxt.Position{position("BTC/USDT:USDT", "long", 5)}}
	r := newFuturesRig(t, oneAccount(fp))
	waitFor(t, "the first sample", func() bool { return len(r.out.events()) == 1 })
	if len(healthEvents(r.out.events())) != 0 {
		t.Fatal("a health event before anything failed")
	}

	fp.set(nil, errors.New("503 from exchange: secret-detail"))
	reads := func() int { n, _ := fp.counts(); return n }
	failures := 0
	poll := func(after time.Duration, wantReads int) {
		t.Helper()
		r.advance(t, after)
		waitFor(t, "the poll", func() bool { return reads() == wantReads })
		failures++
		waitFor(t, "the degraded event", func() bool { return len(healthEvents(r.out.events())) == failures })
	}
	poll(2*time.Minute, 2) // fails; next read in 4 minutes
	poll(4*time.Minute, 3) // fails; the next read would be in 8, but the cap is 5

	// 4 minutes later: not yet. One more minute: read.
	r.advance(t, 4*time.Minute)
	time.Sleep(20 * time.Millisecond)
	if reads() != 3 {
		t.Fatalf("read again after 4 minutes of a 5-minute capped backoff (%d reads)", reads())
	}
	poll(time.Minute, 4)

	// And the cap holds.
	poll(5*time.Minute, 5)

	for _, e := range healthEvents(r.out.events()) {
		p, ok := e.Payload.(sources.SourceHealthPayload)
		if !ok || p.Source != "futures" || p.State != "degraded" || len(p.FailedAccounts) != 1 || p.FailedAccounts[0] != "okx/main" {
			t.Fatalf("payload = %#v", e.Payload)
		}
		if e.Attrs["subject"] != "okx/main" || e.Severity != events.SeverityWarn || e.TrustLevel() != proactive.TrustSystem {
			t.Errorf("event = %+v", e)
		}
		raw, _ := json.Marshal(e)
		if strings.Contains(string(raw), "secret-detail") || strings.Contains(string(raw), "503") {
			t.Errorf("exchange text in the event: %s", raw)
		}
	}
	if st := r.src.Stats(); st.Degraded != 4 {
		t.Errorf("degraded = %d, want 4", st.Degraded)
	}

	// Recovery: positions come back; no further health events.
	fp.set([]ccxt.Position{position("BTC/USDT:USDT", "long", 5)}, nil)
	r.advance(t, 5*time.Minute)
	waitFor(t, "the recovered sample", func() bool { return r.src.Health().ConsecutiveErrors == 0 })
	if n := len(healthEvents(r.out.events())); n != 4 {
		t.Errorf("%d health events after recovery, want 4", n)
	}
}

func TestFuturesSource_ABadSampleFromAnAccountWithPositionsIsReportedToo(t *testing.T) {
	good := position("BTC/USDT:USDT", "long", 5)
	fp := &fakeFutures{positions: []ccxt.Position{good}}
	r := newFuturesRig(t, oneAccount(fp))
	waitFor(t, "the first sample", func() bool { return len(r.out.events()) == 1 })

	bad := position("BTC/USDT:USDT", "long", 5)
	bad.MarkPrice = fptr(-1)
	fp.set([]ccxt.Position{bad}, nil)
	r.advance(t, 2*time.Minute)
	waitFor(t, "the degraded event", func() bool { return len(healthEvents(r.out.events())) == 1 })
	if n := len(r.out.events()); n != 2 {
		t.Fatalf("%d events, want the first sample and the degraded signal", n)
	}
}

// A flat account, or one never read, that fails stays log-and-health only, and its
// backoff is the usual one (not the 5-minute cap).
func TestFuturesSource_AFailingFlatOrNeverReadAccountStaysQuiet(t *testing.T) {
	t.Run("never read", func(t *testing.T) {
		fp := &fakeFutures{err: errors.New("down")}
		r := newFuturesRig(t, oneAccount(fp))
		waitFor(t, "first failure", func() bool { return r.src.Health().ConsecutiveErrors == 1 })
		r.advance(t, 4*time.Minute)
		waitFor(t, "second failure", func() bool { return r.src.Health().ConsecutiveErrors == 2 })
		if n := len(r.out.events()); n != 0 {
			t.Fatalf("%d events from an account that was never read", n)
		}
	})
	t.Run("flat", func(t *testing.T) {
		fp := &fakeFutures{}
		r := newFuturesRig(t, oneAccount(fp))
		waitFor(t, "the flat sample", func() bool { return len(r.out.events()) == 1 })
		fp.set(nil, errors.New("down"))
		r.advance(t, 15*time.Minute)
		waitFor(t, "failure", func() bool { return r.src.Health().ConsecutiveErrors == 1 })
		// A flat account backs off from 15 minutes: the cap for open positions (5)
		// does not apply, and the next read is not five minutes away.
		r.advance(t, 5*time.Minute)
		time.Sleep(20 * time.Millisecond)
		if n, _ := fp.counts(); n != 2 {
			t.Fatalf("a flat account was read again after 5 minutes (%d reads)", n)
		}
		if n := len(healthEvents(r.out.events())); n != 0 {
			t.Fatalf("%d health events from a flat account", n)
		}
	})
}
