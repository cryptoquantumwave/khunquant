package sources_test

import (
	"errors"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral/planhealth"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/sources"
)

// latestHealth decodes the health carried by the n-th event (1-based) the source emitted.
func latestHealth(t *testing.T, r *dnRig, n int) planhealth.Health {
	t.Helper()
	evs := r.out.events()
	if len(evs) < n {
		t.Fatalf("%d events, want at least %d", len(evs), n)
	}
	p, ok := evs[n-1].Payload.(sources.PlanHealthPayload)
	if !ok {
		t.Fatalf("payload = %T", evs[n-1].Payload)
	}
	h, err := planhealth.Decode(p.Health)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func silenceRig(t *testing.T) (*dnRig, func(d time.Duration)) {
	t.Helper()
	r := newDNRig(t, nil, dnPlan(7, "1m"))
	r.store.silenceNow = r.clock.Now
	r.checker.codes = []string{planhealth.CodeMarginCritical, planhealth.CodeLegMismatch, planhealth.CodeFundingNegative}
	return r, r.run(t)
}

func TestDNPlans_ReportCarriesTheCodesTheOwnerSilenced(t *testing.T) {
	r := newDNRig(t, nil, dnPlan(7, "1m"))
	r.store.silenceNow = r.clock.Now
	r.checker.codes = []string{planhealth.CodeMarginCritical, planhealth.CodeLegMismatch, planhealth.CodeFundingNegative}
	// Two codes are silenced; one of them is not in force, which the report ignores.
	r.store.silence(7, planhealth.CodeLegMismatch, epoch.Add(time.Hour))
	r.store.silence(7, planhealth.CodeFundingNegative, epoch.Add(time.Hour))
	r.store.silence(7, planhealth.CodeLiquidationDistanceLow, epoch.Add(time.Hour))
	r.run(t)
	waitFor(t, "the first event", func() bool { return len(r.out.events()) == 1 })

	h := latestHealth(t, r, 1)
	if len(h.BreachCodes) != 3 {
		t.Fatalf("breach codes = %v", h.BreachCodes)
	}
	want := map[string]bool{planhealth.CodeLegMismatch: true, planhealth.CodeFundingNegative: true}
	if len(h.SilencedCodes) != 2 || !want[h.SilencedCodes[0]] || !want[h.SilencedCodes[1]] {
		t.Fatalf("silenced = %v, want the two silenced codes in force", h.SilencedCodes)
	}
}

func TestDNPlans_ASilenceEndsWhenItsTimeIsUpAndTheNextReportDropsIt(t *testing.T) {
	r, advance := silenceRig(t)
	r.store.silence(7, planhealth.CodeLegMismatch, epoch.Add(90*time.Second))
	waitFor(t, "the first event", func() bool { return len(r.out.events()) == 1 })
	if h := latestHealth(t, r, 1); len(h.SilencedCodes) != 1 {
		t.Fatalf("silenced at the start = %v", h.SilencedCodes)
	}

	advance(time.Minute) // t+60s: still silenced
	waitFor(t, "the second event", func() bool { return len(r.out.events()) == 2 })
	if h := latestHealth(t, r, 2); len(h.SilencedCodes) != 1 {
		t.Fatalf("silenced before its end = %v", h.SilencedCodes)
	}

	advance(time.Minute) // t+120s: over, nothing restarts it
	waitFor(t, "the third event", func() bool { return len(r.out.events()) == 3 })
	if h := latestHealth(t, r, 3); len(h.SilencedCodes) != 0 {
		t.Fatalf("silenced after its end = %v", h.SilencedCodes)
	}
}

func TestDNPlans_ClearedSilencesTakeEffectAtTheNextCheck(t *testing.T) {
	r, advance := silenceRig(t)
	r.store.silence(7, planhealth.CodeMarginCritical, epoch.Add(24*time.Hour))
	waitFor(t, "the first event", func() bool { return len(r.out.events()) == 1 })
	if h := latestHealth(t, r, 1); len(h.SilencedCodes) != 1 {
		t.Fatalf("silenced = %v", h.SilencedCodes)
	}

	r.store.clearSilences(7)
	advance(time.Minute)
	waitFor(t, "the second event", func() bool { return len(r.out.events()) == 2 })
	if h := latestHealth(t, r, 2); len(h.SilencedCodes) != 0 {
		t.Fatalf("silenced after clear_silences = %v", h.SilencedCodes)
	}
}

func TestDNPlans_ASilenceOfACodeNotInBreachIsNotCarried(t *testing.T) {
	r, _ := silenceRig(t)
	r.store.silence(7, planhealth.CodeLiquidationDistanceLow, epoch.Add(time.Hour))
	waitFor(t, "the first event", func() bool { return len(r.out.events()) == 1 })
	if h := latestHealth(t, r, 1); len(h.SilencedCodes) != 0 {
		t.Fatalf("silenced = %v", h.SilencedCodes)
	}
}

func TestDNPlans_AnUnreadableSilenceTableSilencesNothingAndIsCounted(t *testing.T) {
	r := newDNRig(t, nil, dnPlan(7, "1m"))
	r.store.silenceErr = errors.New("database is locked")
	r.checker.codes = []string{planhealth.CodeMarginCritical}
	r.run(t)
	waitFor(t, "the first event", func() bool { return len(r.out.events()) == 1 })

	// Fail open: an owner who loses a silence hears an alert, not the reverse.
	if h := latestHealth(t, r, 1); len(h.SilencedCodes) != 0 || len(h.BreachCodes) != 1 {
		t.Fatalf("health = %+v", h)
	}
	if n := r.src.Stats().SilenceFailed; n != 1 {
		t.Fatalf("SilenceFailed = %d, want 1", n)
	}
}

func TestDNPlans_NoBreachMeansNoSilenceRead(t *testing.T) {
	r := newDNRig(t, nil, dnPlan(7, "1m"))
	r.run(t)
	waitFor(t, "the first event", func() bool { return len(r.out.events()) == 1 })
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	if r.store.silenceRd != 0 {
		t.Fatalf("%d silence reads for a healthy plan, want 0", r.store.silenceRd)
	}
}
