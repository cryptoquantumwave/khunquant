package attention

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"math/rand"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex/sqlitextest"
)

// fakeAcceptance is a scriptable AcceptanceSource.
type fakeAcceptance struct {
	mu    sync.Mutex
	mean  float64
	n     int
	err   error
	calls []string
}

func (f *fakeAcceptance) Acceptance(_ context.Context, _ Queryer, kind string, bucket int, _ time.Time) (float64, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, kind)
	return f.mean, f.n, f.err
}

func (f *fakeAcceptance) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// learnEnv is a live env with learning installed. A draw of 1 never holds out.
func learnEnv(t *testing.T, l Learning) *env {
	t.Helper()
	e := newEnv(t)
	e.prof.set(func(p *intent.Profile) { p.NotificationBudgetPerDay = 1000 })
	if l.Rand == nil {
		l.Rand = func() float64 { return 0.999999 }
	}
	e.g.SetLearning(l)
	return e
}

func lowSource() *fakeAcceptance { return &fakeAcceptance{mean: 0.05, n: 20} }

func TestLowAcceptanceToDigest_NoticeOnly(t *testing.T) {
	e := learnEnv(t, Learning{Enabled: true, Adapt: true, Source: lowSource()})

	// A Notice from a kind the owner finds useless goes to the digest.
	expect(t, e.decide(cand("k1", notice, withTrigger("allocation_drift:1"))), proactive.DispDigest, ReasonLowAcceptance)

	// Nothing else is touched: same posterior, other candidates.
	expect(t, e.decide(cand("k2", proactive.SeverityWarning)), proactive.DispNotify, ReasonTemplate)
	expect(t, e.decide(cand("k3", proactive.SeverityWarning, composeLLM)), proactive.DispEscalate, ReasonComposeLLM)
	expect(t, e.decide(cand("k4", proactive.SeverityCritical)), proactive.DispNotify, ReasonTemplate)
	expect(t, e.decide(cand("k5", proactive.SeverityCritical, protective)), proactive.DispNotify, ReasonProtective)
	expect(t, e.decide(cand("k6", notice, protective)), proactive.DispNotify, ReasonTemplate)
	expect(t, e.decide(cand("k7", notice, ownerRequested)), proactive.DispNotify, ReasonTemplate)
	expect(t, e.decide(cand("k8", proactive.SeverityInfo)), proactive.DispDigest, ReasonInfoToDigest)
	expect(t, e.decide(cand("k9", notice, func(c *proactive.Candidate) { c.Shadow = true })), proactive.DispLog, ReasonShadow)
	e.atNight()
	expect(t, e.decide(cand("k10", notice)), proactive.DispDigest, ReasonQuietHours)

	// An escalating Notice is demoted too: it is a would-be message.
	e.clock.Set(bkkAt(0, 12, 0))
	expect(t, e.decide(cand("k11", notice, composeLLM)), proactive.DispDigest, ReasonLowAcceptance)

	// The queue holds the demoted items, with their reason.
	if got := e.count(`SELECT COUNT(*) FROM attention_digest_queue q JOIN attention_decisions d ON d.id = q.decision_id WHERE d.reason = ?`, ReasonLowAcceptance); got != 2 {
		t.Errorf("%d low_acceptance items in the digest queue, want 2", got)
	}
}

func TestLowAcceptance_EvidenceFloorAndThreshold(t *testing.T) {
	src := &fakeAcceptance{mean: 0.01, n: MinEvidence - 1}
	e := learnEnv(t, Learning{Enabled: true, Adapt: true, Source: src})
	expect(t, e.decide(cand("a", notice)), proactive.DispNotify, ReasonTemplate) // 4 responses: too few

	src.mu.Lock()
	src.n = MinEvidence
	src.mu.Unlock()
	expect(t, e.decide(cand("b", notice)), proactive.DispDigest, ReasonLowAcceptance)

	src.mu.Lock()
	src.mean = LowAcceptanceThreshold // not below
	src.mu.Unlock()
	expect(t, e.decide(cand("c", notice)), proactive.DispNotify, ReasonTemplate)
}

func TestLowAcceptance_UnknownPosteriorIsThePrior(t *testing.T) {
	for name, l := range map[string]Learning{
		"no source": {Enabled: true, Adapt: true},
		"error":     {Enabled: true, Adapt: true, Source: &fakeAcceptance{mean: 0, n: 99, err: errors.New("boom")}},
		"garbage":   {Enabled: true, Adapt: true, Source: &fakeAcceptance{mean: math.NaN(), n: 99}},
		"negative":  {Enabled: true, Adapt: true, Source: &fakeAcceptance{mean: -1, n: 99}},
	} {
		t.Run(name, func(t *testing.T) {
			e := learnEnv(t, l)
			for _, k := range []string{"a", "b", "c"} {
				expect(t, e.decide(cand(k, notice)), proactive.DispNotify, ReasonTemplate)
			}
		})
	}
}

func TestLowAcceptance_ReadsTheKindOfTheTrigger(t *testing.T) {
	src := lowSource()
	e := learnEnv(t, Learning{Enabled: true, Adapt: true, Source: src})
	e.decide(cand("x", notice, withTrigger("Allocation_Drift:7")))
	e.decide(cand("y", proactive.SeverityWarning)) // not learnable: not asked
	if got := src.called(); !reflect.DeepEqual(got, []string{"allocation_drift"}) {
		t.Errorf("posterior asked for %v, want [allocation_drift]", got)
	}
	if KindOf("", "drawdown:portfolio:tier1") != "drawdown" || KindOf("heartbeat", "x:y") != "heartbeat" || KindOf("", "") != "" {
		t.Error("KindOf")
	}
	if HourBucket(bkkAt(0, 0, 0)) != 0 || HourBucket(bkkAt(0, 3, 59)) != 0 || HourBucket(bkkAt(0, 4, 0)) != 1 || HourBucket(bkkAt(0, 23, 59)) != 5 {
		t.Error("HourBucket")
	}
}

// With learning on and adapt off, delivery is untouched and what learning would have
// done is stored.
func TestAdaptOff_LoggedNotApplied(t *testing.T) {
	e := learnEnv(t, Learning{Enabled: true, Adapt: false, Source: lowSource(), HoldoutRate: 0.5, Rand: func() float64 { return 0 }})
	d := e.decide(cand("a", notice))
	expect(t, d, proactive.DispNotify, ReasonTemplate)
	r := e.lrow(d.ID)
	if r.learned != "digest" || r.learnedReason != ReasonLowAcceptance || r.holdout != 0 || r.propensity != 1 {
		t.Errorf("row = %+v; want learned digest/low_acceptance, holdout 0, propensity 1", r)
	}

	// Only the holdout would have applied: the draw is logged, the Notice is delivered.
	e2 := learnEnv(t, Learning{Enabled: true, Adapt: false, HoldoutRate: 0.5, Rand: func() float64 { return 0 }})
	d = e2.decide(cand("a", notice))
	expect(t, d, proactive.DispNotify, ReasonTemplate)
	if r := e2.lrow(d.ID); r.learned != "digest" || r.learnedReason != ReasonHoldout || r.holdout != 0 {
		t.Errorf("row = %+v; want learned digest/holdout, holdout 0", r)
	}

	// Nothing to log when learning would have decided the same.
	d = e2.decide(cand("w", proactive.SeverityWarning))
	if r := e2.lrow(d.ID); r.learned != "" || r.learnedReason != "" {
		t.Errorf("warning row = %+v; want no learned columns", r)
	}
	// With adapt on there is nothing to log: the decision is the learned one.
	e3 := learnEnv(t, Learning{Enabled: true, Adapt: true, Source: lowSource()})
	d = e3.decide(cand("a", notice))
	if r := e3.lrow(d.ID); r.learned != "" || r.holdout != 0 || r.propensity != 1 {
		t.Errorf("adapt row = %+v", r)
	}
}

// Learning off: every decision, and every row, is what it was before this change.
func TestLearningOff_ByteIdentical(t *testing.T) {
	scenario := func(g *Governor, e *env) [][2]string {
		var out [][2]string
		for i, c := range []proactive.Candidate{
			cand("a", notice), cand("b", proactive.SeverityWarning), cand("c", proactive.SeverityCritical, protective),
			cand("d", proactive.SeverityInfo), cand("e", notice, ownerRequested), cand("a", notice), cand("f", notice, composeLLM),
		} {
			d, err := g.Decide(context.Background(), c)
			if err != nil {
				t.Fatalf("%d: %v", i, err)
			}
			out = append(out, [2]string{string(d.Disposition), d.Reason})
		}
		return out
	}
	// Budget 3 (liveCfg): a, b and e are the three sends; the critical is not budgeted; a
	// again is an in-flight repeat; f is over the budget. These are the decisions the
	// table made before learning existed.
	golden := [][2]string{
		{"notify", ReasonTemplate}, {"notify", ReasonTemplate}, {"notify", ReasonProtective}, {"digest", ReasonInfoToDigest},
		{"notify", ReasonTemplate}, {"drop", ReasonDedupSame}, {"digest", ReasonBudget},
	}
	off := newEnv(t)
	if got := scenario(off.g, off); !reflect.DeepEqual(got, golden) {
		t.Fatalf("decisions = %v\nwant %v", got, golden)
	}
	// An explicitly disabled Learning, and one with every other field set, change nothing.
	for _, l := range []Learning{
		{}, {Enabled: false, Adapt: true, HoldoutRate: 1, Source: lowSource(), Rand: func() float64 { return 0 },
			Candidate: &config.CandidatePolicy{DailyBudget: ptr(0)}},
	} {
		e := newEnv(t)
		e.g.SetLearning(l)
		if got := scenario(e.g, e); !reflect.DeepEqual(got, golden) {
			t.Fatalf("with %+v: decisions = %v", l, got)
		}
		if n := e.count(`SELECT COUNT(*) FROM attention_decisions WHERE propensity IS NOT NULL OR holdout <> 0 OR shadow_disposition IS NOT NULL OR learned_disposition IS NOT NULL OR learned_reason IS NOT NULL`); n != 0 {
			t.Errorf("%d rows carry learning columns with learning off", n)
		}
	}
}

func ptr[T any](v T) *T { return &v }

// legacyEvaluate is the rule table as it was before rules 7b and 7c.
func legacyEvaluate(c proactive.Candidate, f facts) (proactive.Disposition, string) {
	switch {
	case f.Mode == modeOff:
		return proactive.DispDrop, ReasonOff
	case c.Shadow || f.Mode == modeShadow:
		return proactive.DispLog, ReasonShadow
	case f.Muted:
		return proactive.DispDrop, ReasonMuted
	case f.DedupHit:
		return proactive.DispDrop, ReasonDedupSame
	case f.QueuedHit && !isProtectiveCritical(c):
		return proactive.DispDrop, ReasonDedupQueued
	case isProtectiveCritical(c):
		if c.Compose == proactive.ComposeLLM {
			return proactive.DispEscalate, ReasonProtective
		}
		return proactive.DispNotify, ReasonProtective
	case f.InhibitedBy != "":
		return proactive.DispDrop, ReasonInhibitedPrefix + f.InhibitedBy
	case !f.InActiveHours && !c.OwnerRequested:
		return proactive.DispDigest, ReasonQuietHours
	case c.Severity == proactive.SeverityInfo:
		return proactive.DispDigest, ReasonInfoToDigest
	case c.Severity < proactive.SeverityCritical && f.SendsToday >= f.limitFor(c.Severity) && !f.JoinsPending:
		return proactive.DispDigest, ReasonBudget
	case c.Compose == proactive.ComposeLLM:
		return proactive.DispEscalate, deliverReason(c, f, ReasonComposeLLM)
	default:
		return proactive.DispNotify, deliverReason(c, f, ReasonTemplate)
	}
}

func randomCandidate(r *rand.Rand) proactive.Candidate {
	c := proactive.Candidate{DedupKey: "k", Severity: proactive.Severity(r.Intn(4))}
	c.Protective = r.Intn(3) == 0
	c.OwnerRequested = r.Intn(3) == 0
	c.Shadow = r.Intn(15) == 0
	if r.Intn(2) == 0 {
		c.Compose = proactive.ComposeLLM
	}
	return c
}

func randomFacts(r *rand.Rand) facts {
	f := facts{
		Mode:          []string{modeOff, modeShadow, modeLive, modeLive, modeLive}[r.Intn(5)],
		Muted:         r.Intn(12) == 0,
		DedupHit:      r.Intn(10) == 0,
		QueuedHit:     r.Intn(10) == 0,
		InActiveHours: r.Intn(4) != 0,
		SendsToday:    r.Intn(8),
		Budget:        r.Intn(6),
		JoinsPending:  r.Intn(5) == 0,
	}
	if r.Intn(10) == 0 {
		f.InhibitedBy = "dn:1"
	}
	return f
}

// Zero learning facts change nothing: the new table equals the old one on any facts.
func TestEvaluate_ZeroLearningFactsEqualTheOldTable(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	for i := 0; i < 50000; i++ {
		c, f := randomCandidate(r), randomFacts(r)
		d1, r1 := evaluate(c, f)
		d2, r2 := legacyEvaluate(c, f)
		if d1 != d2 || r1 != r2 {
			t.Fatalf("%+v %+v: %s/%s, old table %s/%s", c, f, d1, r1, d2, r2)
		}
		// Adapt off ignores every other learning fact, however extreme.
		f.Mean, f.Responses, f.LowThreshold, f.HoldoutRate, f.HoldoutDraw = 0, 1000, 1, 1, 0
		if d3, r3 := evaluate(c, f); d3 != d2 || r3 != r2 {
			t.Fatalf("adapt off but %+v changed %s/%s to %s/%s", f, d2, r2, d3, r3)
		}
	}
}

// TestLearning_NeverLooser is the safety property of learning: over generated candidates
// and facts, with posteriors anywhere in [0, 1] and the holdout forced either way, the
// decision with learning is the decision without it, or it is a digest in place of a
// notify or an escalate for a learnable Notice, with the reason of rule 7b or 7c. For
// every other candidate the two are identical, reason included.
func TestLearning_NeverLooser(t *testing.T) {
	r := rand.New(rand.NewSource(20261019))
	changed := 0
	for i := 0; i < 200000; i++ {
		c, f := randomCandidate(r), randomFacts(r)
		f.Adapt = false
		baseDisp, baseReason := evaluate(c, f)

		on := f
		on.Adapt = true
		on.Mean = r.Float64()
		if r.Intn(8) == 0 {
			on.Mean = []float64{0, 1, 0.2, 0.19999999}[r.Intn(4)]
		}
		on.Responses = r.Intn(12)
		on.LowThreshold = r.Float64() * 0.5
		on.HoldoutRate = r.Float64()
		switch r.Intn(3) {
		case 0:
			on.HoldoutRate, on.HoldoutDraw = 1, 0 // always held out
		case 1:
			on.HoldoutRate, on.HoldoutDraw = 0, 0 // never
		default:
			on.HoldoutDraw = r.Float64()
		}
		disp, reason := evaluate(c, on)

		if disp == baseDisp && reason == baseReason {
			continue
		}
		changed++
		eligible := c.Severity == proactive.SeverityNotice && !c.Protective && !c.OwnerRequested
		wasSend := baseDisp == proactive.DispNotify || baseDisp == proactive.DispEscalate
		if !eligible || !wasSend || disp != proactive.DispDigest || (reason != ReasonLowAcceptance && reason != ReasonHoldout) {
			t.Fatalf("learning changed %+v\nfacts %+v\nfrom %s/%s to %s/%s", c, on, baseDisp, baseReason, disp, reason)
		}
		// It never made anything MORE interruptive, on any scale.
		if rank(disp) > rank(baseDisp) {
			t.Fatalf("learning made %+v more interruptive: %s to %s", c, baseDisp, disp)
		}
	}
	if changed < 1000 {
		t.Errorf("only %d of the generated cases were changed: the property barely ran", changed)
	}
}

func rank(d proactive.Disposition) int {
	switch d {
	case proactive.DispNotify:
		return 3
	case proactive.DispEscalate:
		return 4
	case proactive.DispDigest:
		return 2
	case proactive.DispLog:
		return 1
	}
	return 0
}

// The rate and the propensities, at the level of the rules: N draws from a seeded source.
func TestHoldout_RateAndPropensityLogged(t *testing.T) {
	const (
		n    = 20000
		rate = 0.05
	)
	rng := SeededRand(42)
	c := cand("a", notice)
	held := 0
	for i := 0; i < n; i++ {
		f := facts{Mode: modeLive, InActiveHours: true, Budget: 1000, Adapt: true, Mean: 0.5, LowThreshold: LowAcceptanceThreshold,
			HoldoutRate: rate, HoldoutDraw: rng()}
		disp, reason := evaluate(c, f)
		switch reason {
		case ReasonHoldout:
			held++
			if disp != proactive.DispDigest || propensity(c, f, reason) != rate {
				t.Fatal("held out", disp, propensity(c, f, reason))
			}
		case ReasonTemplate:
			if got := propensity(c, f, reason); got != 1-rate {
				t.Fatalf("propensity of a kept Notice = %v, want %v", got, 1-rate)
			}
		default:
			t.Fatalf("reason %s", reason)
		}
	}
	// 5 sigma of a binomial.
	sigma := math.Sqrt(n * rate * (1 - rate))
	if d := math.Abs(float64(held) - n*rate); d > 5*sigma {
		t.Errorf("held out %d of %d, expected %v +- %v", held, n, n*rate, 5*sigma)
	}
	// An ineligible candidate has propensity 1 whatever the rate.
	f := facts{Adapt: true, HoldoutRate: rate, InActiveHours: true, Budget: 10, HoldoutDraw: 0}
	for _, w := range []proactive.Candidate{cand("w", proactive.SeverityWarning), cand("p", notice, protective), cand("o", notice, ownerRequested), cand("i", proactive.SeverityInfo)} {
		_, reason := evaluate(w, f)
		if got := propensity(w, f, reason); got != 1 {
			t.Errorf("%s propensity %v, want 1", w.DedupKey, got)
		}
	}

	// And the columns, through the governor and its database.
	e := learnEnv(t, Learning{Enabled: true, Adapt: true, HoldoutRate: 0.25, Rand: SeededRand(9)})
	heldRows, keptRows := 0, 0
	for i := 0; i < 400; i++ {
		d := e.decide(cand("key"+itoa(i), notice))
		r := e.lrow(d.ID)
		switch d.Reason {
		case ReasonHoldout:
			heldRows++
			if r.holdout != 1 || r.propensity != 0.25 || d.Disposition != proactive.DispDigest {
				t.Fatalf("holdout row = %+v", r)
			}
		case ReasonTemplate:
			keptRows++
			if r.holdout != 0 || r.propensity != 0.75 {
				t.Fatalf("kept row = %+v", r)
			}
			e.mark(d, true)
		default:
			t.Fatalf("reason %s", d.Reason)
		}
	}
	if heldRows < 60 || heldRows > 140 { // 100 +- ~5 sigma
		t.Errorf("held %d of 400 at rate 0.25", heldRows)
	}
}

func itoa(i int) string { return strconv.Itoa(i) }

func TestHoldout_NeverProtective(t *testing.T) {
	e := learnEnv(t, Learning{Enabled: true, Adapt: true, HoldoutRate: 1, Rand: func() float64 { return 0 }})
	for name, c := range map[string]proactive.Candidate{
		"warning":             cand("a", proactive.SeverityWarning),
		"critical":            cand("b", proactive.SeverityCritical),
		"protective critical": cand("c", proactive.SeverityCritical, protective),
		"protective warning":  cand("d", proactive.SeverityWarning, protective),
		"protective notice":   cand("e", notice, protective),
		"owner alert":         cand("f", notice, ownerRequested),
	} {
		d := e.decide(c)
		if d.Disposition != proactive.DispNotify && d.Disposition != proactive.DispEscalate {
			t.Errorf("%s: %s/%s", name, d.Disposition, d.Reason)
		}
		if r := e.lrow(d.ID); r.holdout != 0 || r.propensity != 1 {
			t.Errorf("%s: row %+v", name, r)
		}
	}
	d := e.decide(cand("g", notice))
	expect(t, d, proactive.DispDigest, ReasonHoldout)
	// Already-digest candidates keep their own reason and carry no holdout.
	d = e.decide(cand("h", proactive.SeverityInfo))
	expect(t, d, proactive.DispDigest, ReasonInfoToDigest)
	if r := e.lrow(d.ID); r.holdout != 0 || r.propensity != 1 {
		t.Errorf("info row %+v", r)
	}
	// Over budget the budget keeps the decision, and no draw is acted on.
	e.prof.set(func(p *intent.Profile) { p.NotificationBudgetPerDay = 0 })
	d = e.decide(cand("i", notice))
	expect(t, d, proactive.DispDigest, ReasonBudget)
	if r := e.lrow(d.ID); r.holdout != 0 || r.propensity != 1 {
		t.Errorf("budget row %+v", r)
	}
}

func TestShadowDispositionRecorded(t *testing.T) {
	cp := &config.CandidatePolicy{DailyBudget: ptr(0)}
	e := learnEnv(t, Learning{Enabled: true, Adapt: false, Candidate: cp})
	d := e.decide(cand("a", notice))
	expect(t, d, proactive.DispNotify, ReasonTemplate) // the live policy delivers
	if r := e.lrow(d.ID); r.shadow != "digest" {
		t.Errorf("shadow = %q, want digest (a budget of 0 would hold it)", r.shadow)
	}
	e.mark(d, true)
	if n := e.count(`SELECT COUNT(*) FROM attention_decisions WHERE disposition = 'notify' AND delivered = 1`); n != 1 {
		t.Errorf("delivered = %d: the candidate policy must not affect delivery", n)
	}

	// A threshold and a rate of the candidate are evaluated by the same rules.
	src := &fakeAcceptance{mean: 0.3, n: 10}
	cp2 := &config.CandidatePolicy{LowAcceptanceThreshold: ptr(0.4)}
	e2 := learnEnv(t, Learning{Enabled: true, Adapt: true, Source: src, Candidate: cp2})
	d = e2.decide(cand("a", notice))
	expect(t, d, proactive.DispNotify, ReasonTemplate) // 0.3 is not below the live 0.2
	if r := e2.lrow(d.ID); r.shadow != "digest" {
		t.Errorf("shadow = %q, want digest (0.3 < 0.4)", r.shadow)
	}
	cp3 := &config.CandidatePolicy{HoldoutRate: ptr(0.2)}
	e3 := learnEnv(t, Learning{Enabled: true, Adapt: true, Candidate: cp3, Rand: func() float64 { return 0.1 }})
	d = e3.decide(cand("a", notice))
	expect(t, d, proactive.DispNotify, ReasonTemplate)
	if r := e3.lrow(d.ID); r.shadow != "digest" {
		t.Errorf("shadow = %q, want digest (draw 0.1 < 0.2)", r.shadow)
	}
	// A Warning is the same under any candidate policy, and no candidate: no shadow.
	d = e3.decide(cand("w", proactive.SeverityWarning))
	if r := e3.lrow(d.ID); r.shadow != "notify" {
		t.Errorf("warning shadow = %q", r.shadow)
	}
	e4 := learnEnv(t, Learning{Enabled: true})
	if r := e4.lrow(e4.decide(cand("a", notice)).ID); r.shadow != "" {
		t.Errorf("shadow = %q without a candidate policy", r.shadow)
	}
}

// A database at schema v3 upgrades with its rows valid and the new columns empty.
func TestMigration_V4AddsLearningColumns(t *testing.T) {
	db := sqlitextest.Open(t)
	ctx := context.Background()
	if len(migrations) < 4 || migrations[3].Version != 4 {
		t.Fatalf("migrations = %+v", migrations)
	}
	if err := sqlitex.Migrate(ctx, db, migrations[:3]...); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO attention_decisions
		(id, at_ms, dedup_key, trigger_id, severity, disposition, reason, shadow, candidate_json)
		VALUES ('OLD', 1, 'k', 't', 1, 'notify', 'template', 0, '{}')`); err != nil {
		t.Fatal(err)
	}
	if err := sqlitex.Migrate(ctx, db, migrations...); err != nil {
		t.Fatal(err)
	}
	if n := countWhere(t, db, `id = 'OLD' AND propensity IS NULL AND holdout = 0 AND shadow_disposition IS NULL AND learned_disposition IS NULL AND learned_reason IS NULL`); n != 1 {
		t.Errorf("old row after v4: %d matches", n)
	}
	// Running the new code over it again is harmless.
	if err := sqlitex.Migrate(ctx, db, migrations...); err != nil {
		t.Fatal(err)
	}
}

func countWhere(t *testing.T, db *sql.DB, where string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM attention_decisions WHERE ` + where).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestDigest_NamesWhyLearningHeldAnItem(t *testing.T) {
	mk := func(reason string) Decision {
		return Decision{ID: "x" + reason, Reason: reason, Candidate: cand("k", notice), At: bkkAt(0, 12, 0)}
	}
	_, body := RenderDigest([]Decision{mk(ReasonLowAcceptance)})
	if want := "held for the digest: you found these less useful recently"; !strings.Contains(body, want) {
		t.Errorf("low_acceptance body %q lacks %q", body, want)
	}
	_, body = RenderDigest([]Decision{mk(ReasonHoldout)})
	if !strings.Contains(body, "held for the digest") {
		t.Errorf("holdout body %q", body)
	}
	_, body = RenderDigest([]Decision{mk(ReasonBudget)})
	if strings.Contains(body, "held for the digest") {
		t.Errorf("a budget item gets a learning note: %q", body)
	}
}

// lrow is a decision row's learning columns; "" for NULL.
type learnedRow struct {
	propensity                     float64
	holdout                        int
	shadow, learned, learnedReason string
}

func (e *env) lrow(id string) learnedRow {
	e.t.Helper()
	var (
		r          learnedRow
		p          sql.NullFloat64
		sh, ld, lr sql.NullString
	)
	if err := e.db.QueryRow(`SELECT propensity, holdout, shadow_disposition, learned_disposition, learned_reason
		FROM attention_decisions WHERE id = ?`, id).Scan(&p, &r.holdout, &sh, &ld, &lr); err != nil {
		e.t.Fatal(err)
	}
	r.propensity, r.shadow, r.learned, r.learnedReason = p.Float64, sh.String, ld.String, lr.String
	return r
}
