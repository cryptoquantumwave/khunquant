package attention

import (
	"context"
	crand "crypto/rand"
	"database/sql"
	"encoding/binary"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// Learning, in one place.
//
// proactive.learning.enabled records the owner's responses and keeps the acceptance
// posteriors (pkg/feedback). With it on and proactive.learning.adapt off, nothing about
// delivery changes: for each decision the governor only stores what learning WOULD have
// done (attention_decisions.learned_disposition and learned_reason), so the owner can
// see it before trusting it. Adapt is the only thing that lets learning change a
// disposition, through rules 7b (low_acceptance) and 7c (holdout) of the table in
// rules.go.
//
// What learning may touch is exactly learnable: a Notice that is neither protective nor
// an alert the owner set, and only while rules 0 to 7 and rule 8 have not already held
// it for the digest. It can only move such a candidate from notify or escalate to
// digest. It never changes a Warning, a Critical, a protective candidate, an owner's
// alert, or anything that is already digest, drop or log, and nothing it learns makes a
// message more interruptive. TestLearning_NeverLooser proves that over generated facts.
const (
	// LowAcceptanceThreshold is the posterior mean below which a Notice goes to the
	// digest.
	LowAcceptanceThreshold = 0.2
	// MinEvidence is the evidence floor: a posterior that rests on fewer recorded
	// responses than this never demotes, whatever its mean. (With the Beta(2, 2) prior,
	// seven plain rejections are needed to cross the threshold, so the floor does not
	// delay the first demotion of a user who rejects everything.)
	MinEvidence = 5
	// HourBuckets is the number of times of day a posterior is kept for.
	HourBuckets = 6
	// maxKindRunes bounds a trigger kind, which becomes part of a primary key.
	maxKindRunes = 64
)

// Queryer is the part of a database handle an AcceptanceSource reads through. A
// *sql.Tx satisfies it: the governor passes its own transaction, so the read never needs
// a second connection while the decision holds a write lock.
type Queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// AcceptanceSource gives the owner's acceptance posterior for a trigger kind in an hour
// bucket (see HourBucket) at time now: its mean and the number of responses it rests
// on. A kind it knows nothing about has the prior: mean 0.5 and no responses. pkg/feedback
// implements it; attention does not import that package.
type AcceptanceSource interface {
	Acceptance(ctx context.Context, q Queryer, kind string, bucket int, now time.Time) (mean float64, responses int, err error)
}

// Learning configures the governor's learning. The zero value, and a governor that is
// never given one, mean learning is off: every decision is what it was before.
type Learning struct {
	// Enabled: keep the evidence (read the posterior, draw the holdout) and log what
	// learning would do. It changes no delivery.
	Enabled bool
	// Adapt lets learning change dispositions. It has no effect without Enabled.
	Adapt bool
	// HoldoutRate is the probability that a Notice that would be delivered is held for
	// the digest, while Adapt is on.
	HoldoutRate float64
	// Candidate, when set, is evaluated beside the live policy; see shadowFacts.
	Candidate *config.CandidatePolicy
	// Source gives the posteriors; nil means every posterior is the prior.
	Source AcceptanceSource
	// Rand returns a uniform value in [0, 1) and must be safe for concurrent use. Nil
	// means no holdout is ever drawn.
	Rand func() float64
}

// HourBucket is the time-of-day bucket of t, read in t's own zone: 0 for 00:00 to
// 03:59, 1 for 04:00 to 07:59, and so on up to 5. The governor and the posterior store
// both use it, with the owner's time zone.
func HourBucket(t time.Time) int { return t.Hour() / (24 / HourBuckets) }

// KindOf is the trigger kind of a decision, by code: the part of the trigger ID before
// the first ":" ("allocation_drift:3" gives allocation_drift), or, when there is no
// trigger ID, the same part of the dedup key. It is lower-case and at most 64
// characters, and "" when neither names one.
func KindOf(triggerID, dedupKey string) string {
	s := strings.TrimSpace(triggerID)
	if s == "" {
		s = strings.TrimSpace(dedupKey)
	}
	k, _, _ := strings.Cut(s, ":")
	return strings.ToLower(clampRunes(strings.TrimSpace(k), maxKindRunes))
}

// SeededRand returns a goroutine-safe uniform [0, 1) source with a fixed seed, for
// tests.
func SeededRand(seed uint64) func() float64 {
	var mu sync.Mutex
	r := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	return func() float64 {
		mu.Lock()
		defer mu.Unlock()
		return r.Float64()
	}
}

// CryptoRand returns a goroutine-safe uniform [0, 1) source seeded from the operating
// system's random source, for production. If that source fails it falls back to the
// clock, which is enough for a 5% holdout and never blocks a decision.
func CryptoRand() func() float64 {
	var seed [16]byte
	var s1, s2 uint64
	if _, err := crand.Read(seed[:]); err == nil {
		s1, s2 = binary.LittleEndian.Uint64(seed[:8]), binary.LittleEndian.Uint64(seed[8:])
	} else {
		s1, s2 = uint64(time.Now().UnixNano()), 0x9e3779b97f4a7c15
	}
	var mu sync.Mutex
	r := rand.New(rand.NewPCG(s1, s2))
	return func() float64 {
		mu.Lock()
		defer mu.Unlock()
		return r.Float64()
	}
}

// SetLearning installs the learning settings. It may be called at any time, from any
// goroutine: the gateway builds attention before the feedback service that supplies the
// posteriors, and sets them when the service is up. A Learning with Enabled false turns
// learning off again.
func (g *Governor) SetLearning(l Learning) {
	if !l.Enabled {
		g.learning.Store(nil)
		return
	}
	g.learning.Store(&l)
}

// SetLearning passes the settings to the governor behind the outbox. It reports false
// when the outbox's gate is not a *Governor.
func (o *Outbox) SetLearning(l Learning) bool {
	gov, ok := o.g.(*Governor)
	if ok {
		gov.SetLearning(l)
	}
	return ok
}

// learnFacts fills the learning part of f for c. It reads the posterior through tx. An
// unknown posterior (no source, no rows, an error) is the prior, which never demotes;
// an error is logged once.
func (g *Governor) learnFacts(ctx context.Context, tx *sql.Tx, l *Learning, c proactive.Candidate, now time.Time, bucket int, f *facts) {
	f.Adapt = l.Adapt
	f.Mean, f.Responses = priorMean, 0
	f.LowThreshold = LowAcceptanceThreshold
	f.HoldoutRate = l.HoldoutRate
	f.HoldoutDraw = 1 // no draw: never held out
	if !learnable(c) {
		return
	}
	if l.Source != nil {
		mean, n, err := l.Source.Acceptance(ctx, tx, KindOf(c.TriggerID, c.DedupKey), bucket, now)
		switch {
		case err != nil:
			g.acceptanceWarn.Do(func() {
				logger.ErrorCF("attention", "Acceptance posterior unavailable; using the prior", map[string]any{"error": err.Error()})
			})
		case mean >= 0 && mean <= 1 && n >= 0:
			f.Mean, f.Responses = mean, n
		}
	}
	if l.Rand != nil {
		f.HoldoutDraw = l.Rand()
	}
}

// priorMean is the mean of the Beta(2, 2) prior, and what an unknown posterior is.
const priorMean = 0.5

// learnRow is what a decision stores about learning. Its zero value (learning off) is
// NULL, NULL, 0, NULL, NULL.
type learnRow struct {
	propensity    *float64
	holdout       int
	shadow        *string
	learned       *string
	learnedReason *string
}

// learnRecord evaluates the three views of one decision on the same facts f and the
// same holdout draw: the live one (d, already decided), what learning would have decided
// when it is not applied, and the candidate policy.
func learnRecord(l *Learning, c proactive.Candidate, f facts, d Decision) learnRow {
	p := propensity(c, f, d.Reason)
	row := learnRow{propensity: &p}
	if d.Reason == ReasonHoldout {
		row.holdout = 1
	}
	if !f.Adapt {
		lf := f
		lf.Adapt = true
		if disp, reason := evaluate(c, lf); disp != d.Disposition {
			s, r := string(disp), reason
			row.learned, row.learnedReason = &s, &r
		}
	}
	if l.Candidate != nil {
		disp, _ := evaluate(c, shadowFacts(f, l.Candidate))
		s := string(disp)
		row.shadow = &s
	}
	return row
}

// shadowFacts are f with the candidate policy's values in place of the live ones, and
// learning armed: the point of testing a threshold or a rate is to see what it would do.
// It is evaluated by the same evaluate and its result is only stored.
func shadowFacts(f facts, p *config.CandidatePolicy) facts {
	f.Adapt = true
	if p.DailyBudget != nil {
		f.Budget = *p.DailyBudget
	}
	if p.LowAcceptanceThreshold != nil {
		f.LowThreshold = *p.LowAcceptanceThreshold
	}
	if p.HoldoutRate != nil {
		f.HoldoutRate = *p.HoldoutRate
	}
	return f
}

// propensity is the probability the stored disposition had under the live policy:
// HoldoutRate for a holdout, 1-HoldoutRate for a learnable Notice that was exposed to
// the holdout and kept, and 1 for everything else (a deterministic rule decided it,
// including a low_acceptance demotion, and any decision made with adapt off, where no
// draw is acted on).
func propensity(c proactive.Candidate, f facts, reason string) float64 {
	switch {
	case reason == ReasonHoldout:
		return f.HoldoutRate
	case f.learns(c) && (reason == ReasonTemplate || reason == ReasonComposeLLM):
		return 1 - f.HoldoutRate
	}
	return 1
}
