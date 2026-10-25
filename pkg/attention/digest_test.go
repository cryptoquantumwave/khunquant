package attention

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// queue adds n Info candidates (digest items) to the queue at the current time.
func (e *obEnv) queue(keys ...string) []Decision {
	e.t.Helper()
	var ds []Decision
	for _, k := range keys {
		d := e.submit(cand(k, info))
		expect(e.t, d, proactive.DispDigest, ReasonInfoToDigest)
		ds = append(ds, d)
	}
	return ds
}

// primed is an obEnv whose digest for the starting day (Tuesday, 12:00) is
// already done: the first tick of a day finds the queue empty and marks it.
func newPrimed(t *testing.T) *obEnv {
	t.Helper()
	e := newObEnv(t)
	e.tick()
	return e
}

func (e *obEnv) digests() []Message {
	var out []Message
	for _, m := range e.del.messages() {
		if m.Digest {
			out = append(out, m)
		}
	}
	return out
}

func TestDigest_FlushAtDigestTime_Bangkok(t *testing.T) {
	e := newPrimed(t)
	ds := e.queue("i1", "i2", "i3") // Tuesday 12:00

	// 07:59 the next morning: Bangkok's 07:59 is 00:59 UTC. Not yet.
	e.tickAt(bkkAt(1, 7, 59))
	if n := len(e.digests()); n != 0 {
		t.Fatalf("digest sent at 07:59: %d", n)
	}
	// 08:00 Bangkok.
	e.tickAt(bkkAt(1, 8, 0))
	dgs := e.digests()
	if len(dgs) != 1 {
		t.Fatalf("%d digests at 08:00, want 1", len(dgs))
	}
	m := dgs[0]
	if m.Title != "Daily digest — 3 items" || !m.Silent || !m.Digest || m.Severity != info || len(m.Decisions) != 3 {
		t.Fatalf("digest = %+v", m)
	}
	// the digest took the three items, and only them.
	for _, d := range ds {
		r := e.rowOf(d.ID)
		if r.delivered != 1 || r.intervention != m.InterventionID {
			t.Fatalf("row %s = %+v", d.ID, r)
		}
	}
	if n := e.count(`SELECT COUNT(*) FROM attention_digest_queue WHERE flushed_ms IS NULL`); n != 0 {
		t.Fatalf("%d items left in the queue", n)
	}
	var day string
	if err := e.db.QueryRow(`SELECT value FROM attention_meta WHERE key = ?`, metaDigestDay).Scan(&day); err != nil || day != "2026-03-11" {
		t.Fatalf("last digest day = %q, %v", day, err)
	}
	// once a day.
	e.tickAt(bkkAt(1, 8, 1))
	e.tickAt(bkkAt(1, 15, 0))
	if n := len(e.digests()); n != 1 {
		t.Fatalf("%d digests after later ticks", n)
	}
}

func TestDigest_TimeZoneIsTheProfiles(t *testing.T) {
	e := newObEnv(t)
	e.prof.set(func(p *intent.Profile) { p.Timezone = "America/New_York" })
	ny := mustLoc("America/New_York")
	nyAt := func(day, h, m int) time.Time { return time.Date(2026, 3, 10+day, h, m, 0, 0, ny) }

	e.clock.Set(nyAt(0, 9, 0))
	e.tick() // the first tick finds an empty queue and marks the day done
	e.queue("i1")

	// 08:00 in Bangkok the next day is 21:00 in New York: it is not 08:00 for the owner.
	e.tickAt(bkkAt(1, 8, 0))
	if n := len(e.digests()); n != 0 {
		t.Fatalf("digest followed Bangkok time instead of the profile's: %d", n)
	}
	e.tickAt(nyAt(1, 7, 59))
	if n := len(e.digests()); n != 0 {
		t.Fatalf("digest at 07:59 New York: %d", n)
	}
	e.tickAt(nyAt(1, 8, 0))
	if n := len(e.digests()); n != 1 {
		t.Fatalf("%d digests at 08:00 New York, want 1", n)
	}
}

func TestDigest_EmptyNoMessage(t *testing.T) {
	e := newPrimed(t)
	e.tickAt(bkkAt(1, 8, 0))
	if n := len(e.del.messages()); n != 0 {
		t.Fatalf("an empty queue sent %d messages", n)
	}
	// the day is still marked done, so an item queued afterwards waits for tomorrow.
	var day string
	if err := e.db.QueryRow(`SELECT value FROM attention_meta WHERE key = ?`, metaDigestDay).Scan(&day); err != nil || day != "2026-03-11" {
		t.Fatalf("last digest day = %q, %v", day, err)
	}
	e.clock.Set(bkkAt(1, 9, 0))
	e.queue("late")
	e.tickAt(bkkAt(1, 20, 0))
	if n := len(e.del.messages()); n != 0 {
		t.Fatalf("an item queued after the digest went out in the same day: %d", n)
	}
	e.tickAt(bkkAt(2, 8, 0))
	if n := len(e.digests()); n != 1 || len(e.digests()[0].Decisions) != 1 {
		t.Fatalf("tomorrow's digest = %+v", e.digests())
	}
}

func TestDigest_Cap12(t *testing.T) {
	e := newPrimed(t)
	for i := 0; i < 20; i++ {
		e.queue("i" + strconv.Itoa(i))
		e.clock.Add(time.Second)
	}
	e.tickAt(bkkAt(1, 8, 0))
	dgs := e.digests()
	if len(dgs) != 1 {
		t.Fatalf("%d digests", len(dgs))
	}
	m := dgs[0]
	if m.Title != "Daily digest — 20 items" || len(m.Decisions) != 20 {
		t.Fatalf("digest = %q with %d decisions", m.Title, len(m.Decisions))
	}
	if n := strings.Count(m.Body, "• "); n != 12 {
		t.Fatalf("%d item lines, want 12:\n%s", n, m.Body)
	}
	if !strings.HasSuffix(m.Body, "\n\nand 8 more") {
		t.Fatalf("body does not end with \"and 8 more\":\n%s", m.Body)
	}
	// the items beyond the cap were still delivered: nothing is left to repeat.
	if n := e.count(`SELECT COUNT(*) FROM attention_digest_queue WHERE flushed_ms IS NULL`); n != 0 {
		t.Fatalf("%d items left", n)
	}
}

func TestDigest_RetryNoMoreOftenThanTenMinutes(t *testing.T) {
	e := newPrimed(t)
	ds := e.queue("i1", "i2")
	e.del.setFn(failing)

	e.tickAt(bkkAt(1, 8, 0)) // first attempt: fails
	if n := len(e.digests()); n != 1 {
		t.Fatalf("%d attempts at 08:00", n)
	}
	for _, d := range ds {
		if r := e.rowOf(d.ID); r.delivered != 0 {
			t.Fatalf("failed digest marked %s delivered", d.ID)
		}
	}
	if n := e.count(`SELECT COUNT(*) FROM attention_digest_queue WHERE flushed_ms IS NULL`); n != 2 {
		t.Fatalf("queue = %d items after a failed send, want 2", n)
	}
	if n := e.count(`SELECT COUNT(*) FROM attention_meta WHERE key = ? AND value = '2026-03-11'`, metaDigestDay); n != 0 {
		t.Fatal("a failed digest marked the day done")
	}

	for _, at := range []time.Time{bkkAt(1, 8, 1), bkkAt(1, 8, 5), bkkAt(1, 8, 9)} {
		e.tickAt(at)
	}
	if n := len(e.digests()); n != 1 {
		t.Fatalf("retried before 10 minutes: %d attempts", n)
	}
	e.tickAt(bkkAt(1, 8, 10)) // second attempt: still failing
	if n := len(e.digests()); n != 2 {
		t.Fatalf("%d attempts at 08:10, want 2", n)
	}
	e.tickAt(bkkAt(1, 8, 19))
	if n := len(e.digests()); n != 2 {
		t.Fatalf("retried again before 10 minutes: %d attempts", n)
	}

	e.del.setFn(nil) // the channel is back
	e.tickAt(bkkAt(1, 8, 20))
	dgs := e.digests()
	if len(dgs) != 3 {
		t.Fatalf("%d attempts at 08:20, want 3", len(dgs))
	}
	for _, d := range ds {
		if r := e.rowOf(d.ID); r.delivered != 1 {
			t.Fatalf("row %s = %+v after the retry", d.ID, r)
		}
	}
	e.tickAt(bkkAt(1, 9, 0))
	if n := len(e.digests()); n != 3 {
		t.Fatalf("sent again after success: %d", n)
	}
}

func TestDigest_DigestTimeInQuietHoursWaitsForFirstActiveMinute(t *testing.T) {
	e := newPrimed(t)
	e.prof.set(func(p *intent.Profile) { p.DigestTime = "05:00" })
	e.queue("i1")

	for _, at := range []time.Time{bkkAt(1, 5, 0), bkkAt(1, 6, 30), bkkAt(1, 6, 59)} {
		e.tickAt(at)
	}
	if n := len(e.digests()); n != 0 {
		t.Fatalf("digest sent in quiet hours: %d", n)
	}
	e.tickAt(bkkAt(1, 7, 0))
	if n := len(e.digests()); n != 1 {
		t.Fatalf("%d digests at 07:00 (the first active minute), want 1", n)
	}
	e.tickAt(bkkAt(1, 9, 0))
	if n := len(e.digests()); n != 1 {
		t.Fatalf("%d digests later that day", n)
	}
}

func TestDigest_DigestTimeAfterActiveHoursWaitsForNextMorning(t *testing.T) {
	e := newObEnv(t)
	e.prof.set(func(p *intent.Profile) { p.DigestTime = "23:30" })
	e.tick() // prime under this digest time: Monday's digest, due Tuesday 07:00, is done
	e.queue("i1")

	// Tuesday's digest time (23:30) is never inside the active hours, so Tuesday's
	// digest goes out at the first active minute after it: 07:00 on Wednesday.
	for _, at := range []time.Time{bkkAt(0, 21, 59), bkkAt(0, 22, 0), bkkAt(0, 23, 30), bkkAt(1, 0, 30), bkkAt(1, 6, 59)} {
		e.tickAt(at)
	}
	if n := len(e.digests()); n != 0 {
		t.Fatalf("digest sent before the first active minute: %d", n)
	}
	e.tickAt(bkkAt(1, 7, 0))
	if n := len(e.digests()); n != 1 || len(e.digests()[0].Decisions) != 1 {
		t.Fatalf("digests at 07:00 = %+v", e.digests())
	}
	// Wednesday's own is due on Thursday morning; nothing more today.
	e.tickAt(bkkAt(1, 20, 0))
	if n := len(e.digests()); n != 1 {
		t.Fatalf("%d digests", n)
	}
}

func TestDigest_CatchUpAfterDowntime(t *testing.T) {
	e := newPrimed(t)
	e.queue("i1", "i2")

	// the process was down at 08:00; its first tick is at 14:00.
	e.tickAt(bkkAt(1, 14, 0))
	dgs := e.digests()
	if len(dgs) != 1 || len(dgs[0].Decisions) != 2 {
		t.Fatalf("catch-up digest = %+v", dgs)
	}
	e.tickAt(bkkAt(1, 14, 5))
	if n := len(e.digests()); n != 1 {
		t.Fatalf("%d digests", n)
	}
}

func TestDigest_CatchUpWaitsForActiveHours(t *testing.T) {
	e := newPrimed(t)
	e.queue("i1")
	// down from the day before until 23:00: quiet hours, so nothing goes out.
	e.tickAt(bkkAt(1, 23, 0))
	if n := len(e.digests()); n != 0 {
		t.Fatalf("catch-up digest sent in quiet hours: %d", n)
	}
	e.tickAt(bkkAt(2, 7, 0))
	if n := len(e.digests()); n != 1 {
		t.Fatalf("%d digests at the next active minute, want 1", n)
	}
}

func TestDigest_NoRepeatAfterRestart(t *testing.T) {
	e := newPrimed(t)
	e.queue("i1")
	e.tickAt(bkkAt(1, 8, 0))
	if n := len(e.digests()); n != 1 {
		t.Fatalf("%d digests", n)
	}

	// a new governor and outbox over the same database: the process restarted.
	g2, err := New(e.db, e.prof, digestCfg(), config.ProactiveModeLive, e.clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	ob2 := NewOutbox(g2, e.del, time.Minute, e.clock.Now)
	e.clock.Set(bkkAt(1, 8, 30))
	if err := ob2.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(e.digests()); n != 1 {
		t.Fatalf("a restart repeated the digest: %d", n)
	}

	// and it does not skip tomorrow's.
	e.clock.Set(bkkAt(1, 20, 0))
	d := e.submit(cand("i2", info))
	expect(t, d, proactive.DispDigest, ReasonInfoToDigest)
	e.clock.Set(bkkAt(2, 8, 0))
	if err := ob2.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(e.digests()); n != 2 {
		t.Fatalf("restarted outbox skipped the next day's digest: %d", n)
	}
}

func TestDigest_RetrySpacingSurvivesRestart(t *testing.T) {
	e := newPrimed(t)
	e.queue("i1")
	e.del.setFn(failing)
	e.tickAt(bkkAt(1, 8, 0))

	g2, err := New(e.db, e.prof, digestCfg(), config.ProactiveModeLive, e.clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	ob2 := NewOutbox(g2, e.del, time.Minute, e.clock.Now)
	e.clock.Set(bkkAt(1, 8, 5))
	if err := ob2.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(e.digests()); n != 1 {
		t.Fatalf("a restart retried within 10 minutes: %d attempts", n)
	}
	e.clock.Set(bkkAt(1, 8, 10))
	if err := ob2.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(e.digests()); n != 2 {
		t.Fatalf("%d attempts at 08:10, want 2", n)
	}
}

func TestDigest_DoesNotConsumeBudget(t *testing.T) {
	e := newPrimed(t)
	e.queue("i1", "i2", "i3", "i4")
	e.tickAt(bkkAt(1, 8, 0))
	if n := len(e.digests()); n != 1 {
		t.Fatalf("%d digests", n)
	}
	// the whole budget of 3 is still there.
	for _, k := range []string{"a", "b", "c"} {
		e.clock.Add(2 * time.Minute)
		d := e.submit(cand(k, notice))
		expect(t, d, proactive.DispNotify, ReasonTemplate)
		e.tickAt(e.clock.Now().Add(time.Minute))
	}
	e.clock.Add(2 * time.Minute)
	expect(t, e.submit(cand("d", notice)), proactive.DispDigest, ReasonBudget)
}

func TestDigest_DisabledOrNotLive(t *testing.T) {
	t.Run("config switch off", func(t *testing.T) {
		cfg := digestCfg()
		cfg.Digest = false
		e := newObEnvCfg(t, cfg)
		e.tick()
		e.queue("i1")
		e.tickAt(bkkAt(3, 8, 0))
		if n := len(e.del.messages()); n != 0 {
			t.Fatalf("digest sent with digest disabled: %d", n)
		}
	})
	t.Run("shadow", func(t *testing.T) {
		e := newEnvMode(t, config.ProactiveModeShadow, digestCfg())
		dg, err := e.g.PendingDigest(context.Background())
		if err != nil || dg != nil {
			t.Fatalf("shadow PendingDigest = %v, %v", dg, err)
		}
		if err := e.g.CompleteDigest(context.Background(), &Digest{Day: "2026-03-11"}, Delivery{Delivered: true}); err != nil {
			t.Fatal(err)
		}
		if n := e.count(`SELECT COUNT(*) FROM attention_meta`); n != 0 {
			t.Fatal("shadow wrote digest bookkeeping")
		}
	})
}

func TestDigest_ProfileUnavailableFallsBackToDefaults(t *testing.T) {
	e := newPrimed(t)
	e.queue("i1")
	e.prof.mu.Lock()
	e.prof.err = context.DeadlineExceeded
	e.prof.mu.Unlock()
	e.tickAt(bkkAt(1, 8, 0)) // the default profile: Bangkok, 07-22, digest 08:00
	if n := len(e.digests()); n != 1 {
		t.Fatalf("%d digests with the default profile", n)
	}
}

func TestDigest_BadDigestTimeFallsBackToDefault(t *testing.T) {
	for _, bad := range []string{"", "8am", "25:99", "08:0"} {
		h, m := digestClock(bad)
		if h != 8 || m != 0 {
			t.Errorf("digestClock(%q) = %02d:%02d, want 08:00", bad, h, m)
		}
	}
	if h, m := digestClock(" 21:45 "); h != 21 || m != 45 {
		t.Errorf("digestClock(\" 21:45 \") = %02d:%02d", h, m)
	}
}

func TestDigest_CompleteIsIdempotentAndToleratesPrunedItems(t *testing.T) {
	e := newPrimed(t)
	ds := e.queue("i1", "i2")
	e.clock.Set(bkkAt(1, 8, 0))
	dg, err := e.g.PendingDigest(context.Background())
	if err != nil || dg == nil || len(dg.Items) != 2 {
		t.Fatalf("PendingDigest = %+v, %v", dg, err)
	}
	// a second caller is not handed the same digest.
	if again, err := e.g.PendingDigest(context.Background()); err != nil || again != nil {
		t.Fatalf("second PendingDigest = %+v, %v", again, err)
	}
	// one item is pruned while the digest is being sent.
	if _, err := e.db.Exec(`DELETE FROM attention_decisions WHERE id = ?`, ds[0].ID); err != nil {
		t.Fatal(err)
	}
	r := Delivery{Delivered: true, InterventionID: "01ARZ3NDEKTSV4RRFFQ69G5FAV"}
	for i := 0; i < 2; i++ {
		if err := e.g.CompleteDigest(context.Background(), dg, r); err != nil {
			t.Fatalf("CompleteDigest #%d: %v", i, err)
		}
	}
	if got := e.rowOf(ds[1].ID); got.delivered != 1 || got.intervention != r.InterventionID {
		t.Fatalf("row = %+v", got)
	}
	if s, ok := e.state("i2"); !ok || s.sends != 1 {
		t.Fatalf("state after two completions = %+v ok=%v", s, ok)
	}
	if n := e.count(`SELECT COUNT(*) FROM attention_digest_queue`); n != 1 {
		t.Fatalf("queue rows = %d (the pruned item's must cascade away)", n)
	}
	// nil and failed completions are no-ops.
	if err := e.g.CompleteDigest(context.Background(), nil, r); err != nil {
		t.Fatal(err)
	}
	if err := e.g.CompleteDigest(context.Background(), dg, Delivery{}); err != nil {
		t.Fatal(err)
	}
}

func TestDigest_UndecodableCandidateStillListed(t *testing.T) {
	e := newPrimed(t)
	d := e.queue("broken")[0]
	if _, err := e.db.Exec(`UPDATE attention_decisions SET candidate_json = 'not json' WHERE id = ?`, d.ID); err != nil {
		t.Fatal(err)
	}
	e.tickAt(bkkAt(1, 8, 0))
	dgs := e.digests()
	if len(dgs) != 1 || !strings.Contains(dgs[0].Body, "• broken") {
		t.Fatalf("digest = %+v", dgs)
	}
}

func TestDigest_ProfileDigestTimeChangeAppliesImmediately(t *testing.T) {
	e := newPrimed(t)
	e.queue("i1")
	e.prof.set(func(p *intent.Profile) { p.DigestTime = "10:00" })
	e.tickAt(bkkAt(1, 9, 59))
	if n := len(e.digests()); n != 0 {
		t.Fatal("digest before the profile's digest time")
	}
	e.tickAt(bkkAt(1, 10, 0))
	if n := len(e.digests()); n != 1 {
		t.Fatalf("%d digests at 10:00", n)
	}
}

// ---- the pure renderer ------------------------------------------------------

func decision(key, title, subject string, sev proactive.Severity, sal float64, at time.Time) Decision {
	return Decision{
		ID: key, At: at, Disposition: proactive.DispDigest,
		Candidate: proactive.Candidate{DedupKey: key, Title: title, Subject: subject, Severity: sev, Salience: sal},
	}
}

func TestRenderDigest_GroupsBySubjectAndRanks(t *testing.T) {
	t0 := bkkAt(0, 9, 0)
	items := []Decision{
		decision("a", "BTC small", "BTC/USDT", info, 0.1, t0),
		decision("b", "ETH big", "ETH/USDT", warning, 0.2, t0.Add(time.Minute)),
		decision("c", "BTC bigger", "BTC/USDT", notice, 0.9, t0.Add(2*time.Minute)),
		decision("d", "BTC bigger older", "BTC/USDT", notice, 0.9, t0.Add(-time.Minute)),
		decision("e", "no subject", "", info, 0.5, t0),
	}
	title, body := RenderDigest(items)
	if title != "Daily digest — 5 items" {
		t.Fatalf("title = %q", title)
	}
	want := strings.Join([]string{
		"ETH/USDT", // the warning ranks first, so its subject does
		"• ETH big",
		"BTC/USDT",
		"• BTC bigger older", // notice, salience .9, older
		"• BTC bigger",
		"• BTC small",
		"General",
		"• no subject",
	}, "\n")
	if body != want {
		t.Fatalf("body =\n%s\nwant\n%s", body, want)
	}
	if title, body := RenderDigest(items[:1]); title != "Daily digest — 1 item" || !strings.Contains(body, "• BTC small") {
		t.Fatalf("single = %q %q", title, body)
	}
	if title, body := RenderDigest(nil); title != "" || body != "" {
		t.Fatalf("empty = %q %q", title, body)
	}
	// the input is not reordered.
	if items[0].ID != "a" || items[4].ID != "e" {
		t.Fatal("RenderDigest reordered its argument")
	}
}

func TestRenderDigest_TitleIsOneCleanLine(t *testing.T) {
	t0 := bkkAt(0, 9, 0)
	items := []Decision{
		decision("a", "first\n• INJECTED\r\nsecond\x00\x1b[31m\u202e\u200b", "S\nT", notice, 0, t0),
		decision("b", strings.Repeat("x", 500), "S", notice, 0, t0.Add(time.Second)),
		decision("c", "", "S", notice, 0, t0.Add(2*time.Second)),
		decision("", "", "S", notice, 0, t0.Add(3*time.Second)),
	}
	items[3].Candidate.DedupKey = ""
	_, body := RenderDigest(items)
	got := 0
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "• ") {
			got++
		}
	}
	if got != 4 {
		t.Fatalf("%d item lines, want 4:\n%s", got, body)
	}
	for _, line := range strings.Split(body, "\n") {
		if strings.ContainsAny(line, "\r\x00\x1b\u202e\u200b") {
			t.Errorf("control character in %q", line)
		}
		if utf8Len(line) > maxDigestLineRune+2 {
			t.Errorf("line of %d runes", utf8Len(line))
		}
	}
	if !strings.Contains(body, "S T\n• first • INJECTED second[31m") {
		t.Errorf("newlines were not flattened:\n%s", body)
	}
	if !strings.Contains(body, "• c") || !strings.Contains(body, "• (no title)") {
		t.Errorf("fallback titles missing:\n%s", body)
	}
}

func utf8Len(s string) int { return len([]rune(s)) }

func TestRenderDigest_CapAndMore(t *testing.T) {
	t0 := bkkAt(0, 9, 0)
	var items []Decision
	for i := 0; i < 13; i++ {
		items = append(items, decision("k"+strconv.Itoa(i), "t"+strconv.Itoa(i), "S", notice, 0, t0.Add(time.Duration(i)*time.Second)))
	}
	_, body := RenderDigest(items[:12])
	if strings.Contains(body, "more") || strings.Count(body, "• ") != 12 {
		t.Fatalf("12 items:\n%s", body)
	}
	_, body = RenderDigest(items)
	if strings.Count(body, "• ") != 12 || !strings.HasSuffix(body, "and 1 more") {
		t.Fatalf("13 items:\n%s", body)
	}
}
