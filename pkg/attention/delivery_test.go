package attention

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

type stateSnapshot struct {
	sev   int
	sent  int64
	sends int
	js    string
}

func (e *env) state(key string) (stateSnapshot, bool) {
	e.t.Helper()
	var s stateSnapshot
	err := e.db.QueryRow(`SELECT last_severity, last_sent_ms, sends, last_candidate_json
		FROM attention_state WHERE dedup_key = ?`, key).Scan(&s.sev, &s.sent, &s.sends, &s.js)
	if err == sql.ErrNoRows {
		return s, false
	}
	if err != nil {
		e.t.Fatalf("read state: %v", err)
	}
	return s, true
}

func TestMarkDelivered_UpdatesStateAndBudget(t *testing.T) {
	e := newEnv(t)
	d := e.decide(cand("k", notice))
	expect(t, d, proactive.DispNotify, ReasonTemplate)
	if _, ok := e.state("k"); ok {
		t.Fatal("state written before delivery")
	}

	at := bkkAt(0, 12, 5)
	if err := e.g.MarkDelivered(context.Background(), d, Delivery{Delivered: true, At: at}); err != nil {
		t.Fatal(err)
	}
	s, ok := e.state("k")
	if !ok || s.sev != int(notice) || s.sends != 1 || s.sent != at.UnixMilli() || !strings.Contains(s.js, `"title k"`) {
		t.Fatalf("state = %+v ok=%v", s, ok)
	}
	var delivered int
	var deliveredAt int64
	if err := e.db.QueryRow(`SELECT delivered, delivered_at_ms FROM attention_decisions WHERE id = ?`, d.ID).Scan(&delivered, &deliveredAt); err != nil {
		t.Fatal(err)
	}
	if delivered != 1 || deliveredAt != at.UnixMilli() {
		t.Fatalf("decision row delivered=%d at=%d", delivered, deliveredAt)
	}

	// a later, higher-tier delivery updates the same row.
	e.clock.Add(time.Hour)
	d2 := e.send(cand("k", warning))
	expect(t, d2, proactive.DispNotify, ReasonTemplate)
	s, _ = e.state("k")
	if s.sev != int(warning) || s.sends != 2 {
		t.Fatalf("state after update = %+v", s)
	}

	// the delivered send counts against the budget: 2 of 3 used (k twice).
	expect(t, e.send(cand("x", notice)), proactive.DispNotify, ReasonTemplate)
	expect(t, e.decide(cand("y", notice)), proactive.DispDigest, ReasonBudget)

	// a delivered digest item is told to the owner (state) but is not a send.
	e2 := newEnv(t)
	dd := e2.decide(cand("q", info))
	expect(t, dd, proactive.DispDigest, ReasonInfoToDigest)
	e2.mark(dd, true)
	if s, ok := e2.state("q"); !ok || s.sends != 1 {
		t.Fatalf("digest delivery left state %+v ok=%v", s, ok)
	}
	for _, k := range []string{"a", "b", "c"} {
		expect(t, e2.send(cand(k, notice)), proactive.DispNotify, ReasonTemplate)
	}
}

func TestMarkDelivered_FailedSendConsumesNothing(t *testing.T) {
	e := newEnv(t)
	e.prof.set(func(p *intent.Profile) { p.NotificationBudgetPerDay = 2 })

	d := e.decide(cand("k", warning))
	expect(t, d, proactive.DispNotify, ReasonTemplate)
	e.mark(d, false)

	if _, ok := e.state("k"); ok {
		t.Fatal("a failed send wrote dedup state")
	}
	if n := e.count("SELECT COUNT(*) FROM attention_decisions WHERE delivered = 1"); n != 0 {
		t.Fatalf("failed send left %d delivered rows", n)
	}
	// retry: not deduplicated, budget intact.
	d2 := e.decide(cand("k", warning))
	expect(t, d2, proactive.DispNotify, ReasonTemplate)
	e.mark(d2, true)
	// the retry consumed exactly one of the two slots.
	expect(t, e.send(cand("n", notice)), proactive.DispNotify, ReasonTemplate)
	expect(t, e.decide(cand("n2", notice)), proactive.DispDigest, ReasonBudget)

	// a failure reported for an already-delivered decision does not undo it.
	e.mark(d2, false)
	if s, ok := e.state("k"); !ok || s.sends != 1 {
		t.Fatalf("late failure changed state: %+v ok=%v", s, ok)
	}
	// a failed attempt that is later retried successfully on the same decision still counts.
	e3 := newEnv(t)
	d3 := e3.decide(cand("z", notice))
	e3.mark(d3, false)
	e3.mark(d3, true)
	if s, ok := e3.state("z"); !ok || s.sends != 1 {
		t.Fatalf("retry on the same decision: %+v ok=%v", s, ok)
	}
	// failing a drop decision is a harmless no-op.
	d4 := e3.decide(cand("z", notice))
	expect(t, d4, proactive.DispDrop, ReasonDedupSame)
	e3.mark(d4, false)
}

func TestMarkDelivered_Idempotent(t *testing.T) {
	e := newEnv(t)
	d := e.decide(cand("k", notice))
	for i := 0; i < 3; i++ {
		e.mark(d, true)
	}
	if s, _ := e.state("k"); s.sends != 1 {
		t.Fatalf("sends = %d after 3 identical deliveries, want 1", s.sends)
	}
	if n := e.count("SELECT COUNT(*) FROM attention_decisions WHERE delivered = 1"); n != 1 {
		t.Fatalf("delivered rows = %d, want 1", n)
	}
}

func TestMarkDelivered_Errors(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if err := e.g.MarkDelivered(ctx, Decision{}, Delivery{Delivered: true}); err == nil {
		t.Error("empty decision ID accepted")
	}
	if err := e.g.MarkDelivered(ctx, Decision{ID: "nope"}, Delivery{Delivered: true}); err == nil {
		t.Error("unknown decision ID accepted")
	}
	d := e.decide(cand("k", info))
	expect(t, d, proactive.DispDigest, ReasonInfoToDigest)
	e.mark(d, true) // digest items can be delivered by the flush

	dropped := e.decide(cand("k", info))
	if err := e.g.MarkDelivered(ctx, dropped, Delivery{Delivered: true}); err == nil {
		t.Errorf("delivering a %s decision succeeded", dropped.Disposition)
	}
}

func TestDecide_ConcurrentBudgetIsNotOverspent(t *testing.T) {
	e := newEnv(t) // budget 3

	t.Run("distinct situations", func(t *testing.T) {
		e := newEnv(t)
		const n = 50
		type res struct {
			sev  proactive.Severity
			disp proactive.Disposition
		}
		out := make([]res, n)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				sev := notice
				if i%5 == 0 {
					sev = warning
				}
				<-start
				d, err := e.g.Decide(context.Background(), cand("k"+strconv.Itoa(i), sev))
				if err != nil {
					t.Errorf("Decide: %v", err)
					return
				}
				out[i] = res{sev, d.Disposition}
				if d.Disposition == proactive.DispNotify || d.Disposition == proactive.DispEscalate {
					if err := e.g.MarkDelivered(context.Background(), d, Delivery{Delivered: true}); err != nil {
						t.Errorf("MarkDelivered: %v", err)
					}
				}
			}()
		}
		close(start)
		wg.Wait()

		var notices, warnings int
		for _, r := range out {
			if r.disp != proactive.DispNotify {
				continue
			}
			if r.sev == warning {
				warnings++
			} else {
				notices++
			}
		}
		if notices > 3 {
			t.Errorf("%d notices passed a budget of 3", notices)
		}
		if notices+warnings != 4 {
			t.Errorf("%d sends passed (notices %d, warnings %d), want exactly budget+1 = 4", notices+warnings, notices, warnings)
		}
		if got := e.count("SELECT COUNT(*) FROM attention_decisions WHERE delivered = 1"); got != notices+warnings {
			t.Errorf("delivered rows = %d, want %d", got, notices+warnings)
		}
		if got := e.count("SELECT COUNT(*) FROM attention_decisions"); got != n {
			t.Errorf("decision rows = %d, want %d", got, n)
		}
	})

	t.Run("one situation", func(t *testing.T) {
		const n = 30
		var notified int
		var mu sync.Mutex
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				d, err := e.g.Decide(context.Background(), cand("same", warning))
				if err != nil {
					t.Errorf("Decide: %v", err)
					return
				}
				if d.Disposition == proactive.DispNotify {
					mu.Lock()
					notified++
					mu.Unlock()
				}
			}()
		}
		close(start)
		wg.Wait()
		if notified != 1 {
			t.Errorf("%d concurrent decisions for one situation notified, want 1", notified)
		}
	})
}
