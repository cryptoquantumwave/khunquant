package dca

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The owner's autonomy setting for mandated buys (T2) is checked by dca.execute itself.

func (p *xproc) setDial(f func(context.Context) (string, error)) { p.deps.OwnerDial = f }

func constDial(s string) func(context.Context) (string, error) {
	return func(context.Context) (string, error) { return s, nil }
}

// default: today's behaviour, byte for byte (the existing tests cover the rest).
func TestOwnerDial_T2_DefaultBuys(t *testing.T) {
	for _, s := range []string{"default", ""} {
		w := newXWorld(t, true, monday)
		p := w.newProc()
		p.setDial(constDial(s))
		m := w.mandate(p.store, nil)
		p.run(m, monday)
		if w.ex.orderCount() != 1 || w.asker.count() != 0 {
			t.Fatalf("%q: orders %d, asks %d; want 1 and 0", s, w.ex.orderCount(), w.asker.count())
		}
	}
}

// ask: every run becomes the outside-the-mandate question; no order is placed, and the
// question is made once per slot.
func TestOwnerDial_T2_AskMakesAProposalAndNoOrder(t *testing.T) {
	w := newXWorld(t, true, monday)
	p := w.newProc()
	p.setDial(constDial("ask"))
	m := w.mandate(p.store, nil)
	p.run(m, monday)
	p.run(m, monday) // the same slot again
	if w.ex.orderCount() != 0 {
		t.Fatalf("%d orders were placed with the dial on ask", w.ex.orderCount())
	}
	if w.asker.count() != 1 || !strings.Contains(w.asker.asked[0], "autonomy setting") {
		t.Fatalf("asks = %v, want one that names the owner's setting", w.asker.asked)
	}
	if len(w.execs(p.store)) != 0 || w.usage(p.store, m) != 0 {
		t.Error("a question recorded an execution or usage")
	}
	// The next week asks again: the setting is standing.
	next := monday.AddDate(0, 0, 7)
	w.clock.Advance(7 * 24 * time.Hour)
	p.run(m, next)
	if w.asker.count() != 2 || w.ex.orderCount() != 0 {
		t.Fatalf("next week: asks %d, orders %d", w.asker.count(), w.ex.orderCount())
	}
}

// An owner-dial question must not move the price-move baseline.
func TestOwnerDial_T2_AskDoesNotResetThePriceBaseline(t *testing.T) {
	w := newXWorld(t, true, monday)
	p := w.newProc()
	p.setDial(constDial("ask"))
	m := w.mandate(p.store, nil)
	p.run(m, monday)
	if _, _, ok, err := p.store.LastPriceAsk(context.Background(), w.planID); err != nil || ok {
		t.Fatalf("an owner-dial question counted as a price-move ask: ok=%v err=%v", ok, err)
	}
}

// notify and off: no order, no proposal, a "skipped" row that says why, and ONE notice
// a day per plan.
func TestOwnerDial_T2_NotifyAndOffSkipWithOneNoticeADay(t *testing.T) {
	for _, setting := range []string{"notify", "off"} {
		t.Run(setting, func(t *testing.T) {
			w := newXWorld(t, true, monday)
			p := w.newProc()
			p.setDial(constDial(setting))
			m := w.mandate(p.store, nil)
			p.run(m, monday)
			if w.ex.orderCount() != 0 || w.asker.count() != 0 {
				t.Fatalf("orders %d, asks %d; want none", w.ex.orderCount(), w.asker.count())
			}
			rows := w.execs(p.store)
			if len(rows) != 1 || rows[0].Status != "skipped" || !strings.Contains(rows[0].ErrorMsg, setting) || rows[0].ErrorMsg == PaperSkipReason {
				t.Fatalf("rows = %+v, want one skipped row naming %q", rows, setting)
			}
			if w.usage(p.store, m) != 0 {
				t.Error("a skipped run counted against the mandate")
			}
			if n := w.tells.containing("autonomy setting"); n != 1 {
				t.Fatalf("told %d times: %+v", n, w.tells.all())
			}
			// A second slot the same (plan-local) day: recorded, not told again.
			second := monday.Add(time.Hour)
			w.setClock(second)
			if _, err := p.eng.StartWithID(context.Background(), KindExecute, "second", ExecInput{PlanID: w.planID, DueMS: monday.UnixMilli(), MandateID: m.ID}); err != nil {
				t.Fatal(err)
			}
			p.drive("second")
			if n := w.tells.containing("autonomy setting"); n != 1 {
				t.Errorf("told %d times on one day, want 1", n)
			}
			// The next due slot, a week and so a day later: told again.
			next := monday.AddDate(0, 0, 7)
			w.setClock(next)
			p.run(m, next)
			if n := w.tells.containing("autonomy setting"); n != 2 {
				t.Errorf("told %d times on two days, want 2", n)
			}
			if w.ex.orderCount() != 0 {
				t.Error("an order was placed")
			}
		})
	}
}

// The workflow asks on every run: a change takes effect at the next run, in either
// direction the owner may take, and tightening back to a buy needs no restart.
func TestOwnerDial_T2_ReadOnEveryRun(t *testing.T) {
	w := newXWorld(t, true, monday)
	p := w.newProc()
	var setting atomic.Value
	setting.Store("off")
	p.setDial(func(context.Context) (string, error) { return setting.Load().(string), nil })
	m := w.mandate(p.store, nil)
	p.run(m, monday)
	if w.ex.orderCount() != 0 {
		t.Fatal("bought while off")
	}
	setting.Store("default")
	next := monday.AddDate(0, 0, 7)
	w.clock.Advance(7 * 24 * time.Hour)
	p.run(m, next)
	if w.ex.orderCount() != 1 {
		t.Fatalf("%d orders after the setting went back to default", w.ex.orderCount())
	}
}

// A dial that cannot be read, or says something unknown, skips the buy.
func TestOwnerDial_T2_UnreadableOrUnknownSkips(t *testing.T) {
	for name, f := range map[string]func(context.Context) (string, error){
		"error":   func(context.Context) (string, error) { return "", errors.New("db down") },
		"act":     constDial("act"),
		"garbage": constDial("???"),
	} {
		w := newXWorld(t, true, monday)
		p := w.newProc()
		p.setDial(f)
		m := w.mandate(p.store, nil)
		p.run(m, monday)
		rows := w.execs(p.store)
		if w.ex.orderCount() != 0 || len(rows) != 1 || rows[0].Status != "skipped" {
			t.Errorf("%s: orders %d, rows %+v", name, w.ex.orderCount(), rows)
		}
	}
}

func TestDialSkipNotice_Golden(t *testing.T) {
	en := DialSkipNotice(false, 5, "BTC/THB", "the owner's autonomy setting for mandated buys is off, so the buy was skipped", "2026-10-12")
	if en.Title != "DCA plan 5: buy skipped by your autonomy setting" || en.DedupKey != "dca-dial-skip:5:2026-10-12" || en.Severity != "notice" || en.Silent ||
		!strings.Contains(en.Body, "once a day per plan") {
		t.Errorf("en = %+v", en)
	}
	if th := DialSkipNotice(true, 5, "BTC/THB", "x", "2026-10-12"); !strings.Contains(th.Title, "แผน DCA 5") || !strings.Contains(th.Body, "BTC/THB") {
		t.Errorf("th = %+v", th)
	}
}
