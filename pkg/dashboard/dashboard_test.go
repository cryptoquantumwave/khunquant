package dashboard

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/attention"
	"github.com/cryptoquantumwave/khunquant/pkg/cognition"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/feedback"
	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex/sqlitextest"
)

// The dashboard's reads, over the real tables of attention, feedback, cognition and
// approval in one database, with rows planted by SQL so that each number is exact.

var (
	bkk = time.FixedZone("Asia/Bangkok", 7*3600)
	now = time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC) // 19:00 on 10 March in Bangkok
)

func utc(day, hour, min int) time.Time { return time.Date(2026, 3, day, hour, min, 0, 0, time.UTC) }

type rig struct {
	t   *testing.T
	db  *sql.DB
	fb  *feedback.Service
	r   *Reader
	seq int
}

type fakeApprovalBackend struct{ approval.OrderBackend }
type fakeNotifier struct{ approval.Notifier }

// newRig builds every table through its own package; withFeedback and withApproval leave
// out the optional ones, as a gateway with those features off does.
func newRig(t *testing.T, withFeedback, withApproval bool) *rig {
	t.Helper()
	db := sqlitextest.Open(t)
	ctx := context.Background()
	profiles, err := intent.NewStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := attention.New(db, profiles, config.AttentionConfig{Mode: config.ProactiveModeLive, DailyBudget: 50, DedupWindowHours: 24}, config.ProactiveModeLive, func() time.Time { return now }); err != nil {
		t.Fatal(err)
	}
	if _, err := cognition.New(config.CognitionConfig{}, db, nil, nil); err != nil {
		t.Fatal(err)
	}
	g := &rig{t: t, db: db}
	zone := func(context.Context) *time.Location { return bkk }
	g.r = &Reader{DB: db, Zone: zone, Now: func() time.Time { return now }}
	if withFeedback {
		if _, err := feedback.New(ctx, db, feedback.Options{Clock: func() time.Time { return now }, Zone: zone}); err != nil {
			t.Fatal(err)
		}
		// The fills log began long ago, so the harm metric has complete days to count.
		g.exec(`UPDATE schema_migrations SET applied_at = ? WHERE component = 'feedback' AND version = 3`, utc(1, 0, 0).AddDate(0, -2, 0).UnixMilli())
		g.fb, err = feedback.New(ctx, db, feedback.Options{Clock: func() time.Time { return now }, Zone: zone})
		if err != nil {
			t.Fatal(err)
		}
		g.r.Harm = g.fb
		g.exec(`CREATE TABLE notify_sent (intervention_id TEXT NOT NULL, sent_at_ms INTEGER NOT NULL)`)
	}
	if withApproval {
		cfg := config.DefaultConfig()
		if _, err := approval.New(ctx, approval.Options{DB: db, Cfg: cfg, Backend: fakeApprovalBackend{}, Notifier: fakeNotifier{}}); err != nil {
			t.Fatal(err)
		}
	}
	return g
}

func (g *rig) exec(q string, args ...any) {
	g.t.Helper()
	if _, err := g.db.Exec(q, args...); err != nil {
		g.t.Fatalf("%s: %v", q, err)
	}
}

type dec struct {
	intervention string // "" = none yet
	at           time.Time
	disp         string
	delivered    time.Time // zero = not delivered
	trigger      string
	reason       string
	holdout      int
	propensity   any
	candidate    string
}

// decision plants one attention decision and returns its id.
func (g *rig) decision(d dec) string {
	g.t.Helper()
	g.seq++
	id := fmt.Sprintf("dec-%03d", g.seq)
	if d.trigger == "" {
		d.trigger = "price_cross:1"
	}
	if d.candidate == "" {
		d.candidate = `{"Title":"t` + id + `"}`
	}
	var delivered, deliveredAt any = 0, nil
	if !d.delivered.IsZero() {
		delivered, deliveredAt = 1, d.delivered.UnixMilli()
	}
	var iv any
	if d.intervention != "" {
		iv = d.intervention
	}
	g.exec(`INSERT INTO attention_decisions (id, at_ms, dedup_key, trigger_id, severity, disposition, reason, shadow, candidate_json,
		delivered, delivered_at_ms, intervention_id, propensity, holdout) VALUES (?, ?, 'DEDUP-MARKER', ?, 2, ?, ?, 0, ?, ?, ?, ?, ?, ?)`,
		id, d.at.UnixMilli(), d.trigger, d.disp, d.reason, d.candidate, delivered, deliveredAt, iv, d.propensity, d.holdout)
	return id
}

func (g *rig) respond(intervention string, kind feedback.Kind, at time.Time) {
	g.t.Helper()
	if _, err := g.fb.Record(context.Background(), feedback.Response{InterventionID: intervention, Kind: kind, Source: "test", Event: string(kind) + at.String(), At: at}); err != nil {
		g.t.Fatal(err)
	}
}

func (g *rig) proposal(kind string, reducing int, status string, created time.Time, summary string) {
	g.t.Helper()
	g.seq++
	var decided, finished any
	if status != "pending" {
		decided, finished = created.Add(time.Minute).UnixMilli(), created.Add(2*time.Minute).UnixMilli()
	}
	g.exec(`INSERT INTO proposals (id, code, nonce, kind, params, hash, summary, rationale, origin, reducing, status, created_at_ms, expires_at_ms, decided_at_ms, decided_by, finished_at_ms)
		VALUES (?, ?, 'NONCE-MARKER', ?, '{"secret":"PARAM-MARKER"}', x'00', ?, 'RATIONALE-MARKER', 'owner', ?, ?, ?, ?, ?, 'telegram:SENDER-MARKER', ?)`,
		fmt.Sprintf("p%d", g.seq), fmt.Sprintf("C%d", g.seq), kind, summary, reducing, status, created.UnixMilli(), created.Add(time.Hour).UnixMilli(), decided, finished)
}

func metricJSON(t *testing.T, m Metrics) map[string]map[string]any {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(b, &all); err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string]any{}
	for k, v := range all {
		var mm map[string]any
		if json.Unmarshal(v, &mm) == nil && (mm["value"] != nil || mm["reason"] != nil) {
			out[k] = mm
		}
	}
	return out
}

func TestMetrics_Computation(t *testing.T) {
	g := newRig(t, true, true)

	// Four sent interventions. I1 goes out at 00:05 and I4 at 03:00 Bangkok time on the 9th
	// (the 8th in UTC): the owner's zone puts them on the 9th.
	i1 := g.decision(dec{intervention: "I1", at: utc(8, 16, 55), disp: "notify", delivered: utc(8, 17, 5)}) // latency 600 s
	g.decision(dec{intervention: "I1", at: utc(8, 16, 58), disp: "escalate", delivered: utc(8, 17, 5)})     // shares I1's message
	g.decision(dec{intervention: "I2", at: utc(9, 1, 0), disp: "escalate", delivered: utc(9, 1, 1)})        // 60 s
	g.decision(dec{intervention: "I3", at: utc(10, 3, 0), disp: "notify", delivered: utc(10, 3, 20)})       // 1200 s
	g.decision(dec{intervention: "I4", at: utc(8, 20, 0), disp: "notify", delivered: utc(8, 20, 30)})       // 1800 s
	// Learning: a Notice moved to the digest by low acceptance, and a holdout.
	g.decision(dec{at: utc(10, 4, 0), disp: "digest", reason: attention.ReasonLowAcceptance, propensity: 1.0})
	g.decision(dec{at: utc(9, 5, 0), disp: "digest", reason: attention.ReasonHoldout, holdout: 1, propensity: 0.9})
	g.decision(dec{at: utc(9, 6, 0), disp: "drop", reason: "dedup"})
	_ = i1
	// Before the window: counted nowhere.
	g.decision(dec{intervention: "I0", at: utc(1, 1, 0), disp: "notify", delivered: utc(1, 1, 5)})

	g.respond("I1", feedback.KindUseful, utc(8, 18, 0))
	g.respond("I1", feedback.KindNotUseful, utc(8, 19, 0)) // the latest rating wins
	g.respond("I2", feedback.KindUseful, utc(9, 2, 0))
	g.respond("I3", feedback.KindMute, utc(10, 4, 0))
	g.respond("I4", feedback.KindNone, utc(9, 20, 30))
	g.respond("I0", feedback.KindUseful, utc(1, 2, 0)) // outside the window

	g.exec(`INSERT INTO cognition_usage (day, kind, wakes, input_tokens, output_tokens) VALUES
		('2026-03-09', 'heartbeat', 2, 100, 50), ('2026-03-09', 'news', 1, 10, 5), ('2026-03-01', 'news', 9, 9, 9)`)

	g.proposal("order", 1, "executed", utc(9, 3, 0), "close")
	g.proposal("order", 1, "rejected", utc(9, 4, 0), "close")
	g.proposal("order", 0, "expired", utc(9, 5, 0), "buy")
	g.proposal("order", 0, "pending", utc(10, 5, 0), "buy")
	g.proposal("order", 0, "cancelled", utc(10, 6, 0), "buy") // a stop, not a decision
	g.proposal("memory", 0, "approved", utc(9, 6, 0), "remember")
	g.proposal("mute", 0, "expired", utc(9, 7, 0), "mute")

	// Harm: one alert day (the 9th, I1's alert is in notify_sent) and a fill 17 hours after it,
	// and one fill on a quiet day.
	g.exec(`INSERT INTO notify_sent (intervention_id, sent_at_ms) VALUES ('I1', ?)`, utc(8, 17, 5).UnixMilli())
	g.exec(`INSERT INTO feedback_fills (id, at_ms, symbol) VALUES ('f1', ?, 'BTC/USDT'), ('f2', ?, 'ETH/USDT:USDT')`,
		utc(9, 10, 0).UnixMilli(), utc(3, 10, 0).UnixMilli())

	m, err := g.r.Metrics(context.Background(), 3)
	if err != nil {
		t.Fatal(err)
	}
	if m.Zone != "Asia/Bangkok" || m.From != "2026-03-08" || m.To != "2026-03-10" {
		t.Fatalf("window = %s %s..%s, want the owner's last 3 days", m.Zone, m.From, m.To)
	}
	j := metricJSON(t, m)
	for name, mm := range j {
		if mm["value"] == nil && (mm["reason"] == nil || mm["reason"] == "") {
			t.Errorf("%s is null without a reason", name)
		}
	}

	sends := j["sends_per_day"]["value"].(map[string]any)
	if sends["total"] != 4.0 || fmt.Sprint(sends["average"]) != fmt.Sprint(4.0/3) {
		t.Errorf("sends = %v, want 4 interventions, 4/3 a day (two decisions share I1)", sends)
	}
	var got []string
	for _, d := range sends["series"].([]any) {
		d := d.(map[string]any)
		got = append(got, fmt.Sprintf("%v=%v", d["day"], d["sends"]))
	}
	if strings.Join(got, " ") != "2026-03-08=0 2026-03-09=3 2026-03-10=1" {
		t.Errorf("sends per day = %v: the owner's days, not UTC's", got)
	}
	if v := j["precision_at_sent"]["value"]; v != 0.5 { // I2 useful; I1 not useful (latest)
		t.Errorf("precision@sent = %v, want 0.5", v)
	}
	if v := j["not_useful_rate"]["value"]; v != 0.25 {
		t.Errorf("not-useful rate = %v, want 1 of 4", v)
	}
	if v := j["mute_rate"]["value"]; v != 0.25 {
		t.Errorf("mute rate = %v, want 1 of 4", v)
	}
	if v := j["no_response_rate"]["value"]; fmt.Sprint(v) != fmt.Sprint(1.0/3) { // I3 is too young to count
		t.Errorf("no-response rate = %v, want 1 of the 3 sends older than 24 hours", v)
	}
	if kinds := fmt.Sprint(j["responses_by_kind"]["value"]); kinds != "map[mute:1 none:1 not_useful:1 useful:2]" {
		t.Errorf("responses by kind = %v", kinds)
	}
	if v := j["critical_recall"]; v["value"] != nil || v["reason"] != "needs labels: run khunquant replay" {
		t.Errorf("critical recall = %v", v)
	}
	// Latency is per decision row: 600 and 420 (both of I1's), 60, 1200 and 1800 seconds.
	if v := j["median_queue_latency_seconds"]["value"]; v != 600.0 {
		t.Errorf("median queue latency = %v, want 600", v)
	}
	usage := j["llm_usage_per_day"]["value"].([]any)
	if len(usage) != 1 || fmt.Sprint(usage[0]) != "map[day:2026-03-09 tokens:165 wakes:3]" {
		t.Errorf("llm usage = %v, want only the 9th, summed over kinds", usage)
	}
	learn := j["learning_per_day"]["value"].([]any)
	if fmt.Sprint(learn) != "[map[day:2026-03-08 holdout:0 low_acceptance:0] map[day:2026-03-09 holdout:1 low_acceptance:0] map[day:2026-03-10 holdout:0 low_acceptance:1]]" {
		t.Errorf("learning per day = %v", learn)
	}

	rates := map[string]ApprovalRate{}
	for _, a := range m.ApprovalRates.Value.([]ApprovalRate) {
		rates[a.Group] = a
	}
	rate := func(g string) any {
		if r := rates[g].Rate; r != nil {
			return *r
		}
		return nil
	}
	if rate("T1") != 0.5 || rate("T3") != 0.0 || rate("T2") != nil || rate("memory") != 1.0 || rate("reflection") != 0.0 {
		t.Errorf("approval rates T1 %v T2 %v T3 %v memory %v reflection %v", rate("T1"), rate("T2"), rate("T3"), rate("memory"), rate("reflection"))
	}
	if rates["T3"].Pending != 1 || rates["T3"].Expired != 1 || rates["T3"].Approved != 0 {
		t.Errorf("T3 = %+v: cancelled is not counted, pending is not decided", rates["T3"])
	}

	harm := j["post_alert_trading"]["value"].(map[string]any)
	if harm["alert_days"] != 1.0 || harm["post_alert_trades"] != 1.0 || harm["quiet_trades"] != 1.0 || harm["post_alert_per_alert_day"] != 1.0 {
		t.Errorf("harm = %v", harm)
	}
}

func TestMetrics_NullsWithReasons(t *testing.T) {
	// Everything exists but nothing happened: counts that have a denominator are not
	// invented, and a metric with no data is null with its reason, never 0.
	g := newRig(t, true, true)
	m, err := g.r.Metrics(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	for name, mm := range metricJSON(t, m) {
		if name == "approval_rates" || name == "responses_by_kind" || name == "post_alert_trading" {
			continue // present and empty (a list of groups, a map, a record of days without alerts)
		}
		if mm["value"] != nil || mm["reason"] == nil || mm["reason"] == "" {
			t.Errorf("%s = %v, want null with a reason", name, mm)
		}
	}
	for _, a := range m.ApprovalRates.Value.([]ApprovalRate) {
		if a.Rate != nil {
			t.Errorf("approval rate %s = %v with nothing decided", a.Group, *a.Rate)
		}
	}
	if h := metricJSON(t, m)["post_alert_trading"]["value"].(map[string]any); h["post_alert_per_alert_day"] != nil || h["quiet_per_quiet_day"] != 0.0 {
		t.Errorf("harm with no alert day = %v: the alert rate is null, the quiet rate is a real 0", h)
	}

	// Delivered but never rated: precision is null, the rates that have a denominator are real zeros.
	g.decision(dec{intervention: "X", at: utc(10, 1, 0), disp: "notify", delivered: utc(10, 1, 1)})
	m, _ = g.r.Metrics(context.Background(), 7)
	j := metricJSON(t, m)
	if j["precision_at_sent"]["value"] != nil || j["not_useful_rate"]["value"] != 0.0 || j["mute_rate"]["value"] != 0.0 {
		t.Errorf("precision %v, not useful %v, mute %v", j["precision_at_sent"], j["not_useful_rate"], j["mute_rate"])
	}
	if j["no_response_rate"]["value"] != nil {
		t.Errorf("no-response rate = %v: the only send is under 24 hours old", j["no_response_rate"])
	}
}

func TestMetrics_MissingTablesAreNullNotErrors(t *testing.T) {
	g := newRig(t, false, false)
	g.decision(dec{intervention: "I", at: utc(10, 1, 0), disp: "notify", delivered: utc(10, 1, 1)})
	m, err := g.r.Metrics(context.Background(), 30)
	if err != nil {
		t.Fatal(err)
	}
	j := metricJSON(t, m)
	for _, name := range []string{"precision_at_sent", "not_useful_rate", "mute_rate", "no_response_rate", "responses_by_kind", "post_alert_trading", "approval_rates"} {
		if j[name]["value"] != nil || j[name]["reason"] == nil {
			t.Errorf("%s = %v, want null with a reason while its table does not exist", name, j[name])
		}
	}
	if j["sends_per_day"]["value"] == nil {
		t.Error("sends are known without any feedback table")
	}
	for _, bad := range []int{0, 91, -1} {
		if _, err := g.r.Metrics(context.Background(), bad); !errors.Is(err, ErrBadRequest) {
			t.Errorf("days=%d: %v, want a bad request", bad, err)
		}
	}
	// No attention table at all: still an answer.
	empty := &Reader{DB: sqlitextest.Open(t), Now: func() time.Time { return now }}
	if m, err := empty.Metrics(context.Background(), 7); err != nil || m.SendsPerDay.Value != nil || m.SendsPerDay.Reason == "" {
		t.Errorf("empty database: %+v %v", m.SendsPerDay, err)
	}
}

func TestInterventions_PagingFiltersAndProjection(t *testing.T) {
	g := newRig(t, true, false)
	ctx := context.Background()
	const secret = `{"Title":"Hello <b>there</b>\nline","Quote":"QUOTE-MARKER","Detail":"DETAIL-MARKER","Evidence":{"k":"EVIDENCE-MARKER"}}`
	var ids []string
	for i := 0; i < 5; i++ {
		trig := "price_cross:7"
		if i%2 == 1 {
			trig = "drawdown:portfolio"
		}
		ids = append(ids, g.decision(dec{intervention: fmt.Sprintf("I%d", i), at: utc(9, 10+i, 0), disp: "notify", delivered: utc(9, 10+i, 1), trigger: trig, candidate: secret}))
	}
	g.decision(dec{at: utc(9, 20, 0), disp: "drop", trigger: "price_crossing:9"}) // a longer kind that starts the same
	g.decision(dec{at: utc(9, 21, 0), disp: "log", trigger: "heartbeat", candidate: `{not json`})
	g.respond("I4", feedback.KindUseful, utc(9, 15, 0))
	g.respond("I4", feedback.KindSnooze, utc(9, 15, 5))
	g.exec(`INSERT INTO feedback_hindsight (intervention_id, horizon, pct_change, portfolio_pct, trades, computed_at_ms)
		VALUES ('I4', '24h', -3.5, NULL, 2, ?), ('I4', '1h', 1.5, 0.1, NULL, ?)`, utc(10, 1, 0).UnixMilli(), utc(9, 16, 0).UnixMilli())

	// Paging: newest first, no row twice, the cursor ends.
	var seen []string
	cursor := ""
	for page := 0; page < 10; page++ {
		items, next, err := g.r.Interventions(ctx, Filter{Limit: 3, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range items {
			seen = append(seen, it.ID)
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if len(seen) != 7 {
		t.Fatalf("paged %d rows, want 7: %v", len(seen), seen)
	}
	dup := map[string]bool{}
	for _, id := range seen {
		if dup[id] {
			t.Fatalf("row %s came twice: %v", id, seen)
		}
		dup[id] = true
	}
	first, _, _ := g.r.Interventions(ctx, Filter{Limit: 200})
	for i := 1; i < len(first); i++ {
		if first[i].At.After(first[i-1].At) {
			t.Fatal("not newest first")
		}
	}

	// Filters: disposition, trigger kind (whole, never a pattern), and the time range.
	byDisp, _, _ := g.r.Interventions(ctx, Filter{Disposition: "drop"})
	if len(byDisp) != 1 || byDisp[0].TriggerKind != "price_crossing" {
		t.Errorf("by disposition = %+v", byDisp)
	}
	byTrig, _, _ := g.r.Interventions(ctx, Filter{Trigger: "price_cross"})
	if len(byTrig) != 3 {
		t.Errorf("trigger price_cross matched %d rows, want 3 (not price_crossing)", len(byTrig))
	}
	for _, pattern := range []string{"price%", "price_cros_", "%"} {
		if got, _, _ := g.r.Interventions(ctx, Filter{Trigger: pattern}); len(got) != 0 {
			t.Errorf("trigger %q matched %d rows: a pattern widened the filter", pattern, len(got))
		}
	}
	ranged, _, _ := g.r.Interventions(ctx, Filter{From: utc(9, 12, 0), To: utc(9, 14, 0)})
	if len(ranged) != 2 {
		t.Errorf("range matched %d rows, want the 12:00 and 13:00 ones", len(ranged))
	}

	// Projection: the title only, cleaned; the responses and hindsight of its intervention.
	all, _, _ := g.r.Interventions(ctx, Filter{Limit: 200})
	b, _ := json.Marshal(all)
	for _, marker := range []string{"QUOTE-MARKER", "DETAIL-MARKER", "EVIDENCE-MARKER", "DEDUP-MARKER", "candidate_json", "Evidence", "trigger_id", "dedup_key", "price_cross:7"} {
		if strings.Contains(string(b), marker) {
			t.Errorf("the projection carries %q", marker)
		}
	}
	var row *Intervention
	for i := range all {
		if all[i].Title == "Hello <b>there</b> line" && len(all[i].Responses) > 0 {
			row = &all[i]
		}
	}
	if row == nil {
		t.Fatalf("no row with the cleaned title and its responses: %s", b)
	}
	if len(row.Responses) != 2 || row.Responses[0] != (Response{"useful", ""}) || row.Responses[1].Kind != "snooze" {
		t.Errorf("responses = %+v", row.Responses)
	}
	if len(row.Hindsight) != 2 || row.Hindsight[0].Horizon != "1h" || row.Hindsight[1].PortfolioPct != nil || *row.Hindsight[1].PctChange != -3.5 || *row.Hindsight[1].Trades != 2 {
		t.Errorf("hindsight = %+v", row.Hindsight)
	}
	for _, it := range all {
		if it.Title == "" && it.TriggerKind != "heartbeat" && it.Disposition != "drop" {
			t.Errorf("row %s lost its title", it.ID)
		}
	}

	if _, _, err := g.r.Interventions(ctx, Filter{Cursor: "garbage"}); !errors.Is(err, ErrBadRequest) {
		t.Errorf("bad cursor: %v", err)
	}
	if _, _, err := g.r.Interventions(ctx, Filter{Limit: MaxLimit + 1}); !errors.Is(err, ErrBadRequest) {
		t.Errorf("limit over the maximum: %v", err)
	}
	_ = ids
}

func TestChanges_CursorAdvancesWithoutDuplicates(t *testing.T) {
	g := newRig(t, true, true)
	ctx := context.Background()
	g.decision(dec{at: utc(9, 1, 0), disp: "notify"}) // before the stream starts

	items, head, err := g.r.Changes(ctx, "", 0)
	if err != nil || len(items) != 0 || head == "" {
		t.Fatalf("an empty cursor gave %d items, head %q, %v: a new stream starts at now", len(items), head, err)
	}
	if items, again, _ := g.r.Changes(ctx, head, 0); len(items) != 0 || again != head {
		t.Fatalf("nothing changed, yet %d items and cursor %q (was %q)", len(items), again, head)
	}

	g.decision(dec{at: utc(10, 1, 0), disp: "notify", candidate: `{"Title":"first"}`})
	g.proposal("order", 1, "pending", utc(10, 1, 30), "close BTC")
	g.decision(dec{at: utc(10, 2, 0), disp: "log"})
	items, cur, err := g.r.Changes(ctx, head, 0)
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, it := range items {
		types = append(types, it.Type)
	}
	if strings.Join(types, " ") != "attention.decision approval.pending attention.decision" {
		t.Fatalf("items = %v, want the three in time order", types)
	}
	if again, c2, _ := g.r.Changes(ctx, cur, 0); len(again) != 0 || c2 != cur {
		t.Fatalf("a second poll returned %d items: duplicates", len(again))
	}
	// Resuming from an item's own cursor skips it and everything before it.
	if rest, _, _ := g.r.Changes(ctx, items[0].Cursor, 0); len(rest) != 2 {
		t.Errorf("resuming after the first item gave %d items, want 2", len(rest))
	}

	// The proposal is decided and finished: one new item, with the new status.
	g.exec(`UPDATE proposals SET status = 'executed', decided_at_ms = ?, finished_at_ms = ?, decided_by = 'web'`, utc(10, 3, 0).UnixMilli(), utc(10, 3, 1).UnixMilli())
	items, cur, _ = g.r.Changes(ctx, cur, 0)
	if len(items) != 1 || items[0].Type != "approval.executed" {
		t.Fatalf("after the update: %+v", items)
	}
	if ap := items[0].Data.(ApprovalItem); ap.Group != "T1" || ap.DecidedBy != "web" || ap.Summary != "close BTC" {
		t.Errorf("approval item = %+v", ap)
	}
	b, _ := json.Marshal(items)
	for _, marker := range []string{"NONCE-MARKER", "PARAM-MARKER", "RATIONALE-MARKER", "SENDER-MARKER"} {
		if strings.Contains(string(b), marker) {
			t.Errorf("the approval projection carries %q", marker)
		}
	}
	if again, _, _ := g.r.Changes(ctx, cur, 0); len(again) != 0 {
		t.Fatalf("the executed proposal came again")
	}

	// A poll is capped, and the cursor moves only over what it returned.
	for i := 0; i < 250; i++ {
		g.decision(dec{at: utc(10, 4, 0).Add(time.Duration(i) * time.Second), disp: "log"})
	}
	first, cur, _ := g.r.Changes(ctx, cur, 0)
	second, cur, _ := g.r.Changes(ctx, cur, 0)
	third, _, _ := g.r.Changes(ctx, cur, 0)
	if len(first) != MaxLimit || len(second) != 50 || len(third) != 0 {
		t.Fatalf("pages of %d, %d, %d; want 200, 50, 0", len(first), len(second), len(third))
	}
	dup := map[string]bool{}
	for _, it := range append(first, second...) {
		id := it.Data.(Intervention).ID
		if dup[id] {
			t.Fatalf("%s came twice", id)
		}
		dup[id] = true
	}

	if _, _, err := g.r.Changes(ctx, "nope", 0); !errors.Is(err, ErrBadRequest) {
		t.Errorf("bad cursor: %v", err)
	}
}
