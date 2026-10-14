package attention

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

func TestInterventionKeys(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)

	deliver := func(c proactive.Candidate, intervention string) Decision {
		d := e.decide(c)
		if d.Disposition != proactive.DispNotify {
			t.Fatalf("%s was %s/%s, want notify", c.DedupKey, d.Disposition, d.Reason)
		}
		if err := e.g.MarkDelivered(ctx, d, Delivery{Delivered: true, InterventionID: intervention}); err != nil {
			t.Fatal(err)
		}
		return d
	}

	// Two decisions delivered together in one message, one delivered alone.
	deliver(cand("price:btc", warning), "MSG1")
	e.clock.Add(time.Minute)
	deliver(cand("margin:okx", warning), "MSG1")
	alone := deliver(cand("news:eth", warning), "")

	got, err := e.g.InterventionKeys(ctx, "MSG1")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"price:btc", "margin:okx"}; !reflect.DeepEqual(got, want) {
		t.Errorf("keys of the shared message = %v, want %v", got, want)
	}

	// A decision delivered on its own is its own message: its decision id names it.
	if got, _ := e.g.InterventionKeys(ctx, alone.ID); !reflect.DeepEqual(got, []string{"news:eth"}) {
		t.Errorf("keys of a lone decision = %v", got)
	}

	for _, id := range []string{"", "unknown", "msg1"} {
		if got, err := e.g.InterventionKeys(ctx, id); err != nil || len(got) != 0 {
			t.Errorf("InterventionKeys(%q) = %v, %v; want nothing", id, got, err)
		}
	}

	// A decision that was never delivered says nothing about any message.
	undelivered := e.decide(cand("undelivered", warning))
	if got, _ := e.g.InterventionKeys(ctx, undelivered.ID); len(got) != 0 {
		t.Errorf("an undelivered decision answered %v", got)
	}
}

// A snooze is an exact mute: it silences the one dedup key, not the keys that merely
// start with it, until its deadline. A prefix mute (/mute) still silences the family.
func TestMuteExact(t *testing.T) {
	ctx := context.Background()
	const (
		six    = "price_cross:binance:BTC/USDT:above:6"
		sixty5 = "price_cross:binance:BTC/USDT:above:65000"
	)

	t.Run("silences that key and not one it is a prefix of", func(t *testing.T) {
		e := newEnv(t)
		if err := e.g.MuteExact(ctx, six, e.clock.Now().Add(4*time.Hour), "snoozed"); err != nil {
			t.Fatal(err)
		}
		expect(t, e.decide(cand(sixty5, warning)), proactive.DispNotify, ReasonTemplate)
		expect(t, e.decide(cand("price_cross:binance:BTC/USDT:above:", warning)), proactive.DispNotify, ReasonTemplate)
		expect(t, e.decide(cand(six, warning)), proactive.DispDrop, ReasonMuted)
		// ... until the deadline.
		e.clock.Add(3*time.Hour + 59*time.Minute)
		expect(t, e.decide(cand(six, warning)), proactive.DispDrop, ReasonMuted)
		e.clock.Add(2 * time.Minute)
		expect(t, e.decide(cand(six, warning)), proactive.DispNotify, ReasonTemplate)
		if n := e.count("SELECT COUNT(*) FROM attention_mutes"); n != 0 {
			t.Errorf("%d mute rows after expiry", n)
		}
	})

	t.Run("never matches a trigger id", func(t *testing.T) {
		e := newEnv(t)
		if err := e.g.MuteExact(ctx, "price_cross", e.clock.Now().Add(time.Hour), ""); err != nil {
			t.Fatal(err)
		}
		expect(t, e.decide(cand("other-key", warning, withTrigger("price_cross"))), proactive.DispNotify, ReasonTemplate)
	})

	t.Run("a prefix mute of the same text is a different mute", func(t *testing.T) {
		e := newEnv(t)
		until := e.clock.Now().Add(time.Hour)
		if err := e.g.Mute(ctx, six, until, "family"); err != nil {
			t.Fatal(err)
		}
		if err := e.g.MuteExact(ctx, six, until, "snooze"); err != nil {
			t.Fatal(err)
		}
		if n := e.count("SELECT COUNT(*) FROM attention_mutes"); n != 2 {
			t.Fatalf("%d mute rows, want 2", n)
		}
		expect(t, e.decide(cand(sixty5, warning)), proactive.DispDrop, ReasonMuted) // the prefix one
		if err := e.g.Unmute(ctx, six); err != nil {
			t.Fatal(err)
		}
		expect(t, e.decide(cand(sixty5, warning)), proactive.DispNotify, ReasonTemplate)
		expect(t, e.decide(cand(six, warning)), proactive.DispDrop, ReasonMuted) // the exact one stays
	})

	t.Run("/mute of a kind still silences the family", func(t *testing.T) {
		e := newEnv(t)
		if err := e.g.Mute(ctx, "price_cross", e.clock.Now().Add(30*24*time.Hour), "muted"); err != nil {
			t.Fatal(err)
		}
		expect(t, e.decide(cand(six, warning)), proactive.DispDrop, ReasonMuted)
		expect(t, e.decide(cand(sixty5, warning)), proactive.DispDrop, ReasonMuted)
	})

	t.Run("a protective critical is hidden only by its own exact key", func(t *testing.T) {
		e := newEnv(t)
		if err := e.g.MuteExact(ctx, "dn:12:liq", e.clock.Now().Add(time.Hour), ""); err != nil {
			t.Fatal(err)
		}
		expect(t, e.decide(cand("dn:12:liq:2", critical, protective)), proactive.DispNotify, ReasonProtective)
		expect(t, e.decide(cand("dn:12:liq", critical, protective)), proactive.DispDrop, ReasonMuted)
	})

	t.Run("validation", func(t *testing.T) {
		e := newEnv(t)
		if err := e.g.MuteExact(ctx, "  ", e.clock.Now().Add(time.Hour), ""); err == nil {
			t.Error("an empty key was accepted")
		}
		if err := e.g.MuteExact(ctx, "k", e.clock.Now(), ""); err == nil {
			t.Error("a deadline that is not in the future was accepted")
		}
	})
}

// The outbox forwards to its gate, and says so when the gate cannot.
func TestOutbox_MuteForwards(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	ob := NewOutbox(e.g, nopDeliverer{}, 0, e.clock.Now)

	if err := ob.Mute(ctx, "dn:", e.clock.Now().Add(time.Hour), "owner"); err != nil {
		t.Fatal(err)
	}
	expect(t, e.decide(cand("dn:12", warning)), proactive.DispDrop, ReasonMuted)
	if err := ob.Unmute(ctx, "dn:"); err != nil {
		t.Fatal(err)
	}
	expect(t, e.decide(cand("dn:13", warning)), proactive.DispNotify, ReasonTemplate)
	if _, err := ob.InterventionKeys(ctx, "x"); err != nil {
		t.Errorf("InterventionKeys through the outbox: %v", err)
	}

	bare := NewOutbox(gateOnly{}, nopDeliverer{}, 0, e.clock.Now)
	if err := bare.Mute(ctx, "dn:", e.clock.Now().Add(time.Hour), ""); !errors.Is(err, ErrNoMutes) {
		t.Errorf("Mute on a gate without mutes: %v", err)
	}
	if err := bare.MuteExact(ctx, "dn:12", e.clock.Now().Add(time.Hour), ""); !errors.Is(err, ErrNoMutes) {
		t.Errorf("MuteExact on a gate without mutes: %v", err)
	}
	if err := ob.MuteExact(ctx, "dn:99", e.clock.Now().Add(time.Hour), ""); err != nil {
		t.Errorf("MuteExact through the outbox: %v", err)
	}
	if err := bare.Unmute(ctx, "dn:"); !errors.Is(err, ErrNoMutes) {
		t.Errorf("Unmute on a gate without mutes: %v", err)
	}
	if _, err := bare.InterventionKeys(ctx, "x"); !errors.Is(err, ErrNoDecisionLog) {
		t.Errorf("InterventionKeys on a gate without a log: %v", err)
	}
}

type nopDeliverer struct{}

func (nopDeliverer) Deliver(context.Context, Message) (bool, error) { return true, nil }

// gateOnly is a Gate that has no mutes and no decision log.
type gateOnly struct{}

func (gateOnly) Decide(context.Context, proactive.Candidate) (Decision, error) {
	return Decision{}, nil
}
func (gateOnly) MarkDelivered(context.Context, Decision, Delivery) error { return nil }
func (gateOnly) PendingDigest(context.Context) (*Digest, error)          { return nil, nil }
func (gateOnly) CompleteDigest(context.Context, *Digest, Delivery) error { return nil }
